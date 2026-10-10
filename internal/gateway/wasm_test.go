package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relayops/apim/internal/store"
)

// processRequestWasm is a minimal module exporting process_request() -> i32
// that returns code (200 allows; 4xx/5xx refuse with that status).
func processRequestWasm(code int) []byte {
	b1, b2 := byte(0xc8), byte(0x01) // LEB128 200
	if code == 403 {
		b1, b2 = 0x93, 0x03 // LEB128 403
	}
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x13, 0x01, 0x0f,
		'p', 'r', 'o', 'c', 'e', 's', 's', '_', 'r', 'e', 'q', 'u', 'e', 's', 't',
		0x00, 0x00,
		0x0a, 0x07, 0x01, 0x05, 0x00, 0x41, b1, b2, 0x0b,
	}
}

func TestWasmPluginChainOnRequestPath(t *testing.T) {
	dir := t.TempDir()
	for name, code := range map[string]int{"allow-all": 200, "block-all": 403} {
		if err := os.WriteFile(filepath.Join(dir, name+".wasm"), processRequestWasm(code), 0600); err != nil {
			t.Fatal(err)
		}
	}
	mgr, names, err := LoadWasmPlugins(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if len(names) != 2 {
		t.Fatalf("loaded %v", names)
	}

	up := newUpstream(t, 200)
	g := newTestGateway(t)
	g.SetWasmManager(mgr)
	allowed := testAPI("allowed", "/allowed", up.srv.URL)
	allowed.TrafficPolicy.WasmPlugins = []string{"allow-all"}
	blocked := testAPI("blocked", "/blocked", up.srv.URL)
	blocked.TrafficPolicy.WasmPlugins = []string{"allow-all", "block-all"}
	missing := testAPI("missing", "/missing", up.srv.URL)
	missing.TrafficPolicy.WasmPlugins = []string{"not-deployed"}
	g.load(t, store.SnapshotData{APIs: []store.API{allowed, blocked, missing}})

	if rec := do(g, "GET", "/allowed/x", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("allowed by plugin: %d %s", rec.Code, rec.Body)
	}
	before := up.hits.Load()
	if rec := do(g, "GET", "/blocked/x", nil, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("blocked by the second plugin: %d %s", rec.Code, rec.Body)
	}
	if rec := do(g, "GET", "/missing/x", nil, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("plugin not loaded on this node must fail closed: %d %s", rec.Code, rec.Body)
	}
	if up.hits.Load() != before {
		t.Fatal("a refused request reached the upstream")
	}

	// A gateway with no plugins loaded refuses APIs that require one.
	bare := newTestGateway(t)
	bare.load(t, store.SnapshotData{APIs: []store.API{allowed}})
	if rec := do(bare, "GET", "/allowed/x", nil, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("gateway without plugins: %d", rec.Code)
	}
}

func TestWasmPluginNamesValidated(t *testing.T) {
	p := store.TrafficPolicy{WasmPlugins: []string{"../etc/passwd"}}
	p.Normalize()
	if err := p.Validate(); err == nil {
		t.Fatal("path-like plugin name accepted")
	}
	p = store.TrafficPolicy{WasmPlugins: []string{"waf-v2", "header_rewrite"}}
	p.Normalize()
	if err := p.Validate(); err != nil {
		t.Fatalf("valid names rejected: %v", err)
	}

	for name, bad := range map[string]store.TrafficPolicy{
		"config for an unlisted plugin": {WasmPlugins: []string{"a"}, WasmConfig: map[string]json.RawMessage{"b": json.RawMessage(`{}`)}},
		"config that is not an object":  {WasmPlugins: []string{"a"}, WasmConfig: map[string]json.RawMessage{"a": json.RawMessage(`[1]`)}},
		"oversized config":              {WasmPlugins: []string{"a"}, WasmConfig: map[string]json.RawMessage{"a": json.RawMessage(`{"x":"` + strings.Repeat("y", store.MaxWasmConfigBytes) + `"}`)}},
		"body limit over 1 MiB":         {WasmPlugins: []string{"a"}, WasmBodyLimitBytes: store.MaxWasmBodyBytes + 1},
		"negative body limit":           {WasmPlugins: []string{"a"}, WasmBodyLimitBytes: -1},
	} {
		bad.Normalize()
		if bad.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	ok := store.TrafficPolicy{WasmPlugins: []string{"a"}, WasmConfig: map[string]json.RawMessage{"a": json.RawMessage(`{"k":1}`)}, WasmBodyLimitBytes: 4096}
	ok.Normalize()
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid plugin settings rejected: %v", err)
	}
}

// A plugin that needs the body refuses requests on an API that does not
// grant body access, and nothing reaches the upstream.
func TestWasmPluginBodyRewriteNeedsBodyAccess(t *testing.T) {
	dir := buildPluginKit(t, "pii-redactor")
	mgr, _, err := LoadWasmPlugins(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	up := newUpstream(t, 200)
	g := newTestGateway(t)
	g.SetWasmManager(mgr)
	api := testAPI("nobody", "/nobody", up.srv.URL)
	api.TrafficPolicy.WasmPlugins = []string{"pii-redactor"} // no wasm_body_limit_bytes
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	if rec := do(g, "POST", "/nobody/x", nil, "a@b.io"); rec.Code != http.StatusInternalServerError || up.hits.Load() != 0 {
		t.Fatalf("plugin without body access: %d %s (upstream hits %d)", rec.Code, rec.Body, up.hits.Load())
	}
}
