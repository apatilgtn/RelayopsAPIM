package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIntegrationStudioGateBindings(t *testing.T) {
	for _, scenario := range []string{"passing", "missing-suite", "expired", "changed-suite", "changed-environment", "rotated-credential", "changed-ignore-rules", "latest-failed", "old-comparison-engine", "standard-with-comparison-policy", "override"} {
		t.Run(scenario, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			api, _, err := s.CreateAPIAtomic(ctx, mkAPI("studio", "/studio"), "test", "baseline")
			if err != nil {
				t.Fatal(err)
			}
			rev, err := s.AtomicPublishCanary(ctx, "test", "candidate", CanarySplit{TrafficPercent: 10}, nil)
			if err != nil {
				t.Fatal(err)
			}
			env, err := s.CreateTestEnvironment(ctx, TestEnvironment{Name: "test", GatewayTarget: "http://127.0.0.1:8080", Variables: map[string]string{}, CredentialBindings: map[string]string{"key": "cred"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Pool.Exec(ctx, `INSERT INTO test_credentials(tenant_id,name,provider_ref) VALUES($1,'cred','vault-ref')`, DefaultTenantID)
			if err != nil {
				t.Fatal(err)
			}
			versions, err := s.TestCredentialVersions(ctx, env)
			if err != nil {
				t.Fatal(err)
			}
			var suiteIDs []string
			var lastRun TestRun
			for _, name := range []string{"first", "second"} {
				def := SuiteDefinition{Name: name, Requests: []RequestDef{{ID: "one", Name: "GET", Method: "GET", Path: "/studio", Assertions: []AssertionDef{{Type: "status_code", Expected: "200"}}}}}
				if scenario == "standard-with-comparison-policy" {
					def.Comparison.Headers = []string{"content-type"}
				}
				suite, ver, err := s.CreateTestSuite(ctx, TestSuite{Name: name, APIID: &api.ID, Ownership: "team", Visibility: "tenant"}, def, "test")
				if err != nil {
					t.Fatal(err)
				}
				suiteIDs = append(suiteIDs, suite.ID)
				if scenario == "missing-suite" && name == "second" {
					continue
				}
				run, err := s.CreateTestRun(ctx, TestRun{SuiteID: suite.ID, SuiteVersionID: &ver.ID, SuiteContentHash: ver.ContentHash, EnvironmentID: &env.ID, Mode: "comparison", LifecycleState: "queued", TotalSteps: 1, ImmutableInputs: map[string]any{"environment_snapshot": map[string]any{"id": env.ID, "revision": env.Revision, "gateway_target": env.GatewayTarget, "variables": env.Variables, "credential_bindings": env.CredentialBindings, "credential_versions": versions}}})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.UpdateTestRunProgress(ctx, run.ID, "completed", 1, 0, 0, "", rev-1, rev, map[string]any{"engine_version": ComparisonEngineVersion}); err != nil {
					t.Fatal(err)
				}
				if err := s.RecordTestGateEvidence(ctx, TestGateEvidence{APIID: &api.ID, Revision: rev, RunID: &run.ID, TargetFingerprint: ver.ContentHash, Eligible: true}); err != nil {
					t.Fatal(err)
				}
				lastRun = run
			}
			_, err = s.UpsertTestGatePolicy(ctx, TestGatePolicy{APIID: &api.ID, RequiredSuiteIDs: suiteIDs, FreshnessSeconds: 3600, EnforcementEnabled: true})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "standard-with-comparison-policy":
				_, err = s.Pool.Exec(ctx, `UPDATE test_runs SET mode='standard',actual_revision=$1`, rev)
			case "old-comparison-engine":
				_, err = s.Pool.Exec(ctx, `UPDATE test_runs SET comparison_summary='{}'::jsonb`)
			case "expired":
				_, err = s.Pool.Exec(ctx, `UPDATE test_runs SET completed_at=now()-interval '2 hours'`)
			case "changed-suite":
				_, err = s.Pool.Exec(ctx, `UPDATE test_suite_versions SET content_hash='changed' WHERE suite_id=$1`, suiteIDs[0])
			case "changed-environment":
				env.Variables = map[string]string{"changed": "true"}
				_, err = s.UpdateTestEnvironment(ctx, env)
			case "rotated-credential":
				_, err = s.Pool.Exec(ctx, `UPDATE test_credentials SET credential_version=credential_version+1`)
			case "changed-ignore-rules":
				_, err = s.Pool.Exec(ctx, `UPDATE test_runs SET immutable_inputs=immutable_inputs || '{"ignore_paths":["/result"]}'::jsonb`)
			case "latest-failed", "override":
				err = s.RecordTestGateEvidence(ctx, TestGateEvidence{APIID: &api.ID, Revision: rev, RunID: &lastRun.ID, TargetFingerprint: lastRun.SuiteContentHash, Eligible: false})
			}
			if err != nil {
				t.Fatal(err)
			}
			reasons, err := s.PromoteCanaryWithTestGates(ctx, rev, scenario == "override")
			wantPass := scenario == "passing" || scenario == "override"
			if wantPass && err != nil {
				t.Fatal(err)
			}
			if !wantPass {
				var blocked *GateRejectedError
				if !errors.As(err, &blocked) {
					t.Fatalf("expected gate rejection, got %v", err)
				}
			}
			if scenario == "override" && len(reasons) == 0 {
				t.Fatal("override lost reasons")
			}
			var status string
			_ = s.Pool.QueryRow(ctx, `SELECT status FROM config_revisions WHERE revision=$1`, rev).Scan(&status)
			if (status == "active") != wantPass {
				t.Fatalf("revision status=%s", status)
			}
		})
	}
}

func TestIntegrationStudioCohortUpgradeAndInterruptedLease(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// Simulate the already-installed original 012 shape, then apply the upgrade.
	_, err := s.Pool.Exec(ctx, `ALTER TABLE test_run_steps DROP CONSTRAINT test_run_steps_run_step_cohort_key; ALTER TABLE test_run_steps DROP COLUMN cohort; ALTER TABLE test_run_steps ADD CONSTRAINT test_run_steps_run_step_key UNIQUE(run_id,step_index); DELETE FROM schema_migrations WHERE version='013_test_studio_cohort'`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suite, ver, err := s.CreateTestSuite(ctx, TestSuite{Name: "cohorts"}, SuiteDefinition{Name: "cohorts"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateTestRun(ctx, TestRun{SuiteID: suite.ID, SuiteVersionID: &ver.ID, LifecycleState: "queued", TotalSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, cohort := range []string{"baseline", "candidate"} {
		if _, err := s.RecordTestRunStep(ctx, TestRunStep{RunID: run.ID, StepIndex: 1, Cohort: cohort, Method: "GET", URL: "http://localhost"}); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := s.ListTestRunSteps(ctx, run.ID)
	if err != nil || len(steps) != 2 {
		t.Fatalf("steps=%d err=%v", len(steps), err)
	}
	_, err = s.EnqueueTestJob(ctx, run.ID, DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimTestJob(ctx, "worker", time.Second)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.UpdateTestRunProgress(ctx, run.ID, "running", 0, 0, 0, "", 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	_, err = s.Pool.Exec(ctx, `UPDATE test_jobs SET claim_lease_until=now()-interval '1 second' WHERE id=$1`, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ClaimTestJob(ctx, "other", time.Second)
	if err != nil || reclaimed != nil {
		t.Fatalf("unsafe replay: %+v %v", reclaimed, err)
	}
	updated, err := s.GetTestRun(ctx, run.ID)
	if err != nil || updated.LifecycleState != "interrupted" {
		t.Fatalf("state=%s err=%v", updated.LifecycleState, err)
	}
}
