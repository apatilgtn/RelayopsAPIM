package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A signup must wait for approval: inactive, listed for superadmins, and
// unable to sign in until approved.
func TestSignupAwaitsApproval(t *testing.T) {
	s := openAdminTestStore(t)
	h := newControlPlane(t, s, "signup-node").Handler()
	do := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("POST", "/api/auth/signup", `{"name":"Sam Requester","email":"sam@example.test","team":"Payments","password":"correct-horse-battery"}`, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("signup: %d %s", rec.Code, rec.Body.String())
	}
	u, err := s.GetAdminUserByEmail(context.Background(), "sam@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if u.Active || u.Role != "auditor" || u.SSOProvider != "local" {
		t.Fatalf("signup must be an inactive local auditor, got active=%v role=%s provider=%s", u.Active, u.Role, u.SSOProvider)
	}
	if rec := do("POST", "/api/auth/login", `{"email":"sam@example.test","password":"correct-horse-battery"}`, ""); rec.Code == http.StatusOK {
		t.Fatalf("unapproved signup signed in: %s", rec.Body.String())
	}
	rec := do("GET", "/api/admin/users", "", "test-admin-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("list users: %d %s", rec.Code, rec.Body.String())
	}
	var users []struct {
		Email       string  `json:"email"`
		Active      bool    `json:"active"`
		SSOProvider string  `json:"sso_provider"`
		LastLoginAt *string `json:"last_login_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range users {
		// The Approvals view's isSignupRequest filter.
		if x.Email == "sam@example.test" && !x.Active && x.SSOProvider == "local" && x.LastLoginAt == nil {
			found = true
		}
	}
	if !found {
		t.Fatal("signup is not listed as an account access request")
	}
}
