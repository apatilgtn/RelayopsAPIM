package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/relayops/apim/internal/apiops"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
	"testing/fstest"
)

func TestIntegrationAPIOpsEndpoints(t *testing.T) {
	st := openAdminTestStore(t)
	cp := newControlPlane(t, st, "cp-apiops")
	h := cp.Handler()
	platform := client{t: t, h: h, token: "test-admin-token"}

	// 1. Bundle Validation
	validBundle := apiops.CompiledBundle{
		FormatVersion: "1.0",
		SourceHash:    "sha256:test1234",
		RenderedHash:  "sha256:test5678",
		Config: apiops.DeclarativeConfig{
			FormatVersion: "1.0",
			APIs: []apiops.DeclAPI{
				{Name: "users-api", BasePath: "/users", UpstreamURL: "http://upstream-users:8080"},
			},
			Plans: []apiops.DeclPlan{
				{Name: "free", RateLimitPerMinute: 60},
			},
		},
	}
	validBytes, _ := json.Marshal(validBundle)
	validateRes := platform.must("POST", "/api/apiops/bundles/validate", string(validBytes), 200)
	if validateRes["valid"] != true {
		t.Fatalf("expected valid: true, got %v", validateRes)
	}

	// 2. Create and List Environments
	envReq := `{"slug":"staging-us","name":"Staging US","type":"staging"}`
	createdEnv := platform.must("POST", "/api/apiops/environments", envReq, 201)
	envID, ok := createdEnv["id"].(string)
	if !ok || envID == "" {
		t.Fatalf("expected environment id, got %v", createdEnv)
	}

	envs := platform.list("/api/apiops/environments")
	if len(envs) == 0 {
		t.Fatalf("expected at least 1 environment, got %d", len(envs))
	}

	// 3. Publish Candidate Canary Revision
	applyDoc := `{"format_version":"1.0","apis":[{"name":"users-api","base_path":"/users","upstream_url":"http://upstream-users:8080"}],"plans":[{"name":"free","rate_limit_per_minute":60}]}`
	applyRes := platform.must("POST", "/api/system/apply?rollout=canary&traffic_percent=10", applyDoc, 200)
	candRevFloat, ok := applyRes["revision"].(float64)
	if !ok || candRevFloat <= 0 {
		t.Fatalf("expected valid canary revision, got %v", applyRes)
	}
	candRev := int64(candRevFloat)

	// 4. Create Test Suite & Record Verified Test Run for the Canary
	suiteReq := `{
		"name": "Users Health Suite",
		"description": "Verify candidate users API",
		"definition": {
			"requests": [
				{
					"id": "req-1",
					"name": "Check Users",
					"method": "GET",
					"path": "/users",
					"assertions": [
						{"type": "status_code", "expected": "200"}
					]
				}
			]
		}
	}`
	createdSuite := platform.must("POST", "/api/tests/suites", suiteReq, 201)
	suiteMap := createdSuite["suite"].(map[string]any)
	suiteID := suiteMap["id"].(string)
	tenantID := suiteMap["tenant_id"].(string)

	now := time.Now().UTC()
	err := st.Pool.QueryRow(context.Background(), `
		INSERT INTO test_runs (suite_id, tenant_id, lifecycle_state, actual_revision, total_steps, passed_steps, failed_steps, skipped_steps, completed_at)
		VALUES ($1, $2, 'completed', $3, 2, 2, 0, 0, $4)
		RETURNING id`,
		suiteID, tenantID, candRev, now,
	).Scan(new(string))
	if err != nil {
		t.Fatalf("record test run in test fixture: %v", err)
	}

	// 5. Plan Deployment bound to the Candidate Canary Revision
	planReq := fmt.Sprintf(`{
		"environment_id": "%s",
		"candidate_revision": %d,
		"commit_sha": "a1b2c3d4e5f6",
		"repo_url": "https://github.com/example/api-catalog",
		"branch": "main",
		"plan_hash": "sha256:renderedhash123",
		"expected_base_revision": 0
	}`, envID, candRev)

	planned := platform.must("POST", "/api/apiops/deployments/plan", planReq, 201)
	depMap, ok := planned["deployment"].(map[string]any)
	if !ok {
		t.Fatalf("expected deployment in response, got %v", planned)
	}
	depID := depMap["id"].(string)

	// 6. Get and List Deployments
	getDep := platform.must("GET", "/api/apiops/deployments/"+depID, "", 200)
	if getDep["id"] != depID {
		t.Fatalf("mismatched deployment id %v vs %v", getDep["id"], depID)
	}

	deps := platform.list("/api/apiops/deployments")
	if len(deps) == 0 {
		t.Fatalf("expected deployments, got empty list")
	}

	// 7. Verify Deployment (Server collects test runs and calculates digest)
	verifyRes := platform.must("POST", "/api/apiops/deployments/"+depID+"/verify", "", 200)
	if verifyRes["status"] != "verifying" {
		t.Fatalf("expected verifying status, got %v", verifyRes)
	}
	digest, ok := verifyRes["evidence_digest"].(string)
	if !ok || digest == "" {
		t.Fatalf("expected non-empty server-derived evidence_digest, got %v", verifyRes)
	}

	// 8. Promote Deployment (Generates and seals Release Passport atomically)
	promoteReq := `{"override_reason":"pre-verified staging test suite passed"}`
	promoteRes := platform.must("POST", "/api/apiops/deployments/"+depID+"/promote", promoteReq, 200)
	if promoteRes["status"] != "promoted" {
		t.Fatalf("expected promoted status, got %v", promoteRes)
	}

	// 9. Get Release Passport
	passportRes := platform.must("GET", "/api/apiops/deployments/"+depID+"/passport", "", 200)
	if passportRes["deployment_id"] != depID {
		t.Fatalf("expected passport for deployment %s, got %v", depID, passportRes)
	}
	manifest, ok := passportRes["manifest"].(map[string]any)
	if !ok {
		t.Fatalf("expected manifest map, got %v", passportRes)
	}
	if manifest["commit_sha"] != "a1b2c3d4e5f6" {
		t.Fatalf("expected commit_sha to match, got %v", manifest["commit_sha"])
	}
	// The passport carries the server-collected verification evidence.
	if d, _ := manifest["verification_evidence_digest"].(string); d == "" {
		t.Fatalf("passport manifest has no verification evidence: %v", manifest)
	}
	if passportRes["created_at"] == nil || passportRes["created_at"] == "" {
		t.Fatalf("expected non-empty created_at in passport")
	}

	// 10. Abort Deployment on another deployment
	plan2Req := fmt.Sprintf(`{
		"environment_id": "%s",
		"candidate_revision": 0,
		"commit_sha": "e9f8d7c6b5a4",
		"branch": "feature/test",
		"expected_base_revision": 0
	}`, envID)
	planned2 := platform.must("POST", "/api/apiops/deployments/plan", plan2Req, 201)
	dep2ID := planned2["deployment"].(map[string]any)["id"].(string)
	abortRes := platform.must("POST", "/api/apiops/deployments/"+dep2ID+"/abort", "", 200)
	if abortRes["status"] != "aborted" {
		t.Fatalf("expected aborted status, got %v", abortRes)
	}
}

// Promotion and abort change a fleet-wide revision (every tenant's
// configuration), so tenant administrators must not reach them through
// APIOps; and promotion needs verification evidence for the candidate.
func TestIntegrationAPIOpsPromotionBoundaries(t *testing.T) {
	st := openAdminTestStore(t)
	h := newControlPlane(t, st, "cp-apiops-boundaries").Handler()
	platform := client{t: t, h: h, token: "test-admin-token"}

	platform.must("POST", "/api/tenants", `{"slug":"acme","name":"Acme"}`, 201)
	platform.must("POST", "/api/admin/users", `{"email":"alice@corp.example","name":"alice","role":"developer","password":"Passw0rd-alice-long"}`, 201)
	platform.must("POST", "/api/tenants/acme/members", `{"email":"alice@corp.example","role":"admin"}`, 201)
	alice := login(t, h, "alice@corp.example", "Passw0rd-alice-long").in("acme")

	// A canary candidate with no Test Studio runs against it.
	applyDoc := `{"format_version":"1.0","apis":[{"name":"orders","base_path":"/orders","upstream_url":"http://upstream-orders:8080"}]}`
	applyRes := platform.must("POST", "/api/system/apply?rollout=canary&traffic_percent=10", applyDoc, 200)
	candRev := int64(applyRes["revision"].(float64))

	env := alice.must("POST", "/api/apiops/environments", `{"slug":"acme-stage","name":"Acme Stage","type":"staging"}`, 201)
	planned := alice.must("POST", "/api/apiops/deployments/plan", fmt.Sprintf(`{"environment_id":"%s","commit_sha":"c0ffee1","branch":"main","expected_base_revision":0}`, env["id"]), 201)
	depID := planned["deployment"].(map[string]any)["id"].(string)

	for _, action := range []string{"promote", "abort"} {
		rec := alice.do("POST", "/api/apiops/deployments/"+depID+"/"+action, `{}`)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "platform_only") {
			t.Fatalf("tenant admin %s: %d %s (want 403 platform_only)", action, rec.Code, rec.Body)
		}
	}

	// Platform admin: no verification evidence for the candidate, no promotion.
	planned2 := platform.must("POST", "/api/apiops/deployments/plan", fmt.Sprintf(`{"environment_id":"%s","candidate_revision":%d,"commit_sha":"c0ffee2","branch":"main","expected_base_revision":0}`, env["id"], candRev), 201)
	dep2 := planned2["deployment"].(map[string]any)["id"].(string)
	rec := platform.do("POST", "/api/apiops/deployments/"+dep2+"/promote", `{}`)
	if rec.Code != 412 || !strings.Contains(rec.Body.String(), "no Test Studio runs") {
		t.Fatalf("promotion without evidence: %d %s (want 412)", rec.Code, rec.Body)
	}
	var status string
	_ = st.Pool.QueryRow(context.Background(), `SELECT status FROM config_revisions WHERE revision=$1`, candRev).Scan(&status)
	if status != "canary" {
		t.Fatalf("candidate revision status %q after refused promotion (want canary)", status)
	}
}

// Node credentials grant read access to every tenant's configuration, so only
// superadmins manage them; the runtime endpoint reports settings, no secrets.
func TestIntegrationNodeCredentialAdminAndRuntime(t *testing.T) {
	st := openAdminTestStore(t)
	cp := New(st, nil, realtime.NewHub(), "test-admin-token", "cp-rt", fstest.MapFS{},
		WithDataplane(DataplaneOptions{}), WithAutoRollbackInterval(15*time.Second),
		WithRuntimeInfo(RuntimeInfo{Role: "control-plane", LogSampleRate: 0.25}))
	h := cp.Handler()
	platform := client{t: t, h: h, token: "test-admin-token"}

	rt := platform.must("GET", "/api/system/runtime", "", 200)
	if rt["role"] != "control-plane" || rt["log_sample_rate"] != 0.25 || rt["auto_rollback_interval_seconds"] != 15.0 {
		t.Fatalf("runtime: %v", rt)
	}
	if na, _ := rt["node_api"].(map[string]any); na["enabled"] != true || na["shared_token"] != false {
		t.Fatalf("runtime node_api: %v", rt["node_api"])
	}

	created := platform.must("POST", "/api/admin/dataplane/credentials", `{"node_id":"edge-1","expires_in_days":30}`, 201)
	if tok, _ := created["token"].(string); !strings.HasPrefix(tok, store.DataplaneTokenPrefix) {
		t.Fatalf("issued: %v", created)
	}
	if list := platform.list("/api/admin/dataplane/credentials"); len(list) != 1 {
		t.Fatalf("credentials listed: %d", len(list))
	}

	platform.must("POST", "/api/admin/users", `{"email":"ops@corp.example","name":"ops","role":"admin","password":"Passw0rd-ops-long"}`, 201)
	ops := login(t, h, "ops@corp.example", "Passw0rd-ops-long")
	if rec := ops.do("POST", "/api/admin/dataplane/credentials", `{"node_id":"edge-2"}`); rec.Code != 403 {
		t.Fatalf("non-superadmin issued a node credential: %d", rec.Code)
	}
	if rec := ops.do("GET", "/api/admin/dataplane/credentials", ""); rec.Code != 403 {
		t.Fatalf("non-superadmin listed node credentials: %d", rec.Code)
	}
}

func TestSuperadminGateOverrideBoundary(t *testing.T) {
	st := openAdminTestStore(t)
	cp := newControlPlane(t, st, "cp-boundary")
	h := cp.Handler()
	platform := client{t: t, h: h, token: "test-admin-token"}

	// Onboard a regular operator user
	platform.must("POST", "/api/admin/users", `{"email":"operator1@corp.example","name":"Operator 1","role":"operator","password":"Passw0rd-operator-long"}`, 201)
	opClient := login(t, h, "operator1@corp.example", "Passw0rd-operator-long")

	// Create environment
	env := platform.must("POST", "/api/apiops/environments", `{"slug":"prod-eu","name":"Prod EU","type":"prod"}`, 201)
	envID := env["id"].(string)

	// Create deployment
	planned := platform.must("POST", "/api/apiops/deployments/plan", fmt.Sprintf(`{
		"environment_id": "%s",
		"commit_sha": "abc1234",
		"branch": "main",
		"expected_base_revision": 0
	}`, envID), 201)
	depID := planned["deployment"].(map[string]any)["id"].(string)

	// Operator attempts to promote with gate override -> MUST be rejected with 403 Forbidden!
	rec := opClient.do("POST", "/api/apiops/deployments/"+depID+"/promote", `{"override_reason":"operator trying to bypass gate"}`)
	if rec.Code != 403 {
		t.Fatalf("expected 403 Forbidden when non-superadmin supplies override reason, got %d: %s", rec.Code, rec.Body.String())
	}
}
