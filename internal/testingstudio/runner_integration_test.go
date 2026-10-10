package testingstudio

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/testdb"
)

func TestIntegrationStudioLiveRunner(t *testing.T) {
	for _, scenario := range []string{"comparison-pass", "candidate-fails", "behavior-changes", "missing-revision", "wrong-revision", "untrusted-destination", "header-regression", "latency-regression"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, testdb.New(t))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			api, base, err := db.CreateAPIAtomic(ctx, store.API{Name: "live", BasePath: "/live", UpstreamURL: "http://127.0.0.1:7070", AuthType: "none", Enabled: true, TimeoutMS: 1000}, "test", "base")
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := db.AtomicPublishCanary(ctx, "test", "candidate", store.CanarySplit{TrafficPercent: 10}, nil)
			if err != nil {
				t.Fatal(err)
			}
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				isCandidate := r.Header.Get("X-RelayOps-Target-Cohort") == "canary"
				if scenario != "untrusted-destination" && r.Header.Get("X-RelayOps-Runner-Token") != "test-runner-secret" {
					t.Errorf("runner token missing")
				}
				rev := base
				if isCandidate {
					rev = candidate
				}
				if scenario == "wrong-revision" && isCandidate {
					rev++
				}
				if scenario != "missing-revision" {
					w.Header().Set("X-RelayOps-Revision", strconv.FormatInt(rev, 10))
				}
				w.Header().Set("Content-Type", "application/json")
				if scenario == "header-regression" && isCandidate {
					w.Header().Set("Content-Type", "text/plain")
				}
				if scenario == "latency-regression" {
					if isCandidate {
						time.Sleep(250 * time.Millisecond)
					} else {
						time.Sleep(10 * time.Millisecond)
					}
				}

				if scenario == "candidate-fails" && isCandidate {
					w.WriteHeader(500)
				}
				if scenario == "behavior-changes" && isCandidate {
					fmt.Fprint(w, `{"result":"different"}`)
				} else {
					fmt.Fprint(w, `{"result":"ok"}`)
				}
			}))
			defer service.Close()
			env, err := db.CreateTestEnvironment(ctx, store.TestEnvironment{Name: "live", GatewayTarget: service.URL, Variables: map[string]string{}, CredentialBindings: map[string]string{}})
			if err != nil {
				t.Fatal(err)
			}
			def := store.SuiteDefinition{Name: "live", Requests: []store.RequestDef{{Name: "health", Method: "GET", Path: "/live", Assertions: []store.AssertionDef{{Type: "status_code", Expected: "200"}}}}}
			if scenario == "latency-regression" {
				def.Comparison = store.ComparisonPolicy{MaxLatencyIncreasePercent: 100, MinLatencyIncreaseMS: 100}
			}
			suite, ver, err := db.CreateTestSuite(ctx, store.TestSuite{Name: "live", APIID: &api.ID}, def, "test")
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.UpsertTestGatePolicy(ctx, store.TestGatePolicy{APIID: &api.ID, RequiredSuiteIDs: []string{suite.ID}, FreshnessSeconds: 3600, EnforcementEnabled: true})
			if err != nil {
				t.Fatal(err)
			}
			run, err := db.CreateTestRun(ctx, store.TestRun{SuiteID: suite.ID, SuiteVersionID: &ver.ID, SuiteContentHash: ver.ContentHash, EnvironmentID: &env.ID, Mode: "comparison", LifecycleState: "queued", TotalSteps: 1, ImmutableInputs: map[string]any{"baseline_revision": base, "candidate_revision": candidate, "environment_snapshot": map[string]any{"id": env.ID, "revision": env.Revision, "gateway_target": env.GatewayTarget, "variables": env.Variables, "credential_bindings": env.CredentialBindings, "credential_versions": map[string]any{}}}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.EnqueueTestJob(ctx, run.ID, store.DefaultTenantID)
			if err != nil {
				t.Fatal(err)
			}
			var trusted []string
			if scenario != "untrusted-destination" {
				trusted = []string{service.URL}
			}
			runner := NewRunner(db, RunnerConfig{PollInterval: 10 * time.Millisecond, RunnerToken: "test-runner-secret", TrustedGateways: trusted})
			runner.Start()
			defer runner.Stop()
			deadline := time.Now().Add(5 * time.Second)
			var final store.TestRun
			for time.Now().Before(deadline) {
				final, err = db.GetTestRun(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if final.LifecycleState == "completed" || final.LifecycleState == "failed" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			wantPass := scenario == "comparison-pass"
			if (wantPass && final.LifecycleState != "completed") || (!wantPass && final.LifecycleState != "failed") {
				t.Fatalf("state=%s", final.LifecycleState)
			}
			if (final.FailedSteps == 0) != wantPass {
				t.Fatalf("failed=%d", final.FailedSteps)
			}
			steps, err := db.ListTestRunSteps(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "untrusted-destination" && len(steps) != 2 {
				t.Fatalf("stored %d results; need both cohorts", len(steps))
			}
			// Wait for evidence writing after terminal run update.
			for time.Now().Before(deadline) {
				var count int
				_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM test_gate_evidence WHERE run_id=$1`, run.ID).Scan(&count)
				if count > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			_, err = db.PromoteCanaryWithTestGates(ctx, candidate, false)
			if wantPass && err != nil {
				t.Fatal(err)
			}
			if !wantPass && err == nil {
				t.Fatal("failed comparison allowed promotion")
			}
		})
	}
}
