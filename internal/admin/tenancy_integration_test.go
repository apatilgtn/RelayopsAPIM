package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/license"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

// client drives the control plane over HTTP as one identity.
type client struct {
	t      *testing.T
	h      http.Handler
	token  string
	tenant string
}

func (c client) do(method, path, body string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.tenant != "" {
		req.Header.Set(TenantHeader, c.tenant)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

func (c client) in(tenant string) client { c.tenant = tenant; return c }

func (c client) must(method, path, body string, want int) map[string]any {
	c.t.Helper()
	rec := c.do(method, path, body)
	if rec.Code != want {
		c.t.Fatalf("%s %s as %s/%s: %d %s (want %d)", method, path, c.token[:min(8, len(c.token))], c.tenant, rec.Code, rec.Body, want)
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return m
}

func (c client) list(path string) []map[string]any {
	c.t.Helper()
	rec := c.do("GET", path, "")
	if rec.Code != 200 {
		c.t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	var out []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func names(items []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		out[it["name"].(string)] = true
	}
	return out
}

func login(t *testing.T, h http.Handler, email, password string) client {
	t.Helper()
	c := client{t: t, h: h}
	m := c.must("POST", "/api/auth/login", `{"email":"`+email+`","password":"`+password+`"}`, 200)
	return client{t: t, h: h, token: m["token"].(string)}
}

func TestIntegrationTenantIsolation(t *testing.T) {
	s := openAdminTestStore(t)
	h := newControlPlane(t, s, "cp").Handler()
	platform := client{t: t, h: h, token: "test-admin-token"}

	// Onboard two tenants and their people.
	platform.must("POST", "/api/tenants", `{"slug":"acme","name":"Acme"}`, 201)
	platform.must("POST", "/api/tenants", `{"slug":"globex","name":"Globex"}`, 201)
	for _, u := range []string{"alice", "bob", "carol", "dave"} {
		platform.must("POST", "/api/admin/users", `{"email":"`+u+`@corp.example","name":"`+u+`","role":"developer","password":"Passw0rd-`+u+`-long"}`, 201)
	}
	platform.must("POST", "/api/tenants/acme/members", `{"email":"alice@corp.example","role":"admin"}`, 201)
	platform.must("POST", "/api/tenants/globex/members", `{"email":"bob@corp.example","role":"operator"}`, 201)
	platform.must("POST", "/api/tenants/acme/members", `{"email":"carol@corp.example","role":"auditor"}`, 201)
	platform.must("POST", "/api/tenants/globex/members", `{"email":"carol@corp.example","role":"admin"}`, 201)

	alice := login(t, h, "alice@corp.example", "Passw0rd-alice-long")
	bob := login(t, h, "bob@corp.example", "Passw0rd-bob-long")
	carol := login(t, h, "carol@corp.example", "Passw0rd-carol-long")

	// Each tenant creates resources with the same names; base paths stay global.
	acmeAPI := alice.must("POST", "/api/apis", `{"name":"orders","base_path":"/acme/orders","upstream_url":"http://acme"}`, 201)
	alice.must("POST", "/api/plans", `{"name":"gold","rate_limit_per_minute":100}`, 201)
	acmeConsumer := alice.must("POST", "/api/consumers", `{"name":"shop"}`, 201)
	globexAPI := carol.in("globex").must("POST", "/api/apis", `{"name":"orders","base_path":"/globex/orders","upstream_url":"http://globex"}`, 201)
	carol.in("globex").must("POST", "/api/plans", `{"name":"gold","rate_limit_per_minute":5}`, 201)
	if rec := carol.in("globex").do("POST", "/api/apis", `{"name":"steal","base_path":"/acme/orders","upstream_url":"http://x"}`); rec.Code != http.StatusConflict {
		t.Fatalf("base paths are global across tenants: %d %s", rec.Code, rec.Body)
	}

	// Reads are confined to the caller's tenants.
	if got := alice.list("/api/apis"); len(got) != 1 || got[0]["base_path"] != "/acme/orders" {
		t.Fatalf("alice sees %v", got)
	}
	if got := bob.list("/api/apis"); len(got) != 1 || got[0]["base_path"] != "/globex/orders" {
		t.Fatalf("bob sees %v", got)
	}
	if got := carol.list("/api/apis"); len(got) != 2 {
		t.Fatalf("carol (member of both, none selected) sees %d APIs, want 2", len(got))
	}
	if got := platform.list("/api/plans"); len(got) < 2 {
		t.Fatalf("platform sees %d plans", len(got))
	}
	if got := platform.in("globex").list("/api/apis"); len(got) != 1 {
		t.Fatalf("platform scoped to globex sees %d APIs", len(got))
	}

	// Other tenants' resources are indistinguishable from missing ones.
	gid := globexAPI["id"].(string)
	for _, req := range [][2]string{{"GET", "/api/apis/" + gid}, {"PUT", "/api/apis/" + gid}, {"DELETE", "/api/apis/" + gid},
		{"POST", "/api/apis/" + gid + "/publish"}, {"POST", "/api/apis/" + gid + "/replay-preview"}, {"GET", "/api/apis/" + gid + "/consumer-impact"}} {
		if rec := alice.do(req[0], req[1], `{"description":"x"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("alice %s %s: %d, want 404", req[0], req[1], rec.Code)
		}
	}
	if rec := alice.do("POST", "/api/subscriptions", `{"consumer_id":"`+acmeConsumer["id"].(string)+`","api_id":"`+gid+`"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("subscribing to another tenant's API: %d", rec.Code)
	}
	// Even a platform admin cannot link resources across tenants.
	if rec := platform.do("POST", "/api/subscriptions", `{"consumer_id":"`+acmeConsumer["id"].(string)+`","api_id":"`+gid+`"}`); rec.Code != http.StatusConflict {
		t.Fatalf("cross-tenant subscription by platform admin: %d %s", rec.Code, rec.Body)
	}
	alice.must("POST", "/api/subscriptions", `{"consumer_id":"`+acmeConsumer["id"].(string)+`","api_id":"`+acmeAPI["id"].(string)+`"}`, 201)

	// Roles are per tenant.
	if rec := carol.in("acme").do("POST", "/api/apis", `{"name":"x","base_path":"/x","upstream_url":"http://x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("carol is an auditor in acme: %d", rec.Code)
	}
	if rec := carol.do("POST", "/api/consumers", `{"name":"x"}`); rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
		t.Fatalf("carol with no tenant selected must not create: %d", rec.Code)
	}
	if rec := alice.in("globex").do("GET", "/api/apis", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("selecting a tenant you are not in: %d", rec.Code)
	}

	// Fleet-wide operations are platform-only.
	for _, p := range []string{"/api/revisions", "/api/system/drift", "/api/admin/users", "/api/upstreams/health"} {
		if rec := alice.do("GET", p, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("tenant admin GET %s: %d, want 403", p, rec.Code)
		}
	}

	// Logs, analytics and audit are tenant-scoped.
	acmeID := acmeAPI["tenant_id"].(string)
	globexID := globexAPI["tenant_id"].(string)
	aid, gid2 := acmeAPI["id"].(string), gid
	if err := s.InsertLogs(context.Background(), []store.RequestLog{
		{TS: time.Now(), NodeID: "n", RequestID: "acme-1", APIID: &aid, Method: "GET", Path: "/acme/orders", Status: 200, TenantID: acmeID},
		{TS: time.Now(), NodeID: "n", RequestID: "globex-1", APIID: &gid2, Method: "GET", Path: "/globex/orders", Status: 500, TenantID: globexID},
	}); err != nil {
		t.Fatal(err)
	}
	logs := alice.list("/api/logs")
	if len(logs) != 1 || logs[0]["request_id"] != "acme-1" {
		t.Fatalf("alice's logs: %v", logs)
	}
	if rec := alice.do("GET", "/api/requests/globex-1/diagnose", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("diagnosing another tenant's request: %d", rec.Code)
	}
	sum := alice.must("GET", "/api/analytics/summary?window=1h", "", 200)
	if sum["total"].(float64) != 1 || sum["errors_5xx"].(float64) != 0 {
		t.Fatalf("alice's summary: %v", sum)
	}
	for _, e := range alice.list("/api/audit-logs") {
		if e["tenant_id"] != acmeID {
			t.Fatalf("alice sees an audit entry from another scope: %v", e)
		}
	}

	// Declarative config is per tenant: pruning acme never touches globex.
	exp := alice.do("GET", "/api/system/export", "")
	if strings.Contains(exp.Body.String(), "globex") {
		t.Fatalf("acme export leaked globex: %s", exp.Body)
	}
	alice.must("POST", "/api/system/apply?prune=true", `{"format_version":"1.0","plans":[],"apis":[{"name":"billing","base_path":"/acme/billing","upstream_url":"http://b"}]}`, 200)
	if got := names(bob.list("/api/apis")); !got["orders"] {
		t.Fatalf("acme prune removed globex's API: %v", got)
	}
	if rec := alice.do("POST", "/api/system/plan", `{"format_version":"1.0","apis":[{"name":"x","base_path":"/globex/orders","upstream_url":"http://x"}]}`); !strings.Contains(rec.Body.String(), "another tenant") {
		t.Fatalf("plan must reject a base path owned by another tenant: %s", rec.Body)
	}

	// Delegated onboarding: tenant admins manage their own members only.
	alice.must("POST", "/api/tenants/acme/members", `{"email":"dave@corp.example","role":"operator"}`, 201)
	if rec := alice.do("POST", "/api/tenants/globex/members", `{"email":"dave@corp.example","role":"admin"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("alice adding members to globex: %d", rec.Code)
	}
	if rec := bob.do("POST", "/api/tenants/globex/members", `{"email":"dave@corp.example","role":"admin"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("operator managing members: %d", rec.Code)
	}
	if rec := alice.do("POST", "/api/tenants", `{"slug":"rogue","name":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("tenant admin creating tenants: %d", rec.Code)
	}
	if got := alice.list("/api/tenants"); len(got) != 1 || got[0]["slug"] != "acme" {
		t.Fatalf("alice's tenant list: %v", got)
	}

	// Removing a member ends their access immediately.
	dave := login(t, h, "dave@corp.example", "Passw0rd-dave-long")
	dave.must("GET", "/api/apis", "", 200)
	members := alice.list("/api/tenants/acme/members")
	var daveID string
	for _, m := range members {
		if m["user_email"] == "dave@corp.example" {
			daveID = m["user_id"].(string)
		}
	}
	alice.must("DELETE", "/api/tenants/acme/members/"+daveID, "", http.StatusNoContent)
	if rec := dave.do("GET", "/api/apis", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("removed member still has a session: %d", rec.Code)
	}

	// Tenants that still own resources cannot be deleted.
	if rec := platform.do("DELETE", "/api/tenants/globex", ""); rec.Code != http.StatusConflict {
		t.Fatalf("deleting a non-empty tenant: %d", rec.Code)
	}

	// Self-service portal: one tenant per registration, public APIs only.
	if rec := (client{t: t, h: h}).do("POST", "/portal/api/register", `{"name":"mixer","api_ids":["`+aid+`","`+gid+`"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("portal registration across tenants: %d", rec.Code)
	}
	priv := platform.in("globex").must("POST", "/api/apis", `{"name":"internal","base_path":"/globex/internal","upstream_url":"http://i","visibility":"private"}`, 201)
	if rec := (client{t: t, h: h}).do("POST", "/portal/api/register", `{"name":"sneaky","api_ids":["`+priv["id"].(string)+`"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("portal registration for a private API: %d %s", rec.Code, rec.Body)
	}
	reg := (client{t: t, h: h}).must("POST", "/portal/api/register", `{"name":"globex-dev","api_ids":["`+gid+`"]}`, 201)
	if reg["consumer"].(map[string]any)["tenant_id"] != globexID {
		t.Fatalf("portal consumer created in the wrong tenant: %v", reg["consumer"])
	}
	cat := (client{t: t, h: h}).must("GET", "/portal/api/catalog?tenant=acme", "", 200)
	for _, a := range cat["apis"].([]any) {
		if a.(map[string]any)["tenant"] != "acme" {
			t.Fatalf("tenant catalog leaked: %v", a)
		}
	}
}

func TestIntegrationLicenseTenantCap(t *testing.T) {
	s := openAdminTestStore(t)
	ent := license.Entitlement{Edition: license.EditionPilot, MaxTenants: 1, Enforced: true, ReleaseSafety: true}
	gw := gateway.New(analytics.NewCollector(nil, realtime.NewHub(), "cp-license"), nil, "cp-license")
	h := New(s, gw, realtime.NewHub(), "test-admin-token", "cp-license", fstest.MapFS{}, WithLicense(ent)).Handler()
	platform := client{t: t, h: h, token: "test-admin-token"}
	me := platform.must("GET", "/api/auth/me", "", 200)
	feat, _ := me["features"].(map[string]any)
	if feat["edition"] != "pilot" || feat["release_safety"] != true {
		t.Fatalf("features: %v", feat)
	}
	if rec := platform.do("POST", "/api/tenants", `{"slug":"overflow","name":"Overflow"}`); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("pilot edition should reject a second tenant: %d %s", rec.Code, rec.Body)
	}
}
