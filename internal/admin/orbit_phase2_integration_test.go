package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relayops/apim/internal/orbit"
	"github.com/relayops/apim/internal/store"
)

// scriptedModel asks for one tool call, then answers with fixed text.
func scriptedModel(t *testing.T, tool, args string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []orbit.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body.Messages[len(body.Messages)-1].Role == "user" {
			call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{"id": "p1", "type": "function", "function": map[string]any{"name": tool, "arguments": args}}}}}}})
			_, _ = w.Write(call)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"I drafted a proposal for you to review."}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type orbitHarness struct {
	t  *testing.T
	st *store.Store
	h  http.Handler
}

func (o orbitHarness) user(role, email, token string) {
	u, err := o.st.CreateAdminUser(context.Background(), store.AdminUser{Name: email, Email: email, Role: role})
	if err != nil {
		o.t.Fatal(err)
	}
	if _, err := o.st.Pool.Exec(context.Background(), `UPDATE admin_users SET token_hash=$1 WHERE id=$2`, store.HashKey(token), u.ID); err != nil {
		o.t.Fatal(err)
	}
}

func (o orbitHarness) call(method, path, token string, body any) (int, map[string]any) {
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	o.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestIntegrationOrbitProposalsAreDraftedNotApplied(t *testing.T) {
	st := openAdminTestStore(t)
	ctx := context.Background()
	api, _, err := st.CreateAPIAtomic(ctx, store.API{Name: "catalog", BasePath: "/catalog", UpstreamURL: "http://catalog.internal", StripPath: true,
		AuthType: "none", TimeoutMS: 1000, Enabled: true}, "t", "catalog")
	if err != nil {
		t.Fatal(err)
	}
	model := scriptedModel(t, "propose_api_change", fmt.Sprintf(`{"api_id":%q,"changes":{"rate_limit_per_minute":600},"reason":"Anyone can call it without limit."}`, api.ID))
	srv := New(st, nil, nil, "test-admin-token", "cp", nil, WithOrbit(orbit.Config{BaseURL: model.URL, Model: "m", APIKey: "k"}))
	o := orbitHarness{t: t, st: st, h: srv.Handler()}
	o.user("operator", "op1@example.test", "op1")
	o.user("operator", "op2@example.test", "op2")
	o.user("auditor", "aud@example.test", "aud")

	code, ans := o.call(http.MethodPost, "/api/orbit/ask", "op1", map[string]any{"messages": []map[string]string{{"role": "user", "content": "Add a rate limit to catalog"}}})
	if code != http.StatusOK {
		t.Fatalf("ask: %d %v", code, ans)
	}
	props, _ := ans["proposals"].([]any)
	if len(props) != 1 {
		t.Fatalf("proposals: %v", ans)
	}
	p := props[0].(map[string]any)
	ch := p["changes"].([]any)[0].(map[string]any)
	if ch["field"] != "rate_limit_per_minute" || ch["from"] != float64(0) || ch["to"] != float64(600) || p["api_name"] != "catalog" {
		t.Fatalf("proposal content: %v", p)
	}
	if _, ok := p["recent_requests"]; !ok {
		t.Fatalf("impact preview missing: %v", p)
	}
	token := p["token"].(string)

	// Drafting changed nothing.
	if a, _ := st.GetAPI(ctx, api.ID); a.RateLimitPerMinute != 0 {
		t.Fatal("drafting a proposal changed the API")
	}
	// Auditors are read-only; another operator cannot apply someone else's draft;
	// a tampered token is rejected.
	if code, _ := o.call(http.MethodPost, "/api/orbit/proposals/apply", "aud", map[string]string{"token": token}); code != http.StatusForbidden {
		t.Fatalf("auditor apply: %d", code)
	}
	if code, body := o.call(http.MethodPost, "/api/orbit/proposals/apply", "op2", map[string]string{"token": token}); code != http.StatusForbidden || body["error"] != "proposal_not_yours" {
		t.Fatalf("other operator apply: %d %v", code, body)
	}
	payload, sig, _ := strings.Cut(token, ".")
	tampered := strings.Replace(payload, "A", "B", 1) + "." + sig
	if code, _ := o.call(http.MethodPost, "/api/orbit/proposals/apply", "op1", map[string]string{"token": tampered}); code != http.StatusBadRequest {
		t.Fatalf("tampered token: %d", code)
	}

	// The person who asked applies it.
	if code, body := o.call(http.MethodPost, "/api/orbit/proposals/apply", "op1", map[string]string{"token": token}); code != http.StatusOK {
		t.Fatalf("apply: %d %v", code, body)
	}
	if a, _ := st.GetAPI(ctx, api.ID); a.RateLimitPerMinute != 600 {
		t.Fatalf("rate limit = %d after apply", a.RateLimitPerMinute)
	}
	// Applying again is refused: the API changed since the draft.
	if code, body := o.call(http.MethodPost, "/api/orbit/proposals/apply", "op1", map[string]string{"token": token}); code != http.StatusConflict {
		t.Fatalf("stale apply: %d %v", code, body)
	}
	// Usage goes to /metrics.
	mrec := httptest.NewRecorder()
	mreq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	mreq.Header.Set("Authorization", "Bearer test-admin-token")
	o.h.ServeHTTP(mrec, mreq)
	for _, want := range []string{`relayops_orbit_questions_total{outcome="answered"} 1`, `relayops_orbit_proposals_total{stage="drafted"} 1`,
		`relayops_orbit_proposals_total{stage="applied"} 1`, `relayops_orbit_tokens_total{model="m",kind="prompt"}`} {
		if !strings.Contains(mrec.Body.String(), want) {
			t.Fatalf("metrics missing %s", want)
		}
	}
	audits, _ := st.ListAuditLogs(ctx, 50)
	applied := false
	for _, a := range audits {
		if a.Action == "ORBIT_PROPOSAL_APPLIED" && a.ResourceID == api.ID {
			applied = true
		}
	}
	if !applied {
		t.Fatal("ORBIT_PROPOSAL_APPLIED not audited")
	}

	// Fields outside the allow-list are refused at drafting.
	bad := scriptedModel(t, "propose_api_change", fmt.Sprintf(`{"api_id":%q,"changes":{"upstream_url":"http://evil"},"reason":"x"}`, api.ID))
	srv2 := New(st, nil, nil, "test-admin-token", "cp", nil, WithOrbit(orbit.Config{BaseURL: bad.URL, Model: "m", APIKey: "k"}))
	o2 := orbitHarness{t: t, st: st, h: srv2.Handler()}
	_, ans = o2.call(http.MethodPost, "/api/orbit/ask", "op1", map[string]any{"messages": []map[string]string{{"role": "user", "content": "x"}}})
	if props, _ := ans["proposals"].([]any); len(props) != 0 {
		t.Fatalf("disallowed field was drafted: %v", ans)
	}
	steps := ans["steps"].([]any)
	if st0 := steps[0].(map[string]any); st0["ok"] != false || !strings.Contains(fmt.Sprint(st0["error"]), "cannot propose") {
		t.Fatalf("step: %v", st0)
	}
}

func TestIntegrationOrbitSignalsFlagSpikesAndExposure(t *testing.T) {
	st := openAdminTestStore(t)
	ctx := context.Background()
	pay, _, err := st.CreateAPIAtomic(ctx, store.API{Name: "payments", BasePath: "/payments", UpstreamURL: "http://payments.internal", StripPath: true,
		AuthType: "api_key", RateLimitPerMinute: 100, TimeoutMS: 1000, Enabled: true}, "t", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateAPIAtomic(ctx, store.API{Name: "open-catalog", BasePath: "/open", UpstreamURL: "http://open.internal", StripPath: true,
		AuthType: "none", TimeoutMS: 1000, Enabled: true}, "t", "open"); err != nil {
		t.Fatal(err)
	}
	id := pay.ID
	var logs []store.RequestLog
	for i := 0; i < 1000; i++ { // a healthy baseline two hours ago
		logs = append(logs, store.RequestLog{TS: time.Now().Add(-2 * time.Hour), NodeID: "n", RequestID: fmt.Sprintf("b%d", i), APIID: &id, APIName: "payments", Method: "GET", Path: "/payments", Status: 200, LatencyMS: 20})
	}
	for i := 0; i < 40; i++ { // then 25% failing in the last few minutes
		status := 200
		if i%4 == 0 {
			status = 502
		}
		logs = append(logs, store.RequestLog{TS: time.Now().Add(-2 * time.Minute), NodeID: "n", RequestID: fmt.Sprintf("r%d", i), APIID: &id, APIName: "payments", Method: "GET", Path: "/payments", Status: status, LatencyMS: 25})
	}
	// A brand-new API failing outright: its only traffic is the outage, so its
	// own 24-hour baseline must not hide it.
	fresh, _, err := st.CreateAPIAtomic(ctx, store.API{Name: "billing", BasePath: "/billing", UpstreamURL: "http://billing.internal", StripPath: true,
		AuthType: "api_key", RateLimitPerMinute: 100, TimeoutMS: 1000, Enabled: true}, "t", "billing")
	if err != nil {
		t.Fatal(err)
	}
	fid := fresh.ID
	for i := 0; i < 30; i++ {
		logs = append(logs, store.RequestLog{TS: time.Now().Add(-time.Minute), NodeID: "n", RequestID: fmt.Sprintf("f%d", i), APIID: &fid, APIName: "billing", Method: "GET", Path: "/billing", Status: 502, LatencyMS: 3})
	}
	// One client bursting 50 requests in a single minute on payments.
	for i := 0; i < 50; i++ {
		logs = append(logs, store.RequestLog{TS: time.Now().Add(-90 * time.Minute).Truncate(time.Minute).Add(time.Duration(i) * time.Second), NodeID: "n", RequestID: fmt.Sprintf("x%d", i), APIID: &id, APIName: "payments", ClientIP: "203.0.113.9", Method: "GET", Path: "/payments", Status: 200, LatencyMS: 20})
	}
	if err := st.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	ex, err := st.RateLimitExposure(ctx, pay.ID, 30, 24*time.Hour)
	if err != nil || ex.PeakPerClientMinute < 50 || ex.WouldRefuse < 20 || ex.ClientsAffected < 1 {
		t.Fatalf("rate limit exposure: %+v %v", ex, err)
	}
	if ex, _ := st.RateLimitExposure(ctx, pay.ID, 5000, 24*time.Hour); ex.WouldRefuse != 0 {
		t.Fatalf("a generous limit should refuse nothing: %+v", ex)
	}
	srv := New(st, nil, nil, "test-admin-token", "cp", nil)
	o := orbitHarness{t: t, st: st, h: srv.Handler()}
	code, body := o.call(http.MethodGet, "/api/orbit/signals", "test-admin-token", nil)
	if code != http.StatusOK {
		t.Fatalf("signals: %d %v", code, body)
	}
	byID := map[string]map[string]any{}
	for _, s := range body["signals"].([]any) {
		m := s.(map[string]any)
		byID[m["id"].(string)] = m
	}
	spike := byID["errors:"+pay.ID]
	if spike == nil || spike["severity"] != "critical" || !strings.Contains(spike["title"].(string), "25.0%") {
		t.Fatalf("error spike not flagged: %v", body["signals"])
	}
	if newSpike := byID["errors:"+fresh.ID]; newSpike == nil || newSpike["severity"] != "critical" || !strings.Contains(newSpike["detail"].(string), "no earlier traffic") {
		t.Fatalf("new failing API not flagged: %v", body["signals"])
	}
	if open := byID["exposure:open"]; open == nil || !strings.Contains(open["detail"].(string), "open-catalog") || strings.Contains(open["detail"].(string), "payments") {
		t.Fatalf("exposure signal wrong: %v", open)
	}
	// Signals work without a model: they are rules, not AI.
	if srv.orbit != nil {
		t.Fatal("test server unexpectedly has a model")
	}
}

func TestProposalTokenExpiry(t *testing.T) {
	s := &Server{token: "k"}
	tok := s.signProposal(proposalClaims{APIID: "a", Set: map[string]any{"enabled": false}, User: "u", Expires: time.Now().Add(-time.Second).Unix()})
	if _, err := s.verifyProposal(tok); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token accepted: %v", err)
	}
	other := &Server{token: "different"}
	tok = s.signProposal(proposalClaims{APIID: "a", User: "u", Expires: time.Now().Add(time.Minute).Unix()})
	if _, err := other.verifyProposal(tok); err == nil {
		t.Fatal("token signed with another key accepted")
	}
}
