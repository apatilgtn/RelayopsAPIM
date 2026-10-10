package admin

import (
	"strings"

	"github.com/relayops/apim/internal/grpc"
	"github.com/relayops/apim/internal/store"
)

// synthesizeOpenAPISpec creates a complete, real, standards-compliant OpenAPI 3.0.0
// document for any API that does not already have an uploaded specification.
func (s *Server) synthesizeOpenAPISpec(a store.API) map[string]any {
	serverURL := s.gatewayURL
	if serverURL == "" {
		serverURL = "http://localhost:8080"
	}
	basePath := strings.TrimRight(a.BasePath, "/")

	title := a.Name
	desc := a.Description
	// No description is invented: an empty one stays empty, and the portal
	// says the owner has not described the API.

	// Security Schemes
	secSchemes := map[string]any{}
	securityRequirement := []map[string][]string{}

	switch a.AuthType {
	case "api_key":
		secSchemes["ApiKeyAuth"] = map[string]any{
			"type":        "apiKey",
			"in":          "header",
			"name":        "X-API-Key",
			"description": "API Key issued via RelayOps Developer Portal. Pass as 'X-API-Key: rk_xxx'",
		}
		securityRequirement = append(securityRequirement, map[string][]string{"ApiKeyAuth": {}})
	case "jwt":
		secSchemes["BearerAuth"] = map[string]any{
			"type":         "http",
			"scheme":       "bearer",
			"bearerFormat": "JWT",
			"description":  "Signed JSON Web Token (JWT). Pass as 'Authorization: Bearer <jwt>'",
		}
		securityRequirement = append(securityRequirement, map[string][]string{"BearerAuth": {}})
	case "oidc":
		issuer := a.OIDCIssuer
		secSchemes["OpenIDConnect"] = map[string]any{
			"type":             "openIdConnect",
			"openIdConnectUrl": strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration",
			"description":      "Federated enterprise identity verified against OIDC provider.",
		}
		securityRequirement = append(securityRequirement, map[string][]string{"OpenIDConnect": {}})
	}

	paths := map[string]any{}

	if a.IsAI {
		// AI Model Gateway endpoints
		paths["/v1/chat/completions"] = map[string]any{
			"post": map[string]any{
				"summary":     "Create Chat Completion",
				"description": "Send a chat completion prompt to the governed AI upstream. Enforces token budgets and prompt safety.",
				"operationId": "createChatCompletion",
				"tags":        []string{"AI Models"},
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type":     "object",
								"required": []string{"model", "messages"},
								"properties": map[string]any{
									"model": map[string]any{
										"type":        "string",
										"example":     "claude-3-5-sonnet-20241022",
										"description": "Target model identifier",
									},
									"messages": map[string]any{
										"type": "array",
										"items": map[string]any{
											"type":     "object",
											"required": []string{"role", "content"},
											"properties": map[string]any{
												"role":    map[string]any{"type": "string", "example": "user"},
												"content": map[string]any{"type": "string", "example": "Summarize latest product metrics."},
											},
										},
									},
									"temperature": map[string]any{"type": "number", "example": 0.7},
									"max_tokens":  map[string]any{"type": "integer", "example": 1024},
								},
							},
						},
					},
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "Model response generated successfully",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"id":      map[string]any{"type": "string", "example": "chatcmpl-9K2D"},
										"object":  map[string]any{"type": "string", "example": "chat.completion"},
										"model":   map[string]any{"type": "string", "example": "claude-3-5-sonnet-20241022"},
										"choices": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
										"usage": map[string]any{
											"type": "object",
											"properties": map[string]any{
												"prompt_tokens":     map[string]any{"type": "integer", "example": 25},
												"completion_tokens": map[string]any{"type": "integer", "example": 80},
												"total_tokens":      map[string]any{"type": "integer", "example": 105},
											},
										},
									},
								},
							},
						},
					},
					"401": map[string]any{"description": "Unauthorized - Missing or invalid credentials"},
					"429": map[string]any{"description": "Token quota or rate limit exceeded"},
					"503": map[string]any{"description": "Upstream AI model unavailable or failover in progress"},
				},
			},
		}
		paths["/v1/models"] = map[string]any{
			"get": map[string]any{
				"summary":     "List Available AI Models",
				"description": "Lists the active and fallback models accessible through this gateway route.",
				"operationId": "listModels",
				"tags":        []string{"AI Models"},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "List of available models",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"data": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
									},
								},
							},
						},
					},
				},
			},
		}
	} else {
		for path, item := range synthesizedPaths(a) {
			paths[path] = item
		}
	}

	doc := map[string]any{
		"openapi": "3.0.0",
		"info": map[string]any{
			"title":       title,
			"description": desc,
			"version":     "1.0.0",
			// Marks a reference the gateway generated because the API owner
			// published none; clients should not treat it as a contract.
			"x-relayops-synthesized": true,
			"x-relayops-protocol":    protocolOf(a),
		},
		"servers": []map[string]any{
			{
				"url":         serverURL + basePath,
				"description": "RelayOps gateway",
			},
		},
		"paths": paths,
	}

	if len(secSchemes) > 0 {
		doc["components"] = map[string]any{
			"securitySchemes": secSchemes,
		}
		doc["security"] = securityRequirement
	}

	return doc
}

func protocolOf(a store.API) string {
	if a.Protocol == "" {
		return "http"
	}
	return a.Protocol
}

// synthesizedPaths describes what is actually known about an API that has
// no published specification, per protocol, without inventing endpoints.
func synthesizedPaths(a store.API) map[string]any {
	op := func(summary, desc string, body map[string]any) map[string]any {
		o := map[string]any{"summary": summary, "description": desc, "tags": []string{a.Name},
			"responses": map[string]any{"200": map[string]any{"description": "Response from the upstream service"}}}
		if body != nil {
			o["requestBody"] = body
		}
		return o
	}
	jsonBody := func(schema map[string]any) map[string]any {
		return map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schema}}}
	}
	switch protocolOf(a) {
	case "graphql":
		desc := "GraphQL endpoint. Send {\"query\", \"variables\", \"operationName\"}; subscriptions use WebSocket (graphql-transport-ws)."
		if a.GraphQLSchema != "" {
			desc += "\n\nSchema (SDL):\n\n```graphql\n" + a.GraphQLSchema + "\n```"
		}
		return map[string]any{"/": map[string]any{"post": op("GraphQL query or mutation", desc, jsonBody(map[string]any{
			"type": "object", "required": []string{"query"},
			"properties": map[string]any{"query": map[string]any{"type": "string"}, "variables": map[string]any{"type": "object"}, "operationName": map[string]any{"type": "string"}},
		}))}}
	case "mcp":
		return map[string]any{"/": map[string]any{"post": op("MCP (Model Context Protocol) endpoint",
			"Streamable HTTP transport, JSON-RPC 2.0: initialize, tools/list, tools/call, resources/*, prompts/*. Only tools approved for this API are listed.",
			jsonBody(map[string]any{"type": "object", "required": []string{"jsonrpc", "method"},
				"properties": map[string]any{"jsonrpc": map[string]any{"type": "string", "example": "2.0"}, "id": map[string]any{}, "method": map[string]any{"type": "string", "example": "tools/list"}, "params": map[string]any{"type": "object"}}}))}}
	case "grpc":
		paths := map[string]any{}
		methods, err := grpc.ParseDescriptorSet(a.GRPCDescriptorSet)
		if len(a.GRPCDescriptorSet) == 0 || err != nil {
			return paths // methods are unknown until a descriptor set is uploaded
		}
		for name, m := range methods {
			desc := "gRPC " + m.Kind() + " method. Call it with gRPC or gRPC-Web"
			if a.GRPCPolicy.JSONTranscoding && m.Kind() == "unary" {
				desc += ", or POST JSON (request message " + string(m.Desc.Input().FullName()) + ")"
			}
			paths["/"+name] = map[string]any{"post": op(name, desc+".", nil)}
		}
		return paths
	}
	return map[string]any{"/{path}": map[string]any{
		"parameters": []map[string]any{{"name": "path", "in": "path", "required": true, "schema": map[string]any{"type": "string"},
			"description": "Any path below the base path is forwarded to the service."}},
		"get": op("Any request below the base path",
			"The API owner has not published a reference for this API. Requests to any path and method below the base path are forwarded to the service.", nil),
	}}
}
