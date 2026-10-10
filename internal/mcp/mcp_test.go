package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPInitialize(t *testing.T) {
	engine := NewEngine(nil, "http://localhost:8080", "secret-admin")
	caller := CallerContext{Role: RoleDeveloper}

	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
	}

	resp := engine.HandleRequest(context.Background(), caller, req)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	res, ok := resp.Result.(InitializeResult)
	if !ok {
		t.Fatalf("expected InitializeResult, got %T", resp.Result)
	}

	if res.ServerInfo.Name != "relayops-mcp" {
		t.Errorf("expected server name 'relayops-mcp', got %s", res.ServerInfo.Name)
	}
	if res.Capabilities.Tools == nil {
		t.Errorf("expected tools capability")
	}
}

func TestMCPRoleAdaptiveTools(t *testing.T) {
	engine := NewEngine(nil, "http://localhost:8080", "secret-admin")

	// 1. Developer Role
	devCaller := CallerContext{Role: RoleDeveloper}
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/list",
	}
	devResp := engine.HandleRequest(context.Background(), devCaller, req)
	if devResp.Error != nil {
		t.Fatalf("dev tools/list error: %v", devResp.Error)
	}
	devToolsMap := devResp.Result.(map[string]any)["tools"].([]Tool)
	toolNames := map[string]bool{}
	for _, tool := range devToolsMap {
		toolNames[tool.Name] = true
	}

	if !toolNames["list_apis"] || !toolNames["get_api_spec"] || !toolNames["invoke_api"] || !toolNames["get_my_usage"] {
		t.Errorf("developer should have catalog tools, got: %v", toolNames)
	}
	if toolNames["relayops_plan"] || toolNames["relayops_apply"] {
		t.Errorf("developer should NOT have operator tools, got: %v", toolNames)
	}

	// 2. Admin Role
	adminCaller := CallerContext{Role: RoleAdmin}
	adminResp := engine.HandleRequest(context.Background(), adminCaller, req)
	if adminResp.Error != nil {
		t.Fatalf("admin tools/list error: %v", adminResp.Error)
	}
	adminToolsMap := adminResp.Result.(map[string]any)["tools"].([]Tool)
	adminToolNames := map[string]bool{}
	for _, tool := range adminToolsMap {
		adminToolNames[tool.Name] = true
	}

	if !adminToolNames["relayops_plan"] || !adminToolNames["relayops_apply"] || !adminToolNames["relayops_rollout_status"] {
		t.Errorf("admin should have operator tools, got: %v", adminToolNames)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestMCPInvokeAPI(t *testing.T) {
	mockClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("X-API-Key") != "rk_test_12345" {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(strings.NewReader("Unauthorized")),
					Header:     make(http.Header),
				}, nil
			}
			if r.URL.Path == "/v1/weather" {
				h := make(http.Header)
				h.Set("Content-Type", "application/json")
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"temperature": 22.5, "unit": "celsius"}`)),
					Header:     h,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("Not Found")),
				Header:     make(http.Header),
			}, nil
		}),
	}

	engine := NewEngine(nil, "http://gateway.mock", "admin-token", WithHTTPClient(mockClient))
	caller := CallerContext{
		Role:       RoleDeveloper,
		APIKey:     "rk_test_12345",
		ConsumerID: "c-123",
	}

	callReq := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "invoke_api",
			"arguments": {
				"method": "GET",
				"path": "/v1/weather"
			}
		}`),
	}

	resp := engine.HandleRequest(context.Background(), caller, callReq)
	if resp.Error != nil {
		t.Fatalf("tool call error: %v", resp.Error)
	}

	callRes := resp.Result.(CallToolResult)
	if callRes.IsError {
		t.Fatalf("tool execution reported error: %v", callRes.Content)
	}

	if len(callRes.Content) == 0 {
		t.Fatalf("empty content")
	}

	text := callRes.Content[0].Text
	if !strings.Contains(text, "temperature") || !strings.Contains(text, "22.5") {
		t.Errorf("expected response to contain temperature data, got: %s", text)
	}
}

func TestMCPStdioTransport(t *testing.T) {
	engine := NewEngine(nil, "http://localhost:8080", "admin-token")
	caller := CallerContext{Role: RoleDeveloper}

	input := `{"jsonrpc":"2.0","id":10,"method":"ping"}` + "\n"
	in := strings.NewReader(input)
	out := &bytes.Buffer{}
	log := &bytes.Buffer{}

	err := RunStdio(context.Background(), engine, caller, in, out, log)
	if err != nil {
		t.Fatalf("RunStdio failed: %v", err)
	}

	output := strings.TrimSpace(out.String())
	var resp JSONRPCResponse
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		t.Fatalf("failed to parse output: %v, output: %s", err, output)
	}

	if resp.ID != float64(10) {
		t.Errorf("expected id 10, got %v", resp.ID)
	}
}

func TestMCPSSEHandler(t *testing.T) {
	engine := NewEngine(nil, "http://localhost:8080", "admin-token")
	handler := NewHandler(engine)

	// Direct message POST
	msgReq := httptest.NewRequest("POST", "/mcp/message", strings.NewReader(`{"jsonrpc":"2.0","id":42,"method":"ping"}`))
	msgReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleMessage(w, msgReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp JSONRPCResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ID != float64(42) {
		t.Errorf("expected id 42, got %v", resp.ID)
	}
}
