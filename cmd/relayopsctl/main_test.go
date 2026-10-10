package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestYAMLDocumentsBecomeJSON(t *testing.T) {
	y := []byte(`
format_version: "1.0"
plans:
  - name: gold
    rate_limit_per_minute: 600
apis:
  - name: orders
    base_path: /orders
    upstream_url: http://orders:8080
    traffic_policy:
      targets:
        - url: http://o1
        - url: http://o2
          weight: 0
      retries: {attempts: 3}
`)
	out, err := toJSON(y, "doc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	apis := doc["apis"].([]any)
	targets := apis[0].(map[string]any)["traffic_policy"].(map[string]any)["targets"].([]any)
	if doc["format_version"] != "1.0" || len(targets) != 2 || targets[1].(map[string]any)["weight"] != float64(0) {
		t.Fatalf("converted = %s", out)
	}
	if j, _ := toJSON([]byte(`  {"format_version":"1.0"}`), "x.json"); string(j) != `{"format_version":"1.0"}` {
		t.Fatalf("JSON passthrough = %s", j)
	}
	if _, err := toJSON([]byte("a: [unclosed"), "bad.yaml"); err == nil {
		t.Fatal("invalid YAML must error")
	}
}

func TestFlagsAfterPositionalArgs(t *testing.T) {
	o, err := parse("rollout", []string{"canary", "12", "--traffic-percent", "15", "--canary-header", "X-Canary", "--server", "http://cp:9090"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(o.args, ",") != "canary,12" || o.trafficPercent != 15 || o.canaryHeader != "X-Canary" || o.server != "http://cp:9090" {
		t.Fatalf("parsed = %+v", o)
	}
	if _, err := parse("plan", []string{"--output", "yaml"}); err == nil {
		t.Fatal("unsupported output format must error")
	}
}

// fakeControlPlane records requests and returns canned plan/apply responses.
type fakeControlPlane struct {
	srv      *httptest.Server
	requests []string
	plan     map[string]any
}

func newFake(t *testing.T, plan map[string]any) *fakeControlPlane {
	f := &fakeControlPlane{plan: plan}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"unauthorized","message":"missing or invalid admin bearer token"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.requests = append(f.requests, r.Method+" "+r.URL.String()+" "+string(body))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/system/plan"):
			if valid, _ := f.plan["valid"].(bool); !valid {
				w.WriteHeader(422)
			}
			json.NewEncoder(w).Encode(f.plan)
		case strings.HasPrefix(r.URL.Path, "/api/system/apply"):
			json.NewEncoder(w).Encode(map[string]any{"revision": 9, "applied": true, "rollout": r.URL.Query().Get("rollout"), "message": "applied as revision 9."})
		case r.URL.Path == "/api/system/export":
			w.Write([]byte(`{"format_version":"1.0","apis":[{"name":"a","base_path":"/a","upstream_url":"http://a"}],"plans":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func changedPlan() map[string]any {
	return map[string]any{
		"valid": true, "has_changes": true, "plan_hash": "abc123",
		"summary": map[string]any{"Create": 1, "Update": 1},
		"changes": []any{
			map[string]any{"type": "api", "name": "orders", "action": "update",
				"changes":         []any{map[string]any{"field": "upstream_url", "before": "http://a", "after": "http://b"}},
				"consumer_impact": map[string]any{"breaking_changes": []any{"Authentication scheme changing"}, "total_impacted_applications": 2},
				"replay":          map[string]any{"replayed": 50, "status_changed": 4},
				"warnings":        []any{"4 of 50 recent requests would get a different status"}},
			map[string]any{"type": "plan", "name": "gold", "action": "create"},
		},
	}
}

func writeDoc(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "relayops.yaml")
	os.WriteFile(p, []byte("format_version: \"1.0\"\napis: []\nplans: []\n"), 0o644)
	return p
}

func TestPlanDetailedExitCodeAndRendering(t *testing.T) {
	f := newFake(t, changedPlan())
	doc := writeDoc(t)
	var out bytes.Buffer
	code, err := run([]string{"plan", "-f", doc, "--server", f.srv.URL, "--token", "tok", "--detailed-exitcode", "--prune"}, &out, nil)
	if err != nil || code != 2 {
		t.Fatalf("code=%d err=%v, want 2 (changes present)", code, err)
	}
	text := out.String()
	for _, want := range []string{`~ api "orders" (update)`, `upstream_url: "http://a" -> "http://b"`, "breaking: Authentication scheme", "2 consumer application(s)", "replay: 4 of 50", "Plan hash: abc123"} {
		if !strings.Contains(text, want) {
			t.Errorf("text plan missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(f.requests[0], "prune=true") || !strings.Contains(f.requests[0], `"format_version":"1.0"`) {
		t.Fatalf("request = %s (YAML must be sent as JSON with prune)", f.requests[0])
	}

	out.Reset()
	if _, err := run([]string{"plan", "-f", doc, "--server", f.srv.URL, "--token", "tok", "--output", "markdown"}, &out, nil); err != nil {
		t.Fatal(err)
	}
	md := out.String()
	if !strings.Contains(md, "| update | api `orders` |") || !strings.Contains(md, "**1 to create, 1 to update") || strings.Count(md, "recent requests") != 1 {
		t.Fatalf("markdown:\n%s", md)
	}
}

func TestPlanInvalidDocumentFails(t *testing.T) {
	f := newFake(t, map[string]any{"valid": false, "errors": []any{`apis[0] "x": base_path must start with '/'`}})
	var out bytes.Buffer
	code, err := run([]string{"plan", "-f", writeDoc(t), "--server", f.srv.URL, "--token", "tok"}, &out, nil)
	if code != 1 || err == nil || !strings.Contains(out.String(), "base_path must start") {
		t.Fatalf("code=%d err=%v out=%s", code, err, out.String())
	}
}

func TestApplyRequiresConfirmationAndPinsPlanHash(t *testing.T) {
	f := newFake(t, changedPlan())
	doc := writeDoc(t)
	var out bytes.Buffer
	code, err := run([]string{"apply", "-f", doc, "--server", f.srv.URL, "--token", "tok"}, &out, strings.NewReader("no\n"))
	if code != 1 || err == nil || len(f.requests) != 1 {
		t.Fatalf("declined apply must not call apply: code=%d err=%v requests=%v", code, err, f.requests)
	}
	out.Reset()
	code, err = run([]string{"apply", "-f", doc, "--server", f.srv.URL, "--token", "tok", "--canary", "--traffic-percent", "10", "--canary-header", "X-Canary"}, &out, strings.NewReader("yes\n"))
	if code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	last := f.requests[len(f.requests)-1]
	for _, want := range []string{"plan_hash=abc123", "rollout=canary", "traffic_percent=10", "canary_header=X-Canary"} {
		if !strings.Contains(last, want) {
			t.Errorf("apply request missing %s: %s", want, last)
		}
	}
	if !strings.Contains(out.String(), "rollout promote 9") {
		t.Fatalf("canary guidance missing: %s", out.String())
	}
}

func TestExportYAMLAndAuthErrors(t *testing.T) {
	f := newFake(t, changedPlan())
	path := filepath.Join(t.TempDir(), "live.yaml")
	var out bytes.Buffer
	if code, err := run([]string{"export", "-o", path, "--server", f.srv.URL, "--token", "tok"}, &out, nil); code != 0 || err != nil {
		t.Fatalf("export: %d %v", code, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "format_version: \"1.0\"") || !strings.Contains(string(b), "base_path: /a") {
		t.Fatalf("yaml export:\n%s", b)
	}
	_, err := run([]string{"export", "--server", f.srv.URL, "--token", "wrong"}, &out, nil)
	if err == nil || !strings.Contains(err.Error(), "unauthorized (HTTP 401)") {
		t.Fatalf("auth error = %v", err)
	}
}

func TestTenantHeaderSent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-RelayOps-Tenant")
		w.Write([]byte(`{"format_version":"1.0","apis":[],"plans":[]}`))
	}))
	defer srv.Close()
	var out bytes.Buffer
	if _, err := run([]string{"export", "--server", srv.URL, "--token", "t", "--tenant", "acme"}, &out, nil); err != nil {
		t.Fatal(err)
	}
	if got != "acme" {
		t.Fatalf("tenant header = %q", got)
	}
}

func TestMaintenancePurgeCommand(t *testing.T) {
	var requestedURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedURI = r.URL.RequestURI()
		if r.Header.Get("Authorization") != "Bearer test-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"status": "ok",
			"message": "Maintenance telemetry purge completed successfully",
			"older_than": "48h0m0s",
			"logs_purged": 120,
			"test_studio_purged": 45,
			"sessions_purged": 3,
			"vacuum_executed": true,
			"purged_at": "2026-10-08T12:00:00Z"
		}`))
	}))
	defer srv.Close()

	var out bytes.Buffer
	code, err := run([]string{"maintenance", "purge", "--server", srv.URL, "--token", "test-tok", "--older-than", "48h", "--vacuum"}, &out, nil)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v, want 0", code, err)
	}

	if !strings.Contains(requestedURI, "older_than=48h") || !strings.Contains(requestedURI, "vacuum=true") {
		t.Fatalf("unexpected request URI: %s", requestedURI)
	}

	output := out.String()
	for _, want := range []string{"Maintenance Purge Complete", "Older Than: 48h0m0s", "Logs Purged: 120", "Test Studio Records Purged: 45", "Expired Sessions Purged: 3", "VACUUM Executed: true"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}

	// Test JSON output mode
	out.Reset()
	code, err = run([]string{"maintenance", "purge", "--server", srv.URL, "--token", "test-tok", "--output", "json"}, &out, nil)
	if err != nil || code != 0 {
		t.Fatalf("json code=%d err=%v, want 0", code, err)
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("invalid json output: %v", err)
	}
	if res["status"] != "ok" || res["logs_purged"] != float64(120) {
		t.Fatalf("unexpected json content: %v", res)
	}
}
