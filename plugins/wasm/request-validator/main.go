// Command request-validator rejects malformed requests before they reach the
// upstream.
//
//	"wasm_body_limit_bytes": 65536,
//	"wasm_config": {"request-validator": {
//	  "methods":          ["GET", "POST"],
//	  "required_headers": ["X-Tenant"],
//	  "required_query":   ["version"],
//	  "content_types":    ["application/json"],
//	  "json_required":    ["order.id", "amount"],
//	  "max_body_bytes":   4096
//	}}
//
// Content type, JSON and body-size checks apply to requests with a body and
// need wasm_body_limit_bytes. json_required paths use dots for nesting.
package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strings"

	plugin "github.com/relayops/apim/sdk/wasmplugin"
)

type config struct {
	Methods         []string `json:"methods"`
	RequiredHeaders []string `json:"required_headers"`
	RequiredQuery   []string `json:"required_query"`
	ContentTypes    []string `json:"content_types"`
	JSONRequired    []string `json:"json_required"`
	MaxBodyBytes    int      `json:"max_body_bytes"`
}

func init() { plugin.Handle(handle) }

func main() {}

func handle(r plugin.Request) plugin.Result {
	cfg, err := plugin.Config[config](r, nil)
	if err != nil {
		return plugin.Misconfigured(err)
	}
	if len(cfg.Methods) > 0 && !slices.ContainsFunc(cfg.Methods, func(m string) bool { return strings.EqualFold(m, r.Method) }) {
		return plugin.Deny(http.StatusMethodNotAllowed, "method_not_allowed", fmt.Sprintf("%s is not allowed; use %s", r.Method, strings.Join(cfg.Methods, ", ")))
	}
	for _, h := range cfg.RequiredHeaders {
		if strings.TrimSpace(r.Header(h)) == "" {
			return plugin.Deny(http.StatusBadRequest, "header_required", fmt.Sprintf("header %s is required", h))
		}
	}
	for _, q := range cfg.RequiredQuery {
		if r.QueryParam(q) == "" {
			return plugin.Deny(http.StatusBadRequest, "query_required", fmt.Sprintf("query parameter %s is required", q))
		}
	}
	needsBody := len(cfg.ContentTypes) > 0 || len(cfg.JSONRequired) > 0 || cfg.MaxBodyBytes > 0
	if !needsBody || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return plugin.Allow()
	}
	if r.Body == nil {
		return plugin.Misconfigured(fmt.Errorf("body checks need traffic_policy.wasm_body_limit_bytes"))
	}
	body := *r.Body
	if cfg.MaxBodyBytes > 0 && len(body) > cfg.MaxBodyBytes {
		return plugin.Deny(http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("body is larger than %d bytes", cfg.MaxBodyBytes))
	}
	if len(cfg.ContentTypes) > 0 {
		mt, _, _ := mime.ParseMediaType(r.Header("Content-Type"))
		if !slices.Contains(cfg.ContentTypes, mt) {
			return plugin.Deny(http.StatusUnsupportedMediaType, "content_type_not_allowed", fmt.Sprintf("Content-Type must be one of %s", strings.Join(cfg.ContentTypes, ", ")))
		}
	}
	if len(cfg.JSONRequired) > 0 {
		var doc any
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			return plugin.Deny(http.StatusBadRequest, "invalid_json", "body is not valid JSON")
		}
		for _, path := range cfg.JSONRequired {
			if !present(doc, strings.Split(path, ".")) {
				return plugin.Deny(http.StatusBadRequest, "field_required", fmt.Sprintf("JSON field %s is required", path))
			}
		}
	}
	return plugin.Allow()
}

// present reports whether a dotted path exists with a non-null value.
func present(doc any, path []string) bool {
	for _, key := range path {
		obj, ok := doc.(map[string]any)
		if !ok {
			return false
		}
		if doc, ok = obj[key]; !ok {
			return false
		}
	}
	return doc != nil
}
