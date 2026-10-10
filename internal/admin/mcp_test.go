package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/relayops/apim/internal/mcp"
)

func TestAdminMCPIntegration(t *testing.T) {
	static := os.DirFS("../../web/static")
	srv := New(nil, nil, nil, "admin-secret-token", "node-1", static)
	handler := srv.Handler()

	// 1. Test POST /mcp/message with initialize
	initPayload := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test-ide","version":"1.0"}}}`
	req := httptest.NewRequest("POST", "/mcp/message", strings.NewReader(initPayload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp mcp.JSONRPCResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal JSON-RPC response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", resp.Error)
	}

	// 2. Test tools/list with Admin token
	listPayload := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	adminReq := httptest.NewRequest("POST", "/mcp/message", strings.NewReader(listPayload))
	adminReq.Header.Set("Content-Type", "application/json")
	adminReq.Header.Set("Authorization", "Bearer admin-secret-token")
	wAdmin := httptest.NewRecorder()

	handler.ServeHTTP(wAdmin, adminReq)
	if wAdmin.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wAdmin.Code)
	}

	var adminResp mcp.JSONRPCResponse
	if err := json.Unmarshal(wAdmin.Body.Bytes(), &adminResp); err != nil {
		t.Fatalf("failed to unmarshal JSON-RPC response: %v", err)
	}
	toolsMap := adminResp.Result.(map[string]any)["tools"].([]any)
	hasPlan := false
	for _, toolItem := range toolsMap {
		tool := toolItem.(map[string]any)
		if tool["name"] == "relayops_plan" {
			hasPlan = true
			break
		}
	}
	if !hasPlan {
		t.Errorf("admin tools/list must include relayops_plan")
	}

	// 3. Test tools/list anonymously / developer
	devReq := httptest.NewRequest("POST", "/mcp/message", strings.NewReader(listPayload))
	devReq.Header.Set("Content-Type", "application/json")
	wDev := httptest.NewRecorder()

	handler.ServeHTTP(wDev, devReq)
	if wDev.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wDev.Code)
	}

	var devResp mcp.JSONRPCResponse
	if err := json.Unmarshal(wDev.Body.Bytes(), &devResp); err != nil {
		t.Fatalf("failed to unmarshal JSON-RPC response: %v", err)
	}
	devToolsMap := devResp.Result.(map[string]any)["tools"].([]any)
	for _, toolItem := range devToolsMap {
		tool := toolItem.(map[string]any)
		if tool["name"] == "relayops_plan" {
			t.Errorf("non-admin tools/list must NOT include relayops_plan")
		}
	}
}
