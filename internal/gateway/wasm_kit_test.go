package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relayops/apim/internal/policy"
	"github.com/relayops/apim/internal/store"
)

// buildPluginKit compiles the plugins in plugins/wasm with the Go toolchain
// (GOOS=wasip1, reactor build mode) into a temporary plugins directory.
func buildPluginKit(t *testing.T, names ...string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds WASM plugins with the Go toolchain")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	repo, _ := filepath.Abs("../..")
	dir := t.TempDir()
	// One build at a time: each links a WASM runtime and they are memory hungry.
	for _, n := range names {
		cmd := exec.Command(goBin, "build", "-buildmode=c-shared", "-trimpath", "-o", filepath.Join(dir, n+".wasm"), "./plugins/wasm/"+n)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v: %s", n, err, out)
		}
	}
	return dir
}

func rawJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func TestWasmPluginKitThroughGateway(t *testing.T) {
	dir := buildPluginKit(t, "header-rewrite", "ip-allowlist", "request-validator", "pii-redactor", "hmac-auth")
	mgr, names, err := LoadWasmPlugins(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if len(names) != 5 {
		t.Fatalf("loaded %v", names)
	}

	up := newUpstream(t, 200)
	g := newTestGateway(t)
	g.SetWasmManager(mgr)

	headers := testAPI("headers", "/headers", up.srv.URL)
	headers.TrafficPolicy.WasmPlugins = []string{"header-rewrite"}
	headers.TrafficPolicy.WasmConfig = map[string]json.RawMessage{"header-rewrite": rawJSON(map[string]any{
		"set": map[string]string{"X-Env": "production"}, "remove": []string{"X-Debug"}})}

	ips := testAPI("ips", "/ips", up.srv.URL)
	ips.TrafficPolicy.WasmPlugins = []string{"ip-allowlist"}
	ips.TrafficPolicy.WasmConfig = map[string]json.RawMessage{"ip-allowlist": rawJSON(map[string]any{"allow": []string{"10.0.0.0/8"}})}

	orders := testAPI("orders", "/orders", up.srv.URL)
	orders.TrafficPolicy.WasmPlugins = []string{"request-validator", "pii-redactor"}
	orders.TrafficPolicy.WasmBodyLimitBytes = 4096
	orders.TrafficPolicy.WasmConfig = map[string]json.RawMessage{"request-validator": rawJSON(map[string]any{
		"content_types": []string{"application/json"}, "json_required": []string{"customer"}})}

	const secret = "partner-a-shared-secret-0123456789"
	hooks := testAPI("hooks", "/hooks", up.srv.URL)
	hooks.TrafficPolicy.WasmPlugins = []string{"hmac-auth"}
	hooks.TrafficPolicy.WasmBodyLimitBytes = 4096
	hooks.TrafficPolicy.WasmConfig = map[string]json.RawMessage{"hmac-auth": rawJSON(map[string]any{"keys": map[string]string{"partner-a": secret}})}

	for _, api := range []store.API{headers, ips, orders, hooks} {
		api.TrafficPolicy.Normalize()
		if err := api.TrafficPolicy.Validate(); err != nil {
			t.Fatalf("%s policy: %v", api.Name, err)
		}
	}
	g.load(t, store.SnapshotData{APIs: []store.API{headers, ips, orders, hooks}})

	// header-rewrite
	if rec := do(g, "GET", "/headers/x", map[string]string{"X-Debug": "1"}, ""); rec.Code != 200 {
		t.Fatalf("headers: %d %s", rec.Code, rec.Body)
	}
	if up.last.Header.Get("X-Env") != "production" || up.last.Header.Get("X-Debug") != "" {
		t.Fatalf("upstream headers %v", up.last.Header)
	}

	// ip-allowlist: httptest requests come from 192.0.2.1.
	if rec := do(g, "GET", "/ips/x", nil, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "ip_not_allowed") {
		t.Fatalf("ip-allowlist: %d %s", rec.Code, rec.Body)
	}

	// request-validator then pii-redactor: the upstream receives the redacted body.
	ct := map[string]string{"Content-Type": "application/json"}
	if rec := do(g, "POST", "/orders/new", ct, `{"note":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("validator: %d %s", rec.Code, rec.Body)
	}
	before := up.hits.Load()
	if rec := do(g, "POST", "/orders/new", ct, `{"customer":"Ann","email":"ann@example.com","card":"4111-1111-1111-1111"}`); rec.Code != 200 {
		t.Fatalf("redacted order: %d %s", rec.Code, rec.Body)
	}
	if up.hits.Load() != before+1 || strings.Contains(up.body, "ann@example.com") || strings.Contains(up.body, "4111") ||
		!strings.Contains(up.body, "[REDACTED:email]") || up.last.Header.Get("X-RelayOps-PII-Redacted") != "email:1,credit_card:1" {
		t.Fatalf("upstream got body %q headers %v", up.body, up.last.Header)
	}
	if got := up.last.ContentLength; got != int64(len(up.body)) {
		t.Fatalf("content length %d for a %d byte body", got, len(up.body))
	}
	if rec := do(g, "POST", "/orders/new", ct, `{"customer":"`+strings.Repeat("a", 5000)+`"}`); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body over wasm_body_limit_bytes: %d", rec.Code)
	}

	// hmac-auth
	body := `{"event":"paid"}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sum := sha256.Sum256([]byte(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("POST\n/hooks/pay\n" + ts + "\n" + hex.EncodeToString(sum[:])))
	sig := hex.EncodeToString(mac.Sum(nil))
	signed := map[string]string{"X-Key-Id": "partner-a", "X-Timestamp": ts, "X-Signature": sig}
	if rec := do(g, "POST", "/hooks/pay", signed, body); rec.Code != 200 {
		t.Fatalf("signed webhook: %d %s", rec.Code, rec.Body)
	}
	if up.last.Header.Get("X-Authenticated-Key-Id") != "partner-a" || up.last.Header.Get("X-Signature") != "" || up.body != body {
		t.Fatalf("upstream after hmac-auth: %v %q", up.last.Header, up.body)
	}
	if rec := do(g, "POST", "/hooks/pay", signed, `{"event":"refund"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered webhook: %d %s", rec.Code, rec.Body)
	}
}

// BenchmarkWasmPluginExecute measures one pooled plugin call (warm instance).
func BenchmarkWasmPluginExecute(b *testing.B) {
	dir := b.TempDir()
	repo, _ := filepath.Abs("../..")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", filepath.Join(dir, "ip-allowlist.wasm"), "./plugins/wasm/ip-allowlist")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if out, err := cmd.CombinedOutput(); err != nil {
		b.Fatal(string(out))
	}
	mgr, _, err := LoadWasmPlugins(context.Background(), dir)
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()
	p, _ := mgr.GetPlugin("ip-allowlist")
	req := policy.Request{Method: "GET", Path: "/x", ClientIP: "10.1.2.3", Header: http.Header{"Accept": {"*/*"}},
		PluginConfig: rawJSON(map[string]any{"allow": []string{"10.0.0.0/8"}})}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if res, err := p.Execute(context.Background(), req); err != nil || res.Action != "allow" {
				b.Fatalf("%+v %v", res, err)
			}
		}
	})
}
