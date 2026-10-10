// Package wasmplugin is the Go SDK for RelayOps gateway policy plugins.
//
// A plugin is a WebAssembly reactor module that the gateway runs on each
// request of the APIs that list it in traffic_policy.wasm_plugins, after
// authentication and rate limits and before the upstream call. It can allow
// the request, refuse it, or modify the headers and body sent upstream.
//
//	package main
//
//	import plugin "github.com/relayops/apim/sdk/wasmplugin"
//
//	func init() { plugin.Handle(handle) }
//	func main() {}
//
//	func handle(r plugin.Request) plugin.Result {
//		if r.Header("X-Tenant") == "" {
//			return plugin.Deny(400, "tenant_required", "send X-Tenant")
//		}
//		return plugin.Allow()
//	}
//
// Build with:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o my-plugin.wasm .
//
// Register the handler in init: a reactor module runs package initialisers
// but not main. The gateway reuses module instances across requests (one
// request at a time each), so keep no per-request state in globals.
package wasmplugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"sync"
)

// Request is what the gateway passes to a plugin.
type Request struct {
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	ClientIP string              `json:"client_ip"`
	Headers  map[string][]string `json:"headers"`
	Query    map[string][]string `json:"query_params"`
	// Body is the request body as text. It is nil unless the API sets
	// traffic_policy.wasm_body_limit_bytes.
	Body *string `json:"body,omitempty"`
	// Config is this plugin's settings for the API
	// (traffic_policy.wasm_config[<plugin name>]).
	Config json.RawMessage `json:"config,omitempty"`
}

// Header returns the first value of a request header (case-insensitive).
func (r Request) Header(name string) string {
	if v := r.Headers[textproto.CanonicalMIMEHeaderKey(name)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// QueryParam returns the first value of a query parameter.
func (r Request) QueryParam(name string) string {
	if v := r.Query[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Result is a plugin's decision.
type Result struct {
	Action        string            `json:"action"` // allow, deny or modify
	Status        int               `json:"status,omitempty"`
	Reason        string            `json:"reason,omitempty"`
	Detail        string            `json:"detail,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	RemoveHeaders []string          `json:"remove_headers,omitempty"`
	Body          *string           `json:"body,omitempty"`
}

// Allow lets the request continue unchanged.
func Allow() Result { return Result{Action: "allow"} }

// Deny refuses the request with an HTTP status (400-599), a machine-readable
// reason and a message for the caller.
func Deny(status int, reason, detail string) Result {
	return Result{Action: "deny", Status: status, Reason: reason, Detail: detail}
}

// Modify starts a result that changes the upstream request.
func Modify() Result { return Result{Action: "modify"} }

// SetHeader sets a header on the upstream request.
func (r Result) SetHeader(name, value string) Result {
	if r.Headers == nil {
		r.Headers = map[string]string{}
	}
	r.Headers[name] = value
	return r
}

// RemoveHeader deletes a header from the upstream request.
func (r Result) RemoveHeader(name string) Result {
	r.RemoveHeaders = append(r.RemoveHeaders, name)
	return r
}

// SetBody replaces the upstream request body. The API must set
// traffic_policy.wasm_body_limit_bytes.
func (r Result) SetBody(body string) Result {
	r.Body = &body
	return r
}

// Handler decides on one request.
type Handler func(Request) Result

// Response is what a response-phase handler receives.
type Response struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	// Body is the response body as text. It is nil unless the API sets
	// traffic_policy.wasm_response_body_limit_bytes and the body fit (not
	// streamed); BodyTruncated is then true.
	Body          *string `json:"body,omitempty"`
	BodyTruncated bool    `json:"body_truncated,omitempty"`
	Request       struct {
		Method   string              `json:"method"`
		Path     string              `json:"path"`
		ClientIP string              `json:"client_ip"`
		Headers  map[string][]string `json:"headers"`
	} `json:"request"`
	Config json.RawMessage `json:"config,omitempty"`
}

// Header returns the first value of a response header (case-insensitive).
func (r Response) Header(name string) string {
	if v := r.Headers[textproto.CanonicalMIMEHeaderKey(name)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// ResponseHandler decides on one response: Allow, Modify (headers, body,
// status via SetStatus) or Deny (replace it with an error).
type ResponseHandler func(Response) Result

var (
	handler         Handler
	responseHandler ResponseHandler
)

// Handle registers the plugin's request handler. Call it from init.
func Handle(h Handler) { handler = h }

// HandleResponse registers a response handler. Call it from init.
func HandleResponse(h ResponseHandler) { responseHandler = h }

// SetStatus changes the response status (response phase only).
func (r Result) SetStatus(status int) Result {
	r.Status = status
	return r
}

// Phases is the bitmask the gateway reads: 1 request, 2 response.
func Phases() int32 {
	var p int32
	if handler != nil {
		p |= 1
	}
	if responseHandler != nil {
		p |= 2
	}
	return p
}

// DispatchResponse runs a response handler on the gateway's JSON input.
func DispatchResponse(h ResponseHandler, input []byte) (out []byte) {
	defer func() {
		if p := recover(); p != nil {
			out, _ = json.Marshal(Deny(http.StatusBadGateway, "plugin_panic", fmt.Sprint(p)))
		}
	}()
	if h == nil {
		out, _ = json.Marshal(Allow())
		return out
	}
	var resp Response
	if err := json.Unmarshal(input, &resp); err != nil {
		out, _ = json.Marshal(Deny(http.StatusBadGateway, "plugin_bad_input", err.Error()))
		return out
	}
	out, _ = json.Marshal(h(resp))
	return out
}

// ResponseConfig decodes a response's plugin settings like Config.
func ResponseConfig[T any](r Response, build func(*T) error) (*T, error) {
	return Config[T](Request{Config: r.Config}, build)
}

// Dispatch runs a handler on the gateway's JSON input and returns the JSON
// result. It is what the module's execute_policy export calls; tests can use
// it to exercise a plugin exactly as the gateway does.
func Dispatch(h Handler, input []byte) (out []byte) {
	defer func() {
		if p := recover(); p != nil {
			out, _ = json.Marshal(Deny(http.StatusInternalServerError, "plugin_panic", fmt.Sprint(p)))
		}
	}()
	if h == nil {
		out, _ = json.Marshal(Deny(http.StatusInternalServerError, "plugin_no_handler", "the plugin did not call wasmplugin.Handle"))
		return out
	}
	var req Request
	if err := json.Unmarshal(input, &req); err != nil {
		out, _ = json.Marshal(Deny(http.StatusInternalServerError, "plugin_bad_input", err.Error()))
		return out
	}
	out, _ = json.Marshal(h(req))
	return out
}

// Config decodes the request's plugin settings into T, caching the result per
// distinct settings document so per-request parsing (and anything built from
// the settings, like compiled patterns) is paid once. The cache is bounded.
func Config[T any](r Request, build func(*T) error) (*T, error) {
	key := string(r.Config)
	configMu.Lock()
	if v, ok := configCache[key]; ok {
		configMu.Unlock()
		if c, ok := v.(*T); ok {
			return c, nil
		}
	} else {
		configMu.Unlock()
	}
	c := new(T)
	if len(r.Config) > 0 {
		if err := json.Unmarshal(r.Config, c); err != nil {
			return nil, fmt.Errorf("plugin config: %w", err)
		}
	}
	if build != nil {
		if err := build(c); err != nil {
			return nil, fmt.Errorf("plugin config: %w", err)
		}
	}
	configMu.Lock()
	if len(configCache) >= 64 {
		clear(configCache)
	}
	configCache[key] = c
	configMu.Unlock()
	return c, nil
}

var (
	configMu    sync.Mutex
	configCache = map[string]any{}
)

// Misconfigured is the result for settings a plugin cannot use: the request
// is refused (fail closed) and the reason names the problem.
func Misconfigured(err error) Result {
	return Deny(http.StatusInternalServerError, "plugin_misconfigured", err.Error())
}
