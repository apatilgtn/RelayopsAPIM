package admin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/relayops/apim/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

type pgxTx = pgx.Tx

func openAdminTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func newControlPlane(t *testing.T, s *store.Store, node string) *Server {
	t.Helper()
	gw := gateway.New(analytics.NewCollector(nil, realtime.NewHub(), node), nil, node)
	return New(s, gw, realtime.NewHub(), "test-admin-token", node, fstest.MapFS{})
}

func seedBadRelease(t *testing.T, s *store.Store) (good, bad int64) {
	t.Helper()
	ctx := context.Background()
	_, good, err := s.CreateAPIAtomic(ctx, store.API{Name: "svc", BasePath: "/svc", UpstreamURL: "http://good", StripPath: true,
		AuthType: "none", TimeoutMS: 1000, Enabled: true}, "t", "good")
	if err != nil {
		t.Fatal(err)
	}
	api, _ := s.ListAPIs(ctx)
	api[len(api)-1].UpstreamURL = "http://bad"
	_, bad, err = s.UpdateAPIAtomic(ctx, api[len(api)-1], "t", "bad")
	if err != nil {
		t.Fatal(err)
	}
	var logs []store.RequestLog
	for i := 0; i < 20; i++ {
		logs = append(logs, store.RequestLog{TS: time.Now(), NodeID: "gw-" + string(rune('a'+i%3)), RequestID: "r" + hex.EncodeToString([]byte{byte(i)}),
			Method: "GET", Path: "/svc", Status: 502, ConfigRevision: bad})
	}
	if err := s.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	return good, bad
}

// Two control-plane nodes evaluate the same breach at the same moment: exactly
// one rollback must happen, and the decision must be visible to both.
func TestIntegrationAutoRollbackIsCoordinatedAcrossControlPlanes(t *testing.T) {
	s := openAdminTestStore(t)
	ctx := context.Background()
	good, bad := seedBadRelease(t, s)
	a, b := newControlPlane(t, s, "cp-a"), newControlPlane(t, s, "cp-b")

	var wg sync.WaitGroup
	results := make([]AutoRollbackEvaluationResult, 6)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cp := a
			if i%2 == 1 {
				cp = b
			}
			results[i] = cp.evaluateAutoRollback(ctx)
		}(i)
	}
	wg.Wait()

	triggered := 0
	for _, r := range results {
		if r.Triggered {
			triggered++
			if r.ActiveRevision != bad || r.BaselineRevision != good || r.Mode != "release" {
				t.Fatalf("wrong decision: %+v", r)
			}
		}
	}
	if triggered != 1 {
		t.Fatalf("rollbacks executed = %d, want exactly 1: %+v", triggered, results)
	}
	revs, _ := s.ListConfigRevisions(ctx, 10)
	rollbacks := 0
	for _, r := range revs {
		if r.RollbackOf != nil {
			rollbacks++
		}
	}
	if rollbacks != 1 {
		t.Fatalf("rollback revisions = %d, want 1", rollbacks)
	}
	if rev, _ := s.GetConfigRevision(ctx, bad); rev.Status != "rolled_back" {
		t.Fatalf("bad revision status = %s, want rolled_back", rev.Status)
	}
	// The cooldown is shared: the other node sees it immediately.
	if r := b.evaluateAutoRollback(ctx); !r.InCooldown && !r.Skipped {
		t.Fatalf("second node did not observe the shared cooldown: %+v", r)
	}
	cfg, _ := s.GetAutoRollbackSettings(ctx)
	if cfg.LastTriggeredRevision != bad || cfg.CooldownUntil == nil {
		t.Fatalf("decision not persisted: %+v", cfg)
	}
}

func TestIntegrationAutoRollbackAbortsFailingCanary(t *testing.T) {
	s := openAdminTestStore(t)
	ctx := context.Background()
	_, stable, err := s.CreateAPIAtomic(ctx, store.API{Name: "svc", BasePath: "/svc", UpstreamURL: "http://good", StripPath: true,
		AuthType: "none", TimeoutMS: 1000, Enabled: true}, "t", "good")
	if err != nil {
		t.Fatal(err)
	}
	canary, err := s.AtomicPublishCanary(ctx, "t", "risky", store.CanarySplit{TrafficPercent: 10}, func(tx pgxTx) error {
		_, err := tx.Exec(ctx, `UPDATE apis SET upstream_url='http://bad' WHERE name='svc'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var logs []store.RequestLog
	for i := 0; i < 10; i++ {
		logs = append(logs,
			store.RequestLog{TS: time.Now(), NodeID: "gw", RequestID: "c" + string(rune('a'+i)), Method: "GET", Path: "/svc", Status: 503, ConfigRevision: canary},
			store.RequestLog{TS: time.Now(), NodeID: "gw", RequestID: "s" + string(rune('a'+i)), Method: "GET", Path: "/svc", Status: 200, ConfigRevision: stable})
	}
	if err := s.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	cp := newControlPlane(t, s, "cp")
	res := cp.evaluateAutoRollback(ctx)
	if !res.Triggered || res.Mode != "canary" || res.ActiveRevision != canary || res.BaselineRevision != stable || res.BaselineErrorRatePct != 0 {
		t.Fatalf("canary evaluation = %+v", res)
	}
	if _, c, _ := s.GetRolloutState(ctx); c != 0 {
		t.Fatal("canary still in flight after automatic abort")
	}
	api, _ := s.ListAPIs(ctx)
	if api[len(api)-1].UpstreamURL != "http://good" {
		t.Fatalf("tables not restored: %s", api[len(api)-1].UpstreamURL)
	}
}

func TestIntegrationDeclarativePlanAndApplyOverHTTP(t *testing.T) {
	s := openAdminTestStore(t)
	cp := newControlPlane(t, s, "cp")
	h := cp.Handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	doc := `{"format_version":"1.0","plans":[{"name":"gold","rate_limit_per_minute":100}],
		"apis":[{"name":"orders","base_path":"/orders","upstream_url":"http://orders",
		"traffic_policy":{"targets":[{"url":"http://o1"},{"url":"http://o2","weight":3}],"circuit_breaker":{"failure_threshold":5}}}]}`

	// Legacy UI document shape is rejected with an actionable message.
	if rec := call("POST", "/api/system/apply", `{"format":"relayops-config/1.0","apis":[]}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "format_version") {
		t.Fatalf("legacy document: %d %s", rec.Code, rec.Body)
	}

	rec := call("POST", "/api/system/plan", doc)
	var plan ConfigPlan
	_ = json.Unmarshal(rec.Body.Bytes(), &plan)
	if rec.Code != 200 || !plan.Valid || plan.Summary.Create != 2 || plan.PlanHash == "" {
		t.Fatalf("plan: %d %s", rec.Code, rec.Body)
	}
	if rec := call("POST", "/api/system/apply?plan_hash=stale", doc); rec.Code != http.StatusConflict {
		t.Fatalf("stale plan hash must be refused: %d", rec.Code)
	}
	rec = call("POST", "/api/system/apply?plan_hash="+plan.PlanHash, doc)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"applied":true`) {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	if rec := call("POST", "/api/system/apply", doc); !strings.Contains(rec.Body.String(), `"applied":false`) {
		t.Fatalf("re-apply must be a no-op: %s", rec.Body)
	}

	// Export round-trips through apply unchanged.
	exp := call("GET", "/api/system/export", "")
	if strings.Contains(exp.Body.String(), "jwt_secret") {
		t.Fatal("export leaked secrets")
	}
	if rec := call("POST", "/api/system/plan", exp.Body.String()); !strings.Contains(rec.Body.String(), `"has_changes":false`) {
		t.Fatalf("export -> plan should be a no-op: %s", rec.Body)
	}

	// Canary rollout via apply.
	changed := strings.Replace(doc, `"rate_limit_per_minute":100`, `"rate_limit_per_minute":10`, 1)
	rec = call("POST", "/api/system/apply?rollout=canary&traffic_percent=20&canary_header=X-Canary", changed)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"rollout":"canary"`) {
		t.Fatalf("canary apply: %d %s", rec.Code, rec.Body)
	}
	if _, c, _ := s.GetRolloutState(context.Background()); c == 0 {
		t.Fatal("apply with rollout=canary did not create an in-flight canary")
	}
	if rec := call("POST", "/api/system/apply", doc); rec.Code != http.StatusConflict {
		t.Fatalf("fleet-wide apply during a canary must be refused: %d %s", rec.Code, rec.Body)
	}
}

func TestIntegrationDeactivatedAdminGets403AndReactivationDoesNotReviveSessions(t *testing.T) {
	s := openAdminTestStore(t)
	h := newControlPlane(t, s, "cp").Handler()
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := call("POST", "/api/admin/users", "test-admin-token",
		`{"name":"Ops","email":"ops@example.com","role":"operator","team":"Ops","password":"Sup3r-Secret-Pass!"}`)
	if rec.Code >= 300 {
		t.Fatalf("create user: %d %s", rec.Code, rec.Body)
	}
	var user struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &user)
	rec = call("POST", "/api/auth/login", "", `{"email":"ops@example.com","password":"Sup3r-Secret-Pass!"}`)
	var login struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	if rec.Code != 200 || login.Token == "" {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	if rec := call("GET", "/api/auth/me", login.Token, ""); rec.Code != 200 {
		t.Fatalf("me before deactivation: %d", rec.Code)
	}
	call("PUT", "/api/admin/users/"+user.ID, "test-admin-token", `{"name":"Ops","email":"ops@example.com","role":"operator","team":"Ops","active":false}`)
	if rec := call("GET", "/api/auth/me", login.Token, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "account_deactivated") {
		t.Fatalf("after deactivation: %d %s, want 403 account_deactivated", rec.Code, rec.Body)
	}
	call("PUT", "/api/admin/users/"+user.ID, "test-admin-token", `{"name":"Ops","email":"ops@example.com","role":"operator","team":"Ops","active":true}`)
	if rec := call("GET", "/api/auth/me", login.Token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old session after reactivation: %d %s, want 401", rec.Code, rec.Body)
	}
}

func TestIntegrationDirectSQLEditsAreAdoptedAsRevisions(t *testing.T) {
	s := openAdminTestStore(t)
	ctx := context.Background()
	cp := newControlPlane(t, s, "cp")
	_, r1, err := s.CreateAPIAtomic(ctx, store.API{Name: "svc", BasePath: "/svc", UpstreamURL: "http://u", StripPath: true,
		AuthType: "none", TimeoutMS: 1000, Enabled: true}, "t", "create")
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := s.DetectConfigDrift(ctx); d.Drifted {
		t.Fatalf("a normal publish must not look like drift: %+v", d)
	}

	if _, err := s.Pool.Exec(ctx, `UPDATE apis SET enabled=false WHERE name='svc'`); err != nil {
		t.Fatal(err)
	}
	d, rev, err := cp.adoptConfigDrift(ctx)
	if err != nil || !d.Drifted || rev <= r1 || len(d.ChangedAPIs) != 1 || d.ChangedAPIs[0] != "svc" {
		t.Fatalf("adoption: drift=%+v rev=%d err=%v", d, rev, err)
	}
	snap, _ := s.LoadSnapshotDataForNode(ctx, "default", false)
	for _, a := range snap.APIs {
		if a.Name == "svc" {
			t.Fatal("the disabled API is still served after adoption")
		}
	}
	if _, rev2, _ := cp.adoptConfigDrift(ctx); rev2 != 0 {
		t.Fatal("adoption must be idempotent")
	}

	// Rolling back records the restored tables, so it leaves no drift behind.
	if _, err := s.RollbackConfigRevision(ctx, r1, "t"); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.DetectConfigDrift(ctx); d.Drifted {
		t.Fatalf("rollback left drift: %+v", d)
	}

	// While a canary is in flight, drift is reported but not adopted.
	if _, err := s.AtomicPublishCanary(ctx, "t", "canary", store.CanarySplit{TrafficPercent: 10}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE apis SET timeout_ms=2000 WHERE name='svc'`); err != nil {
		t.Fatal(err)
	}
	if _, rev, err := cp.adoptConfigDrift(ctx); rev != 0 || err == nil || !strings.Contains(err.Error(), "canary") {
		t.Fatalf("adoption during canary: rev=%d err=%v", rev, err)
	}
}

// In release mode the baseline stopped serving when the candidate was published,
// so its sample must come from the window just before publication.
func TestIntegrationReleaseBaselineUsesPrePublicationWindow(t *testing.T) {
	s := openAdminTestStore(t)
	ctx := context.Background()
	_, good, err := s.CreateAPIAtomic(ctx, store.API{Name: "svc", BasePath: "/svc", UpstreamURL: "http://good", StripPath: true,
		AuthType: "none", TimeoutMS: 1000, Enabled: true}, "t", "good")
	if err != nil {
		t.Fatal(err)
	}
	// Baseline traffic before the release: 30 requests, all 503 (the upstream was already failing).
	var logs []store.RequestLog
	for i := 0; i < 30; i++ {
		logs = append(logs, store.RequestLog{TS: time.Now().Add(-20 * time.Second), NodeID: "gw", RequestID: "b" + strconv.Itoa(i),
			Method: "GET", Path: "/svc", Status: 503, ConfigRevision: good})
	}
	if err := s.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	api, _ := s.ListAPIs(ctx)
	api[len(api)-1].Description = "cosmetic change"
	_, bad, err := s.UpdateAPIAtomic(ctx, api[len(api)-1], "t", "cosmetic")
	if err != nil {
		t.Fatal(err)
	}
	logs = nil
	for i := 0; i < 30; i++ {
		logs = append(logs, store.RequestLog{TS: time.Now(), NodeID: "gw", RequestID: "c" + strconv.Itoa(i),
			Method: "GET", Path: "/svc", Status: 503, ConfigRevision: bad})
	}
	if err := s.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	res := newControlPlane(t, s, "cp").evaluateAutoRollback(ctx)
	if res.Triggered || !res.BaselineSufficient || res.DecisionBasis != "relative" || res.BaselineSampleCount != 30 {
		t.Fatalf("an upstream that was already failing must not roll back a cosmetic release: %+v", res)
	}

	// With the hold policy and no baseline at all, a failing release is held, not rolled back.
	cfg, _ := s.GetAutoRollbackSettings(ctx)
	cfg.InsufficientBaselineAction, cfg.MinBaselineRequests = "hold", 1000
	if _, err := s.UpdateAutoRollbackSettings(ctx, cfg, true, "t"); err != nil {
		t.Fatal(err)
	}
	res = newControlPlane(t, s, "cp").evaluateAutoRollback(ctx)
	if res.Triggered || res.DecisionBasis != "held" || !strings.Contains(res.Reason, "holding") {
		t.Fatalf("hold policy: %+v", res)
	}
}

func TestIntegrationNoDefaultAdminCredentials(t *testing.T) {
	s := openAdminTestStore(t)
	h := newControlPlane(t, s, "cp").Handler()
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// The seeded account's old default bearer token no longer authenticates
	// (the control plane here runs with a different cluster token).
	if rec := call("GET", "/api/apis", "relayops-admin", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("legacy default bearer token accepted: %d", rec.Code)
	}
	for _, pw := range []string{"relayops-admin", "admin123"} {
		if rec := call("POST", "/api/auth/login", "", `{"email":"admin@relayops.local","password":"`+pw+`"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("seeded admin signed in with default password %q: %d", pw, rec.Code)
		}
	}
	// Accounts created without a password (as SSO provisioning does) have none.
	call("POST", "/api/admin/users", "test-admin-token", `{"name":"Fed","email":"fed@example.com","role":"admin"}`)
	for _, pw := range []string{"admin123", "relayops-admin", ""} {
		if rec := call("POST", "/api/auth/login", "", `{"email":"fed@example.com","password":"`+pw+`"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("passwordless account signed in with %q: %d %s", pw, rec.Code, rec.Body)
		}
	}
	// A real password still works.
	call("POST", "/api/admin/users", "test-admin-token", `{"name":"Ops","email":"ops2@example.com","role":"operator","password":"Correct-Horse-Battery-9"}`)
	if rec := call("POST", "/api/auth/login", "", `{"email":"ops2@example.com","password":"Correct-Horse-Battery-9"}`); rec.Code != 200 {
		t.Fatalf("valid password rejected: %d %s", rec.Code, rec.Body)
	}
}

func TestIntegrationClusterTokenSignInSessionWorks(t *testing.T) {
	s := openAdminTestStore(t)
	h := newControlPlane(t, s, "cp").Handler()
	req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"token":"test-admin-token"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var login struct {
		Token string
		User  struct{ Role string }
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	if rec.Code != 200 || login.Token == "" {
		t.Fatalf("cluster-token sign-in: %d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest("GET", "/api/apis", nil)
	req.Header.Set("Authorization", "Bearer "+login.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("session from cluster-token sign-in rejected: %d %s", rec.Code, rec.Body)
	}
}

// An upstream outage right after a release: the errors are on an API the
// release did not change, so it must not be rolled back. The same errors on
// the API it did change roll it back.
func TestIntegrationAutoRollbackAttributesErrorsToChangedAPIs(t *testing.T) {
	for _, tc := range []struct {
		name          string
		changedFails  int // of 40 requests to the changed API
		otherFails    int // of 40 requests to the unchanged API
		wantRollback  bool
		wantBasisPart string
	}{
		{"outage on an unchanged API", 0, 40, false, "not_attributed"},
		{"failure on the changed API", 40, 0, true, "absolute"},
		{"shared outage on every API", 34, 40, false, "not_attributed"},
		{"changed API clearly worse than a degraded neighbour", 40, 12, true, "absolute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openAdminTestStore(t)
			ctx := context.Background()
			mk := func(name string) store.API {
				a, _, err := s.CreateAPIAtomic(ctx, store.API{Name: name, BasePath: "/" + name, UpstreamURL: "http://" + name, StripPath: true,
					AuthType: "none", TimeoutMS: 1000, Enabled: true}, "t", name)
				if err != nil {
					t.Fatal(err)
				}
				return a
			}
			changed, other := mk("orders"), mk("catalog")
			changed.Description = "new copy"
			_, rev, err := s.UpdateAPIAtomic(ctx, changed, "t", "edit orders")
			if err != nil {
				t.Fatal(err)
			}
			var logs []store.RequestLog
			status := func(i, fails int) int {
				if i < fails {
					return 502
				}
				return 200
			}
			for i := 0; i < 40; i++ {
				c, o := changed.ID, other.ID
				logs = append(logs,
					store.RequestLog{TS: time.Now(), NodeID: "gw", RequestID: fmt.Sprintf("c%d", i), APIID: &c, Method: "GET", Path: "/x", Status: status(i, tc.changedFails), ConfigRevision: rev},
					store.RequestLog{TS: time.Now(), NodeID: "gw", RequestID: fmt.Sprintf("o%d", i), APIID: &o, Method: "GET", Path: "/y", Status: status(i, tc.otherFails), ConfigRevision: rev})
			}
			if err := s.InsertLogs(ctx, logs); err != nil {
				t.Fatal(err)
			}
			res := newControlPlane(t, s, "cp-attr").evaluateAutoRollback(ctx)
			if res.Triggered != tc.wantRollback || res.DecisionBasis != tc.wantBasisPart {
				t.Fatalf("triggered=%v basis=%s reason=%s", res.Triggered, res.DecisionBasis, res.Reason)
			}
			if !tc.wantRollback && (len(res.ChangedAPIs) != 1 || res.ChangedAPIs[0] != changed.ID || res.UnchangedAPIErrorRatePct < float64(tc.otherFails)*2.5-1) {
				t.Fatalf("attribution details: %+v", res)
			}
		})
	}
}
