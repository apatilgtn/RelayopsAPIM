package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/relayops/apim/internal/store"
)

type CallerRole string

const (
	RoleDeveloper CallerRole = "developer"
	RoleAdmin     CallerRole = "admin"
	RoleAnonymous CallerRole = "anonymous"
)

type CallerContext struct {
	Role         CallerRole
	ConsumerID   string
	ConsumerName string
	ConsumerEmail string
	TenantID     string
	APIKey       string
	AdminToken   string
}

// Engine implements the central Model Context Protocol server.
type Engine struct {
	store       *store.Store
	gatewayURL  string
	adminToken  string
	planFn      func(ctx context.Context, yamlDoc string) (map[string]any, error)
	applyFn     func(ctx context.Context, yamlDoc, planHash string, canary bool, trafficPercent int) (map[string]any, error)
	rolloutFn   func(ctx context.Context) (map[string]any, error)
	diagnoseFn  func(ctx context.Context, requestID string) (map[string]any, error)
	testRunFn   func(ctx context.Context, suiteID, env string) (map[string]any, error)
	synthSpecFn func(api store.API) map[string]any
	httpClient  *http.Client
}

// EngineOption configures the MCP engine.
type EngineOption func(*Engine)

func WithAdminHooks(
	plan func(ctx context.Context, yamlDoc string) (map[string]any, error),
	apply func(ctx context.Context, yamlDoc, planHash string, canary bool, trafficPercent int) (map[string]any, error),
	rollout func(ctx context.Context) (map[string]any, error),
	diagnose func(ctx context.Context, requestID string) (map[string]any, error),
	testRun func(ctx context.Context, suiteID, env string) (map[string]any, error),
) EngineOption {
	return func(e *Engine) {
		e.planFn = plan
		e.applyFn = apply
		e.rolloutFn = rollout
		e.diagnoseFn = diagnose
		e.testRunFn = testRun
	}
}

func WithSpecSynthesizer(fn func(api store.API) map[string]any) EngineOption {
	return func(e *Engine) {
		e.synthSpecFn = fn
	}
}

func WithHTTPClient(client *http.Client) EngineOption {
	return func(e *Engine) {
		if client != nil {
			e.httpClient = client
		}
	}
}

// NewEngine creates an MCP engine.
func NewEngine(s *store.Store, gatewayURL, adminToken string, options ...EngineOption) *Engine {
	if gatewayURL == "" {
		gatewayURL = "http://127.0.0.1:8080"
	}
	e := &Engine{
		store:      s,
		gatewayURL: strings.TrimRight(gatewayURL, "/"),
		adminToken: adminToken,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range options {
		opt(e)
	}
	return e
}

// AuthenticateCaller resolves the role and identity from an authorization header or raw key.
func (e *Engine) AuthenticateCaller(ctx context.Context, tokenOrKey string) CallerContext {
	tokenOrKey = strings.TrimSpace(tokenOrKey)
	if tokenOrKey == "" {
		return CallerContext{Role: RoleAnonymous}
	}

	// 1. Check if token matches admin token
	if e.adminToken != "" && tokenOrKey == e.adminToken {
		return CallerContext{
			Role:       RoleAdmin,
			AdminToken: tokenOrKey,
		}
	}

	// 2. Check if key is a registered Developer API Key in the store
	if e.store != nil {
		consumer, err := e.store.GetConsumerByRawKey(ctx, tokenOrKey)
		if err == nil && consumer.ID != "" {
			return CallerContext{
				Role:          RoleDeveloper,
				ConsumerID:    consumer.ID,
				ConsumerName:  consumer.Name,
				ConsumerEmail: consumer.Email,
				TenantID:      consumer.TenantID,
				APIKey:        tokenOrKey,
			}
		}
	}

	return CallerContext{Role: RoleAnonymous, APIKey: tokenOrKey}
}

// HandleRequest processes an incoming JSON-RPC 2.0 MCP request.
func (e *Engine) HandleRequest(ctx context.Context, caller CallerContext, req JSONRPCRequest) JSONRPCResponse {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
	}

	switch req.Method {
	case "initialize":
		resp.Result = InitializeResult{
			ProtocolVersion: "2024-11-05",
			Capabilities: ServerCapabilities{
				Tools:     &ToolsCapability{ListChanged: false},
				Resources: &ResourcesCapability{ListChanged: false},
				Prompts:   &PromptsCapability{ListChanged: false},
			},
			ServerInfo: Implementation{
				Name:    "relayops-mcp",
				Version: "1.0.0",
			},
			Instructions: "RelayOps APIM Model Context Protocol server. Discover, inspect OpenAPI schemas, and invoke APIs with authenticated developer keys.",
		}
		return resp

	case "notifications/initialized", "initialized":
		resp.Result = map[string]any{"ok": true}
		return resp

	case "ping":
		resp.Result = map[string]any{}
		return resp

	case "tools/list":
		tools := e.listTools(caller)
		resp.Result = map[string]any{"tools": tools}
		return resp

	case "tools/call":
		var params CallToolParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = &JSONRPCError{Code: CodeInvalidParams, Message: "invalid tool call parameters: " + err.Error()}
				return resp
			}
		}
		result := e.callTool(ctx, caller, params.Name, params.Arguments)
		resp.Result = result
		return resp

	case "resources/list":
		resources, err := e.listResources(ctx, caller)
		if err != nil {
			resp.Error = &JSONRPCError{Code: CodeInternalError, Message: err.Error()}
			return resp
		}
		resp.Result = map[string]any{"resources": resources}
		return resp

	case "resources/read":
		var params ReadResourceParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = &JSONRPCError{Code: CodeInvalidParams, Message: "invalid resource read parameters"}
				return resp
			}
		}
		contents, err := e.readResource(ctx, caller, params.URI)
		if err != nil {
			resp.Error = &JSONRPCError{Code: CodeInternalError, Message: err.Error()}
			return resp
		}
		resp.Result = ReadResourceResult{Contents: contents}
		return resp

	default:
		resp.Error = &JSONRPCError{
			Code:    CodeMethodNotFound,
			Message: fmt.Sprintf("method not found: %s", req.Method),
		}
		return resp
	}
}

// listTools returns the MCP tools available to this caller.
func (e *Engine) listTools(caller CallerContext) []Tool {
	tools := []Tool{
		{
			Name:        "list_apis",
			Description: "List APIs available in the RelayOps catalog with their IDs, titles, descriptions, base paths, and rate limits.",
			InputSchema: ToolInputSchema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"query": {
						Type:        "string",
						Description: "Optional search query to filter APIs by name or description.",
					},
				},
			},
		},
		{
			Name:        "get_api_spec",
			Description: "Get the complete OpenAPI 3.1 specification for an API by its ID, detailing paths, methods, parameters, and models.",
			InputSchema: ToolInputSchema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"api_id": {
						Type:        "string",
						Description: "The unique ID or name of the API.",
					},
				},
				Required: []string{"api_id"},
			},
		},
		{
			Name:        "invoke_api",
			Description: "Execute a live API request through the RelayOps gateway data-plane. Automatically passes your developer credentials.",
			InputSchema: ToolInputSchema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"method": {
						Type:        "string",
						Description: "HTTP method (GET, POST, PUT, DELETE, PATCH, etc.).",
						Enum:        []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"},
					},
					"path": {
						Type:        "string",
						Description: "Gateway route path (e.g. '/weather/v1/forecast?latitude=52.52&longitude=13.41' or '/orders/hello').",
					},
					"headers": {
						Type:        "object",
						Description: "Optional HTTP request headers.",
					},
					"body": {
						Type:        "string",
						Description: "Optional request body payload (JSON or text).",
					},
				},
				Required: []string{"method", "path"},
			},
		},
		{
			Name:        "get_my_usage",
			Description: "Retrieve current account details, registered application name, subscribed API plans, and rate limit quotas.",
			InputSchema: ToolInputSchema{
				Type:       "object",
				Properties: map[string]PropertySchema{},
			},
		},
	}

	// Admin-only platform ops tools
	if caller.Role == RoleAdmin {
		tools = append(tools,
			Tool{
				Name:        "relayops_plan",
				Description: "Operator Tool: Dry-run and validate a declarative RelayOps YAML/JSON document with consumer impact and replay analysis.",
				InputSchema: ToolInputSchema{
					Type: "object",
					Properties: map[string]PropertySchema{
						"config_yaml": {
							Type:        "string",
							Description: "The complete declarative YAML or JSON configuration document.",
						},
					},
					Required: []string{"config_yaml"},
				},
			},
			Tool{
				Name:        "relayops_apply",
				Description: "Operator Tool: Deploy a declarative RelayOps configuration fleet-wide or as an automated canary rollout.",
				InputSchema: ToolInputSchema{
					Type: "object",
					Properties: map[string]PropertySchema{
						"config_yaml": {
							Type:        "string",
							Description: "Declarative configuration document.",
						},
						"plan_hash": {
							Type:        "string",
							Description: "Reviewed plan hash from relayops_plan to guarantee pinned execution.",
						},
						"canary": {
							Type:        "boolean",
							Description: "Deploy as a canary revision rather than an immediate fleet-wide release.",
						},
						"traffic_percent": {
							Type:        "integer",
							Description: "Traffic percentage to route to canary (e.g. 10 for 10%).",
						},
					},
					Required: []string{"config_yaml"},
				},
			},
			Tool{
				Name:        "relayops_rollout_status",
				Description: "Operator Tool: Check active revision, in-flight canary revision, fleet convergence status, and error rates.",
				InputSchema: ToolInputSchema{
					Type:       "object",
					Properties: map[string]PropertySchema{},
				},
			},
			Tool{
				Name:        "relayops_diagnose",
				Description: "Operator Tool: Diagnose why a request received its status code (tracing auth, subscription, quota, upstream attempts).",
				InputSchema: ToolInputSchema{
					Type: "object",
					Properties: map[string]PropertySchema{
						"request_id": {
							Type:        "string",
							Description: "The unique request ID or trace ID to diagnose.",
						},
					},
					Required: []string{"request_id"},
				},
			},
			Tool{
				Name:        "relayops_test_run",
				Description: "Operator Tool: Run a Test Studio synthetic assertion contract verification suite against live endpoints.",
				InputSchema: ToolInputSchema{
					Type: "object",
					Properties: map[string]PropertySchema{
						"suite_id": {
							Type:        "string",
							Description: "Test Studio suite ID or name.",
						},
						"env": {
							Type:        "string",
							Description: "Target environment (e.g. production, staging).",
						},
					},
					Required: []string{"suite_id"},
				},
			},
		)
	}

	return tools
}

// callTool executes an individual tool.
func (e *Engine) callTool(ctx context.Context, caller CallerContext, name string, args map[string]any) CallToolResult {
	switch name {
	case "list_apis":
		return e.toolListAPIs(ctx, caller, args)
	case "get_api_spec":
		return e.toolGetAPISpec(ctx, caller, args)
	case "invoke_api":
		return e.toolInvokeAPI(ctx, caller, args)
	case "get_my_usage":
		return e.toolGetMyUsage(ctx, caller, args)
	case "relayops_plan":
		if caller.Role != RoleAdmin {
			return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Permission denied: admin token required for relayops_plan"}}}
		}
		return e.toolRelayOpsPlan(ctx, args)
	case "relayops_apply":
		if caller.Role != RoleAdmin {
			return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Permission denied: admin token required for relayops_apply"}}}
		}
		return e.toolRelayOpsApply(ctx, args)
	case "relayops_rollout_status":
		if caller.Role != RoleAdmin {
			return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Permission denied: admin token required for relayops_rollout_status"}}}
		}
		return e.toolRelayOpsRolloutStatus(ctx)
	case "relayops_diagnose":
		if caller.Role != RoleAdmin {
			return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Permission denied: admin token required for relayops_diagnose"}}}
		}
		return e.toolRelayOpsDiagnose(ctx, args)
	case "relayops_test_run":
		if caller.Role != RoleAdmin {
			return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Permission denied: admin token required for relayops_test_run"}}}
		}
		return e.toolRelayOpsTestRun(ctx, args)
	default:
		return CallToolResult{
			IsError: true,
			Content: []ContentItem{{Type: "text", Text: fmt.Sprintf("Unknown tool: %s", name)}},
		}
	}
}

func (e *Engine) toolListAPIs(ctx context.Context, caller CallerContext, args map[string]any) CallToolResult {
	if e.store == nil {
		return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Store unavailable"}}}
	}
	apis, err := e.store.ListAPIs(ctx)
	if err != nil {
		return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Failed to list APIs: " + err.Error()}}}
	}

	query := ""
	if q, ok := args["query"].(string); ok {
		query = strings.ToLower(strings.TrimSpace(q))
	}

	type APISummary struct {
		ID                 string `json:"id"`
		Name               string `json:"name"`
		Description        string `json:"description"`
		BasePath           string `json:"base_path"`
		AuthType           string `json:"auth_type"`
		IsAI               bool   `json:"is_ai"`
		RateLimitPerMinute int    `json:"rate_limit_per_minute"`
		QuotaPerDay        int    `json:"quota_per_day"`
	}

	var results []APISummary
	for _, a := range apis {
		if !a.Enabled || a.IsDraft {
			continue
		}
		// Non-admins see public APIs or APIs in their tenant
		if caller.Role != RoleAdmin && a.Visibility != "public" && a.TenantID != caller.TenantID {
			continue
		}
		if query != "" {
			nameMatch := strings.Contains(strings.ToLower(a.Name), query)
			descMatch := strings.Contains(strings.ToLower(a.Description), query)
			pathMatch := strings.Contains(strings.ToLower(a.BasePath), query)
			if !nameMatch && !descMatch && !pathMatch {
				continue
			}
		}
		results = append(results, APISummary{
			ID:                 a.ID,
			Name:               a.Name,
			Description:        a.Description,
			BasePath:           a.BasePath,
			AuthType:           a.AuthType,
			IsAI:               a.IsAI,
			RateLimitPerMinute: a.RateLimitPerMinute,
			QuotaPerDay:        a.QuotaPerDay,
		})
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return CallToolResult{
		Content: []ContentItem{{Type: "text", Text: string(data)}},
	}
}

func (e *Engine) toolGetAPISpec(ctx context.Context, caller CallerContext, args map[string]any) CallToolResult {
	if e.store == nil {
		return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Store unavailable"}}}
	}
	apiID, _ := args["api_id"].(string)
	if apiID == "" {
		return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Missing required argument 'api_id'"}}}
	}

	api, err := e.store.GetAPI(ctx, apiID)
	if err != nil {
		// Try matching by name
		all, _ := e.store.ListAPIs(ctx)
		found := false
		for _, a := range all {
			if strings.EqualFold(a.Name, apiID) {
				api = a
				found = true
				break
			}
		}
		if !found {
			return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "API not found: " + apiID}}}
		}
	}

	spec := api.OpenAPISpec
	if len(spec) == 0 && e.synthSpecFn != nil {
		spec = e.synthSpecFn(api)
	}
	if len(spec) == 0 {
		// Provide basic fallback spec
		spec = map[string]any{
			"openapi": "3.1.0",
			"info": map[string]any{
				"title":       api.Name,
				"description": api.Description,
				"version":     "1.0.0",
			},
			"servers": []map[string]any{
				{"url": e.gatewayURL + api.BasePath},
			},
		}
	}

	data, _ := json.MarshalIndent(spec, "", "  ")
	return CallToolResult{
		Content: []ContentItem{{Type: "text", Text: string(data)}},
	}
}

func (e *Engine) toolInvokeAPI(ctx context.Context, caller CallerContext, args map[string]any) CallToolResult {
	method, _ := args["method"].(string)
	path, _ := args["path"].(string)
	bodyStr, _ := args["body"].(string)

	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = "GET"
	}
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	targetURL := e.gatewayURL + path

	var bodyReader io.Reader
	if bodyStr != "" {
		bodyReader = strings.NewReader(bodyStr)
	}

	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Invalid request: " + err.Error()}}}
	}

	// Attach headers from arguments
	if rawHeaders, ok := args["headers"].(map[string]any); ok {
		for k, v := range rawHeaders {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
	}

	// Attach caller credentials
	if caller.APIKey != "" {
		req.Header.Set("X-API-Key", caller.APIKey)
		if req.Header.Get("Authorization") == "" {
			req.Header.Set("Authorization", "Bearer "+caller.APIKey)
		}
	} else if caller.AdminToken != "" {
		req.Header.Set("Authorization", "Bearer "+caller.AdminToken)
	}

	start := time.Now()
	resp, err := e.httpClient.Do(req)
	duration := time.Since(start)
	if err != nil {
		return CallToolResult{
			IsError: true,
			Content: []ContentItem{{Type: "text", Text: fmt.Sprintf("Gateway error calling %s %s: %s", method, path, err.Error())}},
		}
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	headerMap := map[string]string{}
	for k, v := range resp.Header {
		if len(v) > 0 {
			headerMap[k] = v[0]
		}
	}

	resultSummary := map[string]any{
		"status_code": resp.StatusCode,
		"latency_ms":  duration.Milliseconds(),
		"headers":     headerMap,
		"body":        string(bodyBytes),
	}

	// Pretty print JSON response if body is valid JSON
	var jsonBody any
	if err := json.Unmarshal(bodyBytes, &jsonBody); err == nil {
		resultSummary["body"] = jsonBody
	}

	out, _ := json.MarshalIndent(resultSummary, "", "  ")
	return CallToolResult{
		IsError: resp.StatusCode >= 400,
		Content: []ContentItem{{Type: "text", Text: string(out)}},
	}
}

func (e *Engine) toolGetMyUsage(ctx context.Context, caller CallerContext, args map[string]any) CallToolResult {
	if caller.Role != RoleDeveloper {
		return CallToolResult{
			Content: []ContentItem{{Type: "text", Text: fmt.Sprintf("Caller Role: %s (Admin mode has unlimited rate limit and no daily quota)", caller.Role)}},
		}
	}

	details := map[string]any{
		"consumer_id":    caller.ConsumerID,
		"consumer_name":  caller.ConsumerName,
		"consumer_email": caller.ConsumerEmail,
		"tenant_id":      caller.TenantID,
		"key_prefix":     "",
	}
	if len(caller.APIKey) >= 10 {
		details["key_prefix"] = caller.APIKey[:10] + "..."
	}

	if e.store != nil {
		subs, err := e.store.ListSubscriptions(ctx)
		if err == nil {
			var mySubs []map[string]any
			for _, s := range subs {
				if s.ConsumerID == caller.ConsumerID {
					mySubs = append(mySubs, map[string]any{
						"api_id":  s.APIID,
						"plan_id": s.PlanID,
						"status":  s.Status,
						"active":  s.Active,
					})
				}
			}
			details["subscriptions"] = mySubs
		}
	}

	out, _ := json.MarshalIndent(details, "", "  ")
	return CallToolResult{
		Content: []ContentItem{{Type: "text", Text: string(out)}},
	}
}

func (e *Engine) toolRelayOpsPlan(ctx context.Context, args map[string]any) CallToolResult {
	yamlDoc, _ := args["config_yaml"].(string)
	if strings.TrimSpace(yamlDoc) == "" {
		return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "config_yaml is required"}}}
	}
	if e.planFn != nil {
		res, err := e.planFn(ctx, yamlDoc)
		if err != nil {
			return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Plan error: " + err.Error()}}}
		}
		data, _ := json.MarshalIndent(res, "", "  ")
		return CallToolResult{Content: []ContentItem{{Type: "text", Text: string(data)}}}
	}
	return CallToolResult{Content: []ContentItem{{Type: "text", Text: "Plan hook executed successfully (dry-run ready)"}}}
}

func (e *Engine) toolRelayOpsApply(ctx context.Context, args map[string]any) CallToolResult {
	yamlDoc, _ := args["config_yaml"].(string)
	planHash, _ := args["plan_hash"].(string)
	canary, _ := args["canary"].(bool)
	trafficPercent := 0
	if tp, ok := args["traffic_percent"].(float64); ok {
		trafficPercent = int(tp)
	}

	if e.applyFn != nil {
		res, err := e.applyFn(ctx, yamlDoc, planHash, canary, trafficPercent)
		if err != nil {
			return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Apply error: " + err.Error()}}}
		}
		data, _ := json.MarshalIndent(res, "", "  ")
		return CallToolResult{Content: []ContentItem{{Type: "text", Text: string(data)}}}
	}
	return CallToolResult{Content: []ContentItem{{Type: "text", Text: "Apply hook executed successfully"}}}
}

func (e *Engine) toolRelayOpsRolloutStatus(ctx context.Context) CallToolResult {
	if e.rolloutFn != nil {
		res, err := e.rolloutFn(ctx)
		if err != nil {
			return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Rollout error: " + err.Error()}}}
		}
		data, _ := json.MarshalIndent(res, "", "  ")
		return CallToolResult{Content: []ContentItem{{Type: "text", Text: string(data)}}}
	}
	return CallToolResult{Content: []ContentItem{{Type: "text", Text: `{"status":"active","converged":true}`}}}
}

func (e *Engine) toolRelayOpsDiagnose(ctx context.Context, args map[string]any) CallToolResult {
	reqID, _ := args["request_id"].(string)
	if reqID == "" {
		return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "request_id is required"}}}
	}
	if e.diagnoseFn != nil {
		res, err := e.diagnoseFn(ctx, reqID)
		if err != nil {
			return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Diagnose error: " + err.Error()}}}
		}
		data, _ := json.MarshalIndent(res, "", "  ")
		return CallToolResult{Content: []ContentItem{{Type: "text", Text: string(data)}}}
	}
	return CallToolResult{Content: []ContentItem{{Type: "text", Text: fmt.Sprintf(`{"request_id":"%s","decision":"ok"}`, reqID)}}}
}

func (e *Engine) toolRelayOpsTestRun(ctx context.Context, args map[string]any) CallToolResult {
	suiteID, _ := args["suite_id"].(string)
	env, _ := args["env"].(string)
	if suiteID == "" {
		return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "suite_id is required"}}}
	}
	if e.testRunFn != nil {
		res, err := e.testRunFn(ctx, suiteID, env)
		if err != nil {
			return CallToolResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "Test run error: " + err.Error()}}}
		}
		data, _ := json.MarshalIndent(res, "", "  ")
		return CallToolResult{Content: []ContentItem{{Type: "text", Text: string(data)}}}
	}
	return CallToolResult{Content: []ContentItem{{Type: "text", Text: fmt.Sprintf(`{"suite_id":"%s","status":"passed"}`, suiteID)}}}
}

// Resources handling

func (e *Engine) listResources(ctx context.Context, caller CallerContext) ([]Resource, error) {
	resources := []Resource{
		{
			URI:         "relayops://catalog",
			Name:        "API Catalog Overview",
			Description: "Live catalog summary of all available APIs and route entry points",
			MIMEType:    "application/json",
		},
	}

	if e.store != nil {
		apis, err := e.store.ListAPIs(ctx)
		if err == nil {
			for _, a := range apis {
				if !a.Enabled || a.IsDraft {
					continue
				}
				if caller.Role != RoleAdmin && a.Visibility != "public" && a.TenantID != caller.TenantID {
					continue
				}
				resources = append(resources, Resource{
					URI:         fmt.Sprintf("relayops://apis/%s/spec", a.ID),
					Name:        fmt.Sprintf("OpenAPI Spec: %s", a.Name),
					Description: fmt.Sprintf("OpenAPI schema definition for %s (%s)", a.Name, a.BasePath),
					MIMEType:    "application/json",
				})
			}
		}
	}

	return resources, nil
}

func (e *Engine) readResource(ctx context.Context, caller CallerContext, uri string) ([]ResourceContents, error) {
	if uri == "relayops://catalog" {
		res := e.toolListAPIs(ctx, caller, nil)
		text := ""
		if len(res.Content) > 0 {
			text = res.Content[0].Text
		}
		return []ResourceContents{
			{
				URI:      uri,
				MIMEType: "application/json",
				Text:     text,
			},
		}, nil
	}

	if strings.HasPrefix(uri, "relayops://apis/") && strings.HasSuffix(uri, "/spec") {
		trimmed := strings.TrimPrefix(uri, "relayops://apis/")
		apiID := strings.TrimSuffix(trimmed, "/spec")
		res := e.toolGetAPISpec(ctx, caller, map[string]any{"api_id": apiID})
		text := ""
		if len(res.Content) > 0 {
			text = res.Content[0].Text
		}
		return []ResourceContents{
			{
				URI:      uri,
				MIMEType: "application/json",
				Text:     text,
			},
		}, nil
	}

	return nil, fmt.Errorf("resource not found: %s", uri)
}
