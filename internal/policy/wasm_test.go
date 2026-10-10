package policy

import (
	"context"
	"net/http"
	"testing"
)

// buildMinimalProcessRequestWasm creates valid Wasm bytecode that exports "process_request" returning retCode.
func buildMinimalProcessRequestWasm(retCode int) []byte {
	// LEB128 encoding for 200: 0xc8, 0x01
	// LEB128 encoding for 403: 0x93, 0x03
	codeByte1 := byte(0xc8)
	codeByte2 := byte(0x01)
	if retCode == 403 {
		codeByte1 = byte(0x93)
		codeByte2 = byte(0x03)
	}

	wasm := []byte{
		// Wasm Magic & Version
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		// Type section: 1 type (func() -> i32)
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
		// Function section: 1 func using type 0
		0x03, 0x02, 0x01, 0x00,
		// Export section: "process_request" as func 0
		0x07, 0x13, 0x01, 0x0f,
		'p', 'r', 'o', 'c', 'e', 's', 's', '_', 'r', 'e', 'q', 'u', 'e', 's', 't',
		0x00, 0x00,
		// Code section: 1 body
		0x0a, 0x07, 0x01, 0x05, 0x00, 0x41, codeByte1, codeByte2, 0x0b,
	}
	return wasm
}

func TestWasmManagerLifecycleAndExecution(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewWasmManager(ctx)
	if err != nil {
		t.Fatalf("failed to create WasmManager: %v", err)
	}
	defer mgr.Close()

	// 1. Compile and register an allow plugin
	allowWasm := buildMinimalProcessRequestWasm(200)
	allowPlugin, err := mgr.RegisterPlugin(ctx, "allow-filter", allowWasm)
	if err != nil {
		t.Fatalf("failed to register allow plugin: %v", err)
	}
	if allowPlugin == nil {
		t.Fatal("expected non-nil allow plugin")
	}

	// 2. Compile and register a deny plugin (returns 403)
	denyWasm := buildMinimalProcessRequestWasm(403)
	denyPlugin, err := mgr.RegisterPlugin(ctx, "deny-waf-filter", denyWasm)
	if err != nil {
		t.Fatalf("failed to register deny plugin: %v", err)
	}
	if denyPlugin == nil {
		t.Fatal("expected non-nil deny plugin")
	}

	// 3. Verify lookup
	p, ok := mgr.GetPlugin("allow-filter")
	if !ok || p != allowPlugin {
		t.Fatal("failed to retrieve registered plugin by name")
	}

	// 4. Execute Allow Plugin
	req := Request{
		Method:   "GET",
		Path:     "/test/resource",
		ClientIP: "192.168.1.100",
		Header:   http.Header{"User-Agent": []string{"curl/7.88"}},
	}
	res, err := allowPlugin.Execute(ctx, req)
	if err != nil {
		t.Fatalf("allow plugin execution failed: %v", err)
	}
	if res.Action != ActionAllow {
		t.Fatalf("expected ActionAllow, got %s (status %d)", res.Action, res.Status)
	}

	// 5. Execute Deny Plugin
	denyRes, err := denyPlugin.Execute(ctx, req)
	if err != nil {
		t.Fatalf("deny plugin execution failed: %v", err)
	}
	if denyRes.Action != ActionDeny {
		t.Fatalf("expected ActionDeny, got %s", denyRes.Action)
	}
	if denyRes.Status != 403 {
		t.Fatalf("expected status 403, got %d", denyRes.Status)
	}
}
