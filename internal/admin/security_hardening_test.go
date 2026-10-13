package admin

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// A suite required by an enforced release gate can only be changed or deleted
// by an admin or superadmin, and only admins may change test environments:
// otherwise a developer or operator could weaken the checks (or repoint where
// they run) and let an unsafe release pass, or delete the suite and block
// every promotion. Found by the 2026-10-13 internal assessment.
func TestIntegrationGateSuitesAndEnvironmentsAreProtected(t *testing.T) {
	st := openAdminTestStore(t)
	cp := newControlPlane(t, st, "cp-gate-protect")
	h := cp.Handler()
	platform := client{t: t, h: h, token: "test-admin-token"}

	api := platform.must("POST", "/api/apis", `{"name":"orders","base_path":"/orders","upstream_url":"http://127.0.0.1:1","auth_type":"api_key"}`, 201)
	apiID := api["id"].(string)
	suiteBody := func(name, expected string) string {
		return `{"name":"` + name + `","api_id":"` + apiID + `","definition":{"name":"` + name + `","requests":[{"id":"r","name":"r","method":"GET","path":"/orders","assertions":[{"type":"status_code","expected":"` + expected + `"}]}]}}`
	}
	gated := platform.must("POST", "/api/tests/suites", suiteBody("gate", "401"), 201)
	gatedID := gated["suite"].(map[string]any)["id"].(string)
	free := platform.must("POST", "/api/tests/suites", suiteBody("free", "401"), 201)
	freeID := free["suite"].(map[string]any)["id"].(string)
	env := platform.must("POST", "/api/tests/environments", `{"name":"Production","gateway_target":"","variables":{},"credential_bindings":{}}`, 201)
	envID := env["id"].(string)
	platform.must("PUT", "/api/tests/gates/"+apiID, `{"required_suite_ids":["`+gatedID+`"],"freshness_seconds":3600,"enforcement_enabled":true}`, 200)

	for _, u := range []struct{ email, role string }{{"dev@corp.example", "developer"}, {"op@corp.example", "operator"}, {"adm@corp.example", "admin"}} {
		platform.must("POST", "/api/admin/users", `{"email":"`+u.email+`","name":"`+u.role+`","role":"`+u.role+`","password":"Passw0rd-`+u.role+`-long"}`, 201)
	}
	dev := login(t, h, "dev@corp.example", "Passw0rd-developer-long")
	op := login(t, h, "op@corp.example", "Passw0rd-operator-long")
	adm := login(t, h, "adm@corp.example", "Passw0rd-admin-long")

	weakened := suiteBody("gate", "1xx-5xx")
	for name, c := range map[string]client{"developer": dev, "operator": op} {
		if rec := c.do("PUT", "/api/tests/suites/"+gatedID, weakened); rec.Code != 403 {
			t.Errorf("%s rewrote a gate suite: %d %s", name, rec.Code, rec.Body.String())
		}
		if rec := c.do("DELETE", "/api/tests/suites/"+gatedID, ""); rec.Code != 403 {
			t.Errorf("%s deleted a gate suite: %d", name, rec.Code)
		}
		if rec := c.do("PUT", "/api/tests/environments/"+envID, `{"name":"Production","gateway_target":"http://127.0.0.1:9"}`); rec.Code != 403 {
			t.Errorf("%s repointed a test environment: %d", name, rec.Code)
		}
	}
	// Ordinary authoring still works.
	if rec := dev.do("PUT", "/api/tests/suites/"+freeID, suiteBody("free", "2xx")); rec.Code != 200 {
		t.Errorf("developer could not edit a non-gate suite: %d %s", rec.Code, rec.Body.String())
	}
	if rec := dev.do("POST", "/api/tests/suites", suiteBody("new", "401")); rec.Code != 201 {
		t.Errorf("developer could not create a suite: %d", rec.Code)
	}
	// Admins keep full control.
	if rec := adm.do("PUT", "/api/tests/suites/"+gatedID, suiteBody("gate", "401")); rec.Code != 200 {
		t.Errorf("admin could not update the gate suite: %d %s", rec.Code, rec.Body.String())
	}
}

func TestConsoleSecurityHeaders(t *testing.T) {
	st := openAdminTestStore(t)
	h := newControlPlane(t, st, "cp-headers").Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	want := map[string]string{
		"Content-Security-Policy": "frame-ancestors 'none'",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "strict-origin-when-cross-origin",
		"X-Content-Type-Options":  "nosniff",
		"Permissions-Policy":      "camera=()",
	}
	for k, v := range want {
		if !strings.Contains(rec.Header().Get(k), v) {
			t.Errorf("%s = %q, want it to contain %q", k, rec.Header().Get(k), v)
		}
	}
	if strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self' 'unsafe-inline'") {
		t.Error("CSP allows inline scripts")
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS sent over plain HTTP")
	}
	req := httptest.NewRequest("GET", "/login", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Header().Get("Strict-Transport-Security"), "max-age=") {
		t.Errorf("HSTS missing behind an HTTPS proxy: %q", rec.Header().Get("Strict-Transport-Security"))
	}
}
