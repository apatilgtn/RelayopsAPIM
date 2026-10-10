package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/relayops/apim/internal/policy"
	"github.com/relayops/apim/internal/store"
)

// wasmTimeout bounds one plugin execution on the request path.
var wasmTimeout = 50 * time.Millisecond

// WasmPluginNames lists the plugins this gateway has loaded.
func (g *Gateway) WasmPluginNames() []string {
	if g.wasm == nil {
		return []string{}
	}
	return g.wasm.Names()
}

// SetWasmManager makes WASM policy plugins available to APIs that list them
// in traffic_policy.wasm_plugins.
func (g *Gateway) SetWasmManager(m *policy.WasmManager) { g.wasm = m }

// LoadWasmPlugins compiles every *.wasm file in dir and registers it under its
// file name without the extension. Plugins are code: they ship with the
// gateway (image or mounted volume), not in configuration.
func LoadWasmPlugins(ctx context.Context, dir string) (*policy.WasmManager, []string, error) {
	m, err := policy.NewWasmManager(ctx)
	if err != nil {
		return nil, nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.wasm"))
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	sort.Strings(files)
	var names []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			m.Close()
			return nil, nil, fmt.Errorf("read WASM plugin %s: %w", f, err)
		}
		name := strings.TrimSuffix(filepath.Base(f), ".wasm")
		if _, err := m.RegisterPlugin(ctx, name, raw); err != nil {
			m.Close()
			return nil, nil, fmt.Errorf("compile WASM plugin %s: %w", f, err)
		}
		names = append(names, name)
	}
	return m, names, nil
}

// runWasmPlugins executes an API's plugin chain in order. A deny, an error,
// a timeout or a plugin missing on this node refuses the request (fail
// closed). A modify result sets headers on the request sent upstream.
func (g *Gateway) runWasmPlugins(w http.ResponseWriter, r *http.Request, st *reqState, tp store.TrafficPolicy, req policy.Request) bool {
	names := tp.WasmPlugins
	evals := make([]map[string]any, 0, len(names))
	record := func() {
		if st.policyEvaluations == nil {
			st.policyEvaluations = make(map[string]any)
		}
		st.policyEvaluations["wasm"] = evals
	}
	// Plugins see the body only when the API opts in; it is buffered (bounded)
	// and handed back to the request for the upstream call.
	if limit := tp.WasmBodyLimitBytes; limit > 0 && r.Body != nil && r.Body != http.NoBody {
		body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
		r.Body.Close()
		if err != nil {
			st.decisionPolicy, st.decisionReason = "wasm", "body_unreadable"
			g.reject(w, st, http.StatusBadRequest, "request_body_unreadable", "could not read the request body")
			return false
		}
		if len(body) > limit {
			st.decisionPolicy, st.decisionReason = "wasm", "body_too_large"
			g.reject(w, st, http.StatusRequestEntityTooLarge, "request_body_too_large",
				fmt.Sprintf("request body exceeds the %d bytes this API's plugins inspect", limit))
			return false
		}
		setRequestBody(r, body)
		req.Body = body
	} else if tp.WasmBodyLimitBytes > 0 {
		req.Body = []byte{}
	}
	for _, name := range names {
		var plugin *policy.WasmPlugin
		ok := false
		if g.wasm != nil {
			plugin, ok = g.wasm.GetPlugin(name)
		}
		if !ok {
			evals = append(evals, map[string]any{"plugin": name, "action": "unavailable"})
			record()
			st.decisionPolicy, st.decisionReason = "wasm", "plugin_unavailable"
			slog.Warn("API references a WASM plugin this gateway has not loaded", "plugin", name, "api", r.URL.Path)
			g.reject(w, st, http.StatusServiceUnavailable, "wasm_plugin_unavailable",
				fmt.Sprintf("WASM plugin %q is not loaded on this gateway", name))
			return false
		}
		if !plugin.HandlesRequests() {
			continue // a response-only plugin
		}
		req.PluginConfig = tp.WasmConfig[name]
		ctx, cancel := context.WithTimeout(r.Context(), wasmTimeout)
		started := time.Now()
		res, err := plugin.Execute(ctx, req)
		cancel()
		eval := map[string]any{"plugin": name, "action": string(res.Action), "took_ms": float64(time.Since(started).Microseconds()) / 1000}
		if res.Reason != "" {
			eval["reason"] = res.Reason
		}
		evals = append(evals, eval)
		if err != nil || res.Action == policy.ActionDeny {
			record()
			status, reason, detail := res.Status, res.Reason, res.Detail
			if err != nil {
				status, reason, detail = http.StatusInternalServerError, "wasm_plugin_failed", err.Error()
			}
			if status < 400 || status > 599 {
				status = http.StatusForbidden
			}
			if reason == "" {
				reason = "wasm_policy_denied"
			}
			if detail == "" {
				detail = fmt.Sprintf("request refused by WASM plugin %q", name)
			}
			st.decisionPolicy, st.decisionReason = "wasm", reason
			g.reject(w, st, status, reason, detail)
			return false
		}
		if res.Action == policy.ActionModify {
			for _, k := range res.RemoveHeaders {
				r.Header.Del(k)
			}
			for k, v := range res.Headers {
				r.Header.Set(k, v)
			}
			if res.Body != nil {
				if tp.WasmBodyLimitBytes <= 0 {
					// The plugin never saw the body; replacing it blind is refused.
					st.decisionPolicy, st.decisionReason = "wasm", "body_rewrite_without_access"
					record()
					g.reject(w, st, http.StatusInternalServerError, "wasm_plugin_failed",
						fmt.Sprintf("WASM plugin %q rewrote the body but traffic_policy.wasm_body_limit_bytes is 0", name))
					return false
				}
				body := []byte(*res.Body)
				setRequestBody(r, body)
				req.Body = body
				eval["body_rewritten"] = true
			}
		}
	}
	record()
	return true
}

// setRequestBody replaces the body sent upstream.
func setRequestBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
}

// responsePlugins are the API's loaded plugins that run on responses.
func (g *Gateway) responsePlugins(tp store.TrafficPolicy) []*policy.WasmPlugin {
	if g.wasm == nil || len(tp.WasmPlugins) == 0 {
		return nil
	}
	var out []*policy.WasmPlugin
	for _, name := range tp.WasmPlugins {
		if p, ok := g.wasm.GetPlugin(name); ok && p.HandlesResponses() {
			out = append(out, p)
		}
	}
	return out
}

// runWasmResponse runs response-phase plugins in order. A plugin can change
// headers, status and (when the API grants it) the body, or refuse the
// response; a failing plugin refuses it (fail closed).
func (g *Gateway) runWasmResponse(resp *http.Response, st *reqState) {
	tp := st.route.API.TrafficPolicy
	plugins := g.responsePlugins(tp)
	if len(plugins) == 0 {
		return
	}
	in := policy.ResponseInput{Status: resp.StatusCode, Header: resp.Header,
		Request: policy.Request{Method: resp.Request.Method, Path: resp.Request.URL.Path, Header: resp.Request.Header,
			ClientIP: resp.Request.Header.Get("X-Forwarded-For")}}
	buffered := false
	if limit := tp.WasmResponseBodyLimitBytes; limit > 0 && resp.Body != nil && resp.Body != http.NoBody &&
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
		switch {
		case err != nil:
			in.BodyTruncated = true
			resp.Body = readCloser{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
		case len(body) > limit:
			in.BodyTruncated = true
			resp.Body = readCloser{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
		default:
			resp.Body.Close()
			in.Body, buffered = body, true
		}
	} else if limit > 0 {
		in.BodyTruncated = true // streamed
	}
	var evals []map[string]any
	defer func() { st.policyEvaluations["wasm_response"] = evals }()
	for _, p := range plugins {
		in.PluginConfig = tp.WasmConfig[p.Name]
		ctx, cancel := context.WithTimeout(context.Background(), wasmTimeout)
		started := time.Now()
		res, err := p.ExecuteResponse(ctx, in)
		cancel()
		eval := map[string]any{"plugin": p.Name, "action": string(res.Action), "took_ms": float64(time.Since(started).Microseconds()) / 1000}
		if res.Reason != "" {
			eval["reason"] = res.Reason
		}
		evals = append(evals, eval)
		if err == nil && res.Action == policy.ActionModify && res.Body != nil && !buffered {
			err = fmt.Errorf("plugin %q rewrote a response body it was not given (set traffic_policy.wasm_response_body_limit_bytes)", p.Name)
		}
		if err != nil || res.Action == policy.ActionDeny {
			status, reason, detail := res.Status, res.Reason, res.Detail
			if err != nil {
				status, reason, detail = http.StatusBadGateway, "wasm_plugin_failed", err.Error()
			}
			if status < 400 || status > 599 {
				status = http.StatusBadGateway
			}
			if reason == "" {
				reason = "wasm_response_denied"
			}
			st.decisionPolicy, st.decisionReason, st.err = "wasm", reason, detail
			replaceResponse(resp, status, reason, detail)
			return
		}
		if res.Action == policy.ActionModify {
			for _, k := range res.RemoveHeaders {
				resp.Header.Del(k)
			}
			for k, v := range res.Headers {
				resp.Header.Set(k, v)
			}
			if res.Status >= 100 && res.Status <= 599 {
				resp.StatusCode = res.Status
				in.Status = res.Status
			}
			if res.Body != nil {
				in.Body = []byte(*res.Body)
				eval["body_rewritten"] = true
			}
		}
	}
	if buffered {
		resp.Body = io.NopCloser(bytes.NewReader(in.Body))
		resp.ContentLength = int64(len(in.Body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(in.Body)))
	}
}

// replaceResponse turns resp into a gateway error response.
func replaceResponse(resp *http.Response, status int, code, msg string) {
	if resp.Body != nil {
		resp.Body.Close()
	}
	body, _ := json.Marshal(map[string]string{"error": code, "message": msg})
	resp.StatusCode = status
	resp.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	resp.Header = http.Header{"Content-Type": {"application/json"}, "Content-Length": {strconv.Itoa(len(body))}}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
}
