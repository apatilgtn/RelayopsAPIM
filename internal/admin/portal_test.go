package admin_test

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relayops/apim/internal/admin"
	"github.com/relayops/apim/web"
)

func portalServer(t *testing.T, options ...admin.Option) http.Handler {
	t.Helper()
	static, err := fs.Sub(web.Static, "static")
	if err != nil {
		t.Fatal(err)
	}
	return admin.New(nil, nil, nil, "private-admin-token", "test-node", static, options...).Handler()
}

func TestProductEmbedsDeveloperHub(t *testing.T) {
	handler := portalServer(t)
	for _, tc := range []struct {
		path, contentType, content string
	}{
		{"/portal", "text/html", "API workbench"},
		{"/portal/assets/portal.css", "text/css", ".rail"},
		{"/portal/assets/portal.js", "text/javascript", "/portal/api/catalog"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("GET", tc.path, nil))
			if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), tc.contentType) || !strings.Contains(response.Body.String(), tc.content) {
				t.Fatalf("product route %s did not serve its embedded asset: status=%d type=%s", tc.path, response.Code, response.Header().Get("Content-Type"))
			}
		})
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/portal/assets/.env", nil))
	if response.Code != http.StatusNotFound {
		t.Fatal("unknown portal assets must not be exposed")
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/overview", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatal("public developer hub must not remove admin API authentication")
	}
}

func TestWorkbenchUsesConfiguredGateway(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.RequestURI() != "/orders?source=portal" || string(body) != `{"quantity":1}` || r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("workbench changed the request: method=%s path=%s body=%s", r.Method, r.URL.RequestURI(), body)
		}
		w.Header().Set("X-Request-ID", "gateway-request")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	}))
	defer gateway.Close()
	handler := portalServer(t, admin.WithGatewayAddress(strings.TrimPrefix(gateway.URL, "http://")))
	input := `{"method":"POST","path":"/orders?source=portal","headers":{"X-API-Key":"test-key"},"body":"{\"quantity\":1}"}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/portal/api/try", strings.NewReader(input)))
	var result struct {
		Status  int               `json:"status"`
		Body    string            `json:"body"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || result.Status != http.StatusCreated || result.Body != `{"created":true}` || result.Headers["X-Request-Id"] != "gateway-request" {
		t.Fatalf("workbench did not preserve the gateway response: %s", response.Body.String())
	}
}

func TestWorkbenchDoesNotFollowRedirects(t *testing.T) {
	visited := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { visited = true }))
	defer target.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer gateway.Close()
	handler := portalServer(t, admin.WithGatewayAddress(strings.TrimPrefix(gateway.URL, "http://")))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/portal/api/try", strings.NewReader(`{"method":"GET","path":"/redirect"}`)))
	if visited || !strings.Contains(response.Body.String(), `"status":302`) {
		t.Fatal("workbench must return redirects without following them outside the gateway")
	}
}

func TestWorkbenchRejectsInvalidTargets(t *testing.T) {
	handler := portalServer(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "https://example.com"}, {"GET", "//example.com"}, {"GET", "/orders#fragment"}, {"GET", "/\\example.com"}, {"CONNECT", "/orders"},
	} {
		body, _ := json.Marshal(map[string]string{"method": tc.method, "path": tc.path})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("POST", "/portal/api/try", strings.NewReader(string(body))))
		if response.Code != http.StatusBadRequest {
			t.Errorf("accepted invalid target %s %s: %d", tc.method, tc.path, response.Code)
		}
	}
}
