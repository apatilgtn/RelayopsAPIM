package gateway

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relayops/apim/internal/store"
)

func TestWasmResponsePhase(t *testing.T) {
	dir := buildPluginKit(t, "pii-redactor", "header-rewrite")
	mgr, _, err := LoadWasmPlugins(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	for _, n := range []string{"pii-redactor", "header-rewrite"} {
		if p, _ := mgr.GetPlugin(n); !p.HandlesResponses() || !p.HandlesRequests() {
			t.Fatalf("%s phases not detected", n)
		}
	}

	// The upstream answers with personal data, a Server header, and (when the
	// client accepts it) gzip.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "legacy-app/1.0")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: contact bob@example.com\n\n")
			return
		}
		body := `{"customer":"Ann","email":"ann@example.com","card":"4111 1111 1111 1111"}`
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			fmt.Fprint(zw, body)
			zw.Close()
			return
		}
		fmt.Fprint(w, body)
	}))
	defer up.Close()

	cfg := func(mode string) map[string]json.RawMessage {
		return map[string]json.RawMessage{
			"pii-redactor":   rawJSON(map[string]any{"scan": []string{"response"}, "mode": mode}),
			"header-rewrite": rawJSON(map[string]any{"response": map[string]any{"remove": []string{"Server"}, "set": map[string]string{"Cache-Control": "no-store"}}}),
		}
	}
	redact := testAPI("redact", "/r", up.URL)
	redact.TrafficPolicy.WasmPlugins = []string{"header-rewrite", "pii-redactor"}
	redact.TrafficPolicy.WasmResponseBodyLimitBytes = 64 << 10
	redact.TrafficPolicy.WasmConfig = cfg("redact")
	block := testAPI("block", "/b", up.URL)
	block.TrafficPolicy = redact.TrafficPolicy
	block.TrafficPolicy.WasmConfig = cfg("block")
	noBody := testAPI("nobody", "/n", up.URL)
	noBody.TrafficPolicy = redact.TrafficPolicy
	noBody.TrafficPolicy.WasmResponseBodyLimitBytes = 0
	for _, a := range []*store.API{&redact, &block, &noBody} {
		a.TrafficPolicy.Normalize()
		if err := a.TrafficPolicy.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	g := newTestGateway(t)
	g.SetWasmManager(mgr)
	g.load(t, store.SnapshotData{APIs: []store.API{redact, block, noBody}})

	// Redacted, even though the client asked for gzip (the gateway reads the
	// decoded body and the client gets it uncompressed).
	rec := do(g, "GET", "/r/customer", map[string]string{"Accept-Encoding": "gzip"}, "")
	got := rec.Body.String()
	if rec.Code != 200 || strings.Contains(got, "ann@example.com") || strings.Contains(got, "4111") || !strings.Contains(got, "[REDACTED:email]") {
		t.Fatalf("redacted response: %d %s", rec.Code, got)
	}
	if rec.Header().Get("X-RelayOps-PII-Redacted") != "email:1,credit_card:1" || rec.Header().Get("Server") != "" ||
		rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("response headers %v", rec.Header())
	}

	// A streamed response cannot be scanned: marked, or refused in block mode.
	if rec := do(g, "GET", "/r/stream", nil, ""); rec.Header().Get("X-RelayOps-PII-Unscanned") == "" || !strings.Contains(rec.Body.String(), "bob@") {
		t.Fatalf("streamed response: %v %s", rec.Header(), rec.Body)
	}
	if rec := do(g, "GET", "/b/customer", nil, ""); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "pii_in_response") {
		t.Fatalf("block mode: %d %s", rec.Code, rec.Body)
	}
	// Without response body access, the scan reports it could not look.
	if rec := do(g, "GET", "/n/customer", nil, ""); rec.Header().Get("X-RelayOps-PII-Unscanned") == "" || rec.Header().Get("Server") != "" {
		t.Fatalf("no body access: %v", rec.Header())
	}
}

// MCP tool results pass through response plugins after the MCP gateway, so
// personal data in a tool's output can be removed before the model sees it.
func TestWasmResponsePhaseOnMCPToolResults(t *testing.T) {
	dir := buildPluginKit(t, "pii-redactor")
	mgr, _, err := LoadWasmPlugins(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Owner: jane@corp.example, SSN 123-45-6789"}]}}`)
	}))
	defer up.Close()
	api := testAPI("mcp-pii", "/m", up.URL)
	api.Protocol = "mcp"
	api.MCPPolicy.Normalize()
	api.TrafficPolicy.WasmPlugins = []string{"pii-redactor"}
	api.TrafficPolicy.WasmResponseBodyLimitBytes = 64 << 10
	api.TrafficPolicy.WasmConfig = map[string]json.RawMessage{"pii-redactor": rawJSON(map[string]any{"scan": []string{"response"}})}
	g := newTestGateway(t)
	g.SetWasmManager(mgr)
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	rec, msgs := rpc(g, "/m", nil, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup"}}`)
	if rec.Code != 200 || len(msgs) != 1 || strings.Contains(rec.Body.String(), "jane@corp.example") || strings.Contains(rec.Body.String(), "123-45-6789") {
		t.Fatalf("tool result: %d %s", rec.Code, rec.Body)
	}
}
