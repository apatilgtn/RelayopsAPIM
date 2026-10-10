package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relayops/apim/internal/orbit"
	"github.com/relayops/apim/internal/store"
)

// orbitFakeModel asks for three tools, then answers. It records every tool
// result Orbit sends back so the test can check what left the control plane.
type orbitFakeModel struct {
	mu          sync.Mutex
	toolResults []string
}

func (m *orbitFakeModel) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []orbit.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		last := body.Messages[len(body.Messages)-1]
		w.Header().Set("Content-Type", "application/json")
		if last.Role == "user" {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
				{"id":"a","type":"function","function":{"name":"list_apis","arguments":"{}"}},
				{"id":"b","type":"function","function":{"name":"get_traffic_summary","arguments":"{\"window\":\"1h\"}"}},
				{"id":"c","type":"function","function":{"name":"list_recent_changes","arguments":"{}"}}]}}]}`))
			return
		}
		m.mu.Lock()
		for _, msg := range body.Messages {
			if msg.Role == "tool" {
				m.toolResults = append(m.toolResults, msg.Content)
			}
		}
		m.mu.Unlock()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"orders is healthy: 2 requests, no 5xx."}}],"usage":{"prompt_tokens":120,"completion_tokens":12}}`))
	}
}

func TestIntegrationOrbitAnswersWithTheUsersPermissions(t *testing.T) {
	st := openAdminTestStore(t)
	ctx := context.Background()
	model := &orbitFakeModel{}
	llm := httptest.NewServer(model.handler(t))
	defer llm.Close()

	api, _, err := st.CreateAPIAtomic(ctx, store.API{Name: "orders", BasePath: "/orders", UpstreamURL: "http://orders.internal", StripPath: true,
		AuthType: "none", TimeoutMS: 1000, Enabled: true, RequestHeaders: map[string]string{"Authorization": "Bearer SUPERSECRET-UPSTREAM"}}, "t", "orders")
	if err != nil {
		t.Fatal(err)
	}
	id := api.ID
	if err := st.InsertLogs(ctx, []store.RequestLog{
		{TS: time.Now(), NodeID: "n", RequestID: "r1", APIID: &id, Method: "GET", Path: "/orders/1", Status: 200, LatencyMS: 12},
		{TS: time.Now(), NodeID: "n", RequestID: "r2", APIID: &id, Method: "GET", Path: "/orders/2", Status: 200, LatencyMS: 14},
	}); err != nil {
		t.Fatal(err)
	}
	userWithToken := func(role, token string) {
		u, err := st.CreateAdminUser(ctx, store.AdminUser{Name: role, Email: role + "@example.test", Role: role})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, `UPDATE admin_users SET token_hash=$1 WHERE id=$2`, store.HashKey(token), u.ID); err != nil {
			t.Fatal(err)
		}
	}
	userWithToken("operator", "operator-token")
	userWithToken("auditor", "auditor-token")

	srv := New(st, nil, nil, "test-admin-token", "cp", nil, WithOrbit(orbit.Config{BaseURL: llm.URL, Model: "test-model", APIKey: "k"}))
	h := srv.Handler()
	ask := func(token string) (int, orbit.Answer) {
		body, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": "How is orders doing?"}}, "context": map[string]string{"view": "apis"}})
		req := httptest.NewRequest(http.MethodPost, "/api/orbit/ask", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var ans orbit.Answer
		_ = json.Unmarshal(rec.Body.Bytes(), &ans)
		if rec.Code != http.StatusOK {
			t.Logf("body: %s", rec.Body.String())
		}
		return rec.Code, ans
	}
	stepOK := func(ans orbit.Answer, tool string) (bool, string) {
		for _, s := range ans.Steps {
			if s.Tool == tool {
				return s.OK, s.Error
			}
		}
		t.Fatalf("tool %s not run: %+v", tool, ans.Steps)
		return false, ""
	}

	// An operator may ask, but the audit log is not theirs to read: the tool
	// fails exactly as the page would.
	code, ans := ask("operator-token")
	if code != http.StatusOK || ans.Text != "orders is healthy: 2 requests, no 5xx." || ans.Model != "test-model" {
		t.Fatalf("operator: %d %+v", code, ans)
	}
	if ok, _ := stepOK(ans, "list_apis"); !ok {
		t.Fatal("list_apis failed for operator")
	}
	if ok, _ := stepOK(ans, "get_traffic_summary"); !ok {
		t.Fatal("get_traffic_summary failed for operator")
	}
	if ok, msg := stepOK(ans, "list_recent_changes"); ok || !strings.Contains(msg, "403") {
		t.Fatalf("operator read the audit log through Orbit: ok=%v %s", ok, msg)
	}

	// Auditors are read-only but may ask, and may read the audit log.
	code, ans = ask("auditor-token")
	if code != http.StatusOK {
		t.Fatalf("auditor: %d", code)
	}
	if ok, msg := stepOK(ans, "list_recent_changes"); !ok {
		t.Fatalf("auditor audit read failed: %s", msg)
	}

	// What reached the model: real data, no credentials.
	model.mu.Lock()
	sent := strings.Join(model.toolResults, "\n")
	model.mu.Unlock()
	if !strings.Contains(sent, "orders") || !strings.Contains(sent, "/orders") {
		t.Fatalf("tool data missing from model input: %.300s", sent)
	}
	if strings.Contains(sent, "SUPERSECRET") || strings.Contains(sent, "request_headers") {
		t.Fatal("an upstream credential was sent to the model")
	}

	// Every question is audited with the tools consulted, not the content.
	audits, err := st.ListAuditLogs(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, a := range audits {
		if a.Action == "ORBIT_ASK" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("ORBIT_ASK audit entries = %d, want 2", found)
	}
}

func TestOrbitNotConfigured(t *testing.T) {
	srv := New(nil, nil, nil, "test-admin-token", "cp", nil)
	req := httptest.NewRequest(http.MethodGet, "/api/orbit/status", nil)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	if srv.features()["orbit"] != false {
		t.Fatal("orbit reported as enabled without a model")
	}
}

func TestOrbitRateLimitAndHistory(t *testing.T) {
	o := &orbitState{windows: map[string][]time.Time{}}
	now := time.Now()
	for i := 0; i < orbitPerMinute; i++ {
		if !o.allow("u", now) {
			t.Fatalf("question %d refused", i+1)
		}
	}
	if o.allow("u", now) {
		t.Fatal("rate limit not applied")
	}
	if !o.allow("u", now.Add(61*time.Second)) || !o.allow("other", now) {
		t.Fatal("limit must be per user and per minute")
	}
	if _, err := orbitHistory([]orbit.Message{{Role: "system", Content: "x"}, {Role: "user", Content: "q"}}); err == nil {
		t.Fatal("system messages from the client must be rejected")
	}
	if _, err := orbitHistory([]orbit.Message{{Role: "user", Content: strings.Repeat("a", 4001)}}); err == nil {
		t.Fatal("oversized message accepted")
	}
}

func TestRedactSecrets(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`{"name":"x","client_secret":"s","tokens_total":5,"request_headers":{"A":"b"},
		"nested":[{"api_key":"k","password":"p","key_prefix":"rk_1","prompt_tokens":3}]}`), &v)
	out, _ := json.Marshal(redactSecrets(v))
	got := string(out)
	for _, leak := range []string{`"s"`, `"k"`, `"p"`, "request_headers", "client_secret"} {
		if strings.Contains(got, leak) {
			t.Fatalf("leaked %s in %s", leak, got)
		}
	}
	for _, keep := range []string{"tokens_total", "prompt_tokens", "key_prefix", `"name":"x"`} {
		if !strings.Contains(got, keep) {
			t.Fatalf("dropped %s from %s", keep, got)
		}
	}
}
