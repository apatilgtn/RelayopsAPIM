package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relayops/apim/internal/store"
)

func seedReleaseEvidence(t *testing.T, st *store.Store, tenant, api, suite string, rev int64) {
	t.Helper()
	ctx := context.Background()
	env, err := st.CreateTestEnvironment(ctx, store.TestEnvironment{TenantID: tenant, Name: "release", GatewayTarget: "http://localhost", Variables: map[string]string{}, CredentialBindings: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	ver, err := st.GetLatestTestSuiteVersion(ctx, suite)
	if err != nil {
		t.Fatal(err)
	}
	versions, err := st.TestCredentialVersions(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateTestRun(ctx, store.TestRun{TenantID: tenant, SuiteID: suite, SuiteVersionID: &ver.ID, SuiteContentHash: ver.ContentHash, EnvironmentID: &env.ID, Mode: "standard", LifecycleState: "queued", TotalSteps: 1, ActualRevision: rev, ImmutableInputs: map[string]any{"environment_snapshot": map[string]any{"revision": env.Revision, "gateway_target": env.GatewayTarget, "variables": env.Variables, "credential_bindings": env.CredentialBindings, "credential_versions": versions}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.UpdateTestRunProgress(ctx, run.ID, "completed", 1, 0, 0, "", rev, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err = st.RecordTestGateEvidence(ctx, store.TestGateEvidence{TenantID: tenant, APIID: &api, Revision: rev, RunID: &run.ID, TargetFingerprint: ver.ContentHash, Eligible: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpsertTestGatePolicy(ctx, store.TestGatePolicy{TenantID: tenant, APIID: &api, RequiredSuiteIDs: []string{suite}, FreshnessSeconds: 3600, EnforcementEnabled: true}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationGuidedDraftIsolationAndAbort(t *testing.T) {
	st := openAdminTestStore(t)
	cp := newControlPlane(t, st, "guided")
	c := client{t: t, h: cp.Handler(), token: "test-admin-token"}
	config := `{"format_version":"1.0","apis":[{"name":"guided","base_path":"/guided","upstream_url":"http://upstream","auth_type":"api_key","rate_limit_per_minute":60}],"plans":[]}`
	raw, _ := json.Marshal(map[string]any{"configuration": json.RawMessage(config)})
	d := c.must("POST", "/api/release-drafts", string(raw), 200)
	id := d["id"].(string)
	apis, err := st.ListAPIs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range apis {
		if a.Name == "guided" {
			t.Fatal("draft changed live config")
		}
	}
	cfgRaw, _ := json.Marshal(map[string]any{"version": 1, "configuration": json.RawMessage(config)})
	c.must("PUT", "/api/release-drafts/"+id, string(cfgRaw), 200)
	c.must("PUT", "/api/release-drafts/"+id, string(cfgRaw), 409)
	env := c.must("POST", "/api/tests/environments", `{"name":"guided","gateway_target":"","variables":{},"credential_bindings":{}}`, 201)
	suite := c.must("POST", "/api/tests/suites", `{"name":"guided","definition":{"name":"guided","requests":[{"id":"get","name":"get","method":"GET","path":"/guided","assertions":[{"type":"status_code","expected":"200"}]}]}}`, 201)["suite"].(map[string]any)
	cfgRaw, _ = json.Marshal(map[string]any{"version": 2, "configuration": json.RawMessage(config), "suite_ids": []string{suite["id"].(string)}, "environment_id": env["id"]})
	c.must("PUT", "/api/release-drafts/"+id, string(cfgRaw), 200)
	plan := c.must("POST", "/api/release-drafts/"+id+"/plan", `{}`, 200)["plan"].(map[string]any)
	// Record actual completed baseline outcomes; passing candidate evidence remains separate.
	ver, err := st.GetLatestTestSuiteVersion(context.Background(), suite["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	environment, err := st.GetTestEnvironment(context.Background(), env["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	base, _, err := st.GetRolloutState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := st.CreateTestRun(context.Background(), store.TestRun{SuiteID: suite["id"].(string), SuiteContentHash: ver.ContentHash, EnvironmentID: &environment.ID, ActualRevision: base, TotalSteps: 1, LifecycleState: "queued", Mode: "standard", ImmutableInputs: map[string]any{"environment_snapshot": map[string]any{"revision": environment.Revision}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.UpdateTestRunProgress(context.Background(), baseline.ID, "completed", 0, 1, 0, "new API absent on baseline", base, 0, nil); err != nil {
		t.Fatal(err)
	}
	launch := fmt.Sprintf(`{"version":3,"plan_hash":%q,"traffic_percent":10}`, plan["plan_hash"])
	first := c.must("POST", "/api/release-drafts/"+id+"/canary", launch, 200)
	second := c.must("POST", "/api/release-drafts/"+id+"/canary", launch, 200)
	if first["revision"] != second["revision"] {
		t.Fatal("duplicate canary")
	}
	j := c.must("GET", "/api/release-drafts/"+id+"/journey", "", 200)
	if j["can_promote"] != false {
		t.Fatal("missing evidence appeared eligible")
	}
	c.must("POST", "/api/release-drafts/"+id+"/decide/promote", `{"reviewed":false}`, 412)
	c.must("POST", "/api/release-drafts/"+id+"/decide/abort", `{"reviewed":true}`, 200)
	c.must("POST", "/api/release-drafts/"+id+"/decide/abort", `{"reviewed":true}`, 200)
	export := c.must("GET", "/api/release-drafts/"+id+"/evidence", "", 200)
	encoded, _ := json.Marshal(export)
	if strings.Contains(string(encoded), "upstream_url") {
		t.Fatal("export exposed configuration")
	}
}

func TestDraftRejectsInlineSecrets(t *testing.T) {
	for _, upstream := range []string{"https://user:password@example.test", "https://example.test?api_key=plaintext"} {
		if draftSecretReferences(DeclarativeConfig{APIs: []DeclAPI{{UpstreamURL: upstream}}}) == nil {
			t.Fatal("accepted inline upstream credentials")
		}
	}
	if !secretReference("Bearer ${secret:UPSTREAM_TOKEN}") || secretReference("${secret:token}plaintext}") {
		t.Fatal("invalid secret reference validation")
	}
	for _, a := range []DeclAPI{{JWTSecret: "plaintext"}, {RequestHeaders: map[string]string{"Authorization": "Bearer plaintext"}}, {OpenAPISpec: map[string]any{"password": "plaintext"}}} {
		if draftSecretReferences(DeclarativeConfig{APIs: []DeclAPI{a}}) == nil {
			t.Fatal("inline secret accepted")
		}
	}
	if err := draftSecretReferences(DeclarativeConfig{APIs: []DeclAPI{{JWTSecret: "${secret:jwt}"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationConcurrentPromotionSealsOneDecision(t *testing.T) {
	st := openAdminTestStore(t)
	ctx := context.Background()
	a, _, err := st.CreateAPIAtomic(ctx, store.API{Name: "seal", BasePath: "/seal", UpstreamURL: "http://upstream", AuthType: "none", Enabled: true, TimeoutMS: 1000}, "test", "baseline")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := st.AtomicPublishCanary(ctx, "test", "candidate", store.CanarySplit{TrafficPercent: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	suite, _, err := st.CreateTestSuite(ctx, store.TestSuite{APIID: &a.ID, Name: "seal"}, store.SuiteDefinition{Name: "seal", Requests: []store.RequestDef{{ID: "get", Name: "get", Method: "GET", Path: "/seal"}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	seedReleaseEvidence(t, st, a.TenantID, a.ID, suite.ID, rev)
	dep, err := st.CreateAPIOpsDeployment(ctx, store.APIOpsDeployment{TenantID: a.TenantID, CandidateRevision: rev})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	ids := make(chan string, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, _, e := st.PromoteCanaryAndSealPassport(ctx, dep.ID, rev, false, store.APIOpsReleasePassport{DeploymentID: dep.ID, TenantID: dep.TenantID, Manifest: map[string]any{"promoted_by": "test"}})
			ids <- p.ID
			errs <- e
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first || id == "" {
			t.Fatal("concurrent requests sealed different passports")
		}
	}
	var n int
	if err = st.Pool.QueryRow(ctx, `SELECT count(*) FROM release_decisions WHERE revision=$1`, rev).Scan(&n); err != nil || n != 1 {
		t.Fatalf("decisions=%d err=%v", n, err)
	}
	cp := newControlPlane(t, st, "seal")
	c := client{t: t, h: cp.Handler(), token: "test-admin-token"}
	result := c.must("GET", fmt.Sprintf("/api/revisions/%d/evidence", rev), "", 200)
	if result["gate_report"].(map[string]any)["eligible"] != true {
		t.Fatal("export not bound to passing decision")
	}
}

func TestIntegrationAuditRetentionRequiresArchive(t *testing.T) {
	st := openAdminTestStore(t)
	c := client{t: t, h: newControlPlane(t, st, "audit").Handler(), token: "test-admin-token"}
	if _, err := st.Pool.Exec(context.Background(), `INSERT INTO audit_logs(ts,actor,action,resource_type,resource_id,tenant_id) VALUES(now()-interval '60 days','test','TEST','api','test',$1)`, store.DefaultTenantID); err != nil {
		t.Fatal(err)
	}
	archive := c.must("GET", "/api/audit/archives/export?before="+time.Now().Add(-30*24*time.Hour).UTC().Format(time.RFC3339), "", 200)
	path := "/api/audit/archives/" + archive["archive_id"].(string) + "/purge"
	body := fmt.Sprintf(`{"archived":true,"digest":%q}`, archive["digest"])
	t.Setenv("RELAYOPS_AUDIT_RETENTION_DAYS", "")
	c.must("POST", path, body, 412)
	t.Setenv("RELAYOPS_AUDIT_RETENTION_DAYS", "30")
	c.must("POST", path, `{"archived":true,"digest":"wrong"}`, 412)
	if result := c.must("POST", path, body, 200); result["deleted"].(float64) != 1 {
		t.Fatalf("purge=%v", result)
	}
	if result := c.must("POST", path, body, 200); result["deleted"].(float64) != 0 {
		t.Fatal("purge retry deleted more records")
	}
}
