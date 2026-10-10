package admin

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relayops/apim/web"
	"golang.org/x/crypto/bcrypt"
)

func TestAuthPagesAndSignupValidation(t *testing.T) {
	static, err := fs.Sub(web.Static, "static")
	if err != nil {
		t.Fatal(err)
	}
	handler := New(nil, nil, nil, "test-private-token", "test-node", static).Handler()
	for _, path := range []string{"/login", "/signup"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 || !strings.Contains(response.Body.String(), "auth-form") || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("auth page %s not served correctly", path)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/auth/signup", strings.NewReader(`{"name":"Alex","email":"not-an-email","team":"Platform","password":"short","role":"superadmin","active":true}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid signup accepted: %d", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"token":"incorrect-token"}`)))
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "invalid_admin_token") || strings.Contains(response.Body.String(), "email is required") {
		t.Fatalf("incorrect token must get a token-specific error: %d %s", response.Code, response.Body.String())
	}
}

func TestSignupPasswordVerification(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-password-123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyAccountPassword(string(hash), "correct-password-123") {
		t.Fatal("signup password must authenticate")
	}
	if verifyAccountPassword(string(hash), "wrong-password") || verifyAccountPassword("", "") {
		t.Fatal("invalid credentials must fail")
	}
}
