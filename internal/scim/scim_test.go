package scim

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relayops/apim/internal/store"
)

type mockUserStore struct {
	users map[string]store.AdminUser
}

func newMockUserStore() *mockUserStore {
	return &mockUserStore{users: make(map[string]store.AdminUser)}
}

func (m *mockUserStore) ListAdminUsers(ctx context.Context) ([]store.AdminUser, error) {
	out := make([]store.AdminUser, 0, len(m.users))
	for _, u := range m.users {
		out = append(out, u)
	}
	return out, nil
}

func (m *mockUserStore) GetAdminUser(ctx context.Context, id string) (store.AdminUser, error) {
	u, ok := m.users[id]
	if !ok {
		return store.AdminUser{}, store.ErrNotFound
	}
	return u, nil
}

func (m *mockUserStore) GetAdminUserByEmail(ctx context.Context, email string) (store.AdminUser, error) {
	for _, u := range m.users {
		if u.Email == email {
			return u, nil
		}
	}
	return store.AdminUser{}, store.ErrNotFound
}

func (m *mockUserStore) CreateAdminUser(ctx context.Context, u store.AdminUser) (store.AdminUser, error) {
	for _, existing := range m.users {
		if existing.Email == u.Email {
			return store.AdminUser{}, store.ErrConflict
		}
	}
	if u.ID == "" {
		u.ID = "user-" + time.Now().Format("150405.000000")
	}
	u.CreatedAt = time.Now()
	u.UpdatedAt = time.Now()
	m.users[u.ID] = u
	return u, nil
}

func (m *mockUserStore) UpdateAdminUser(ctx context.Context, u store.AdminUser) (store.AdminUser, error) {
	if _, ok := m.users[u.ID]; !ok {
		return store.AdminUser{}, store.ErrNotFound
	}
	u.UpdatedAt = time.Now()
	m.users[u.ID] = u
	return u, nil
}

func (m *mockUserStore) DeleteAdminUser(ctx context.Context, id string) error {
	if _, ok := m.users[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.users, id)
	return nil
}

func TestSCIMAuthEnforcement(t *testing.T) {
	st := newMockUserStore()
	srv := NewServer(st, "secret-token", "https://api.relayops.test/scim/v2")
	h := srv.Handler()

	// 1. Missing Authorization header
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/Users", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without token, got %d", rec.Code)
	}

	// 2. Wrong token
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/Users", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized with wrong token, got %d", rec.Code)
	}

	// 3. Valid token
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/Users", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with valid token, got %d", rec.Code)
	}
}

func TestSCIMServiceProviderConfigAndMetadata(t *testing.T) {
	st := newMockUserStore()
	srv := NewServer(st, "token", "https://api.relayops.test/scim/v2")
	h := srv.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ServiceProviderConfig", nil)
	req.Header.Set("Authorization", "Bearer token")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for ServiceProviderConfig, got %d", rec.Code)
	}
	var cfg ServiceProviderConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("failed to decode ServiceProviderConfig: %v", err)
	}
	if !cfg.Patch.Supported || !cfg.Filter.Supported {
		t.Fatalf("patch and filter must be supported: %+v", cfg)
	}
}

func TestSCIMUserLifecycle(t *testing.T) {
	st := newMockUserStore()
	srv := NewServer(st, "test-token", "https://api.relayops.test/scim/v2")
	h := srv.Handler()

	// 1. Create User
	createPayload := `{
		"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"],
		"userName": "alice@example.com",
		"name": {
			"formatted": "Alice Smith",
			"familyName": "Smith",
			"givenName": "Alice"
		},
		"emails": [{"value": "alice@example.com", "primary": true}],
		"roles": [{"value": "operator", "primary": true}],
		"active": true
	}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/Users", bytes.NewBufferString(createPayload))
	req.Header.Set("Authorization", "Bearer test-token")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}
	var created UserResource
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if created.UserName != "alice@example.com" || !created.Active || len(created.Roles) == 0 || created.Roles[0].Value != "operator" {
		t.Fatalf("unexpected created user: %+v", created)
	}
	userID := created.ID

	// 2. Query with filter
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/Users?filter=userName%20eq%20%22alice@example.com%22", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var listResp ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list error: %v", err)
	}
	if listResp.TotalResults != 1 || listResp.Resources[0].ID != userID {
		t.Fatalf("expected 1 result matching alice, got: %+v", listResp)
	}

	// 3. Patch User (Deprovision / Set active=false)
	patchPayload := `{
		"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations": [
			{"op": "replace", "path": "active", "value": false},
			{"op": "replace", "path": "roles", "value": "auditor"}
		]
	}`
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PATCH", "/Users/"+userID, bytes.NewBufferString(patchPayload))
	req.Header.Set("Authorization", "Bearer test-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for PATCH, got %d: %s", rec.Code, rec.Body.String())
	}
	var patched UserResource
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal patch error: %v", err)
	}
	if patched.Active != false {
		t.Fatalf("expected active to be false after patch, got: %v", patched.Active)
	}
	if len(patched.Roles) == 0 || patched.Roles[0].Value != "auditor" {
		t.Fatalf("expected role to be auditor, got: %+v", patched.Roles)
	}

	// 4. Delete User (Deprovision / Remove)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("DELETE", "/Users/"+userID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 No Content, got %d", rec.Code)
	}

	// 5. Verify User is gone
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/Users/"+userID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found after deletion, got %d", rec.Code)
	}
}
