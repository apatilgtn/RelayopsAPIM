package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/relayops/apim/internal/grpc"
	"github.com/relayops/apim/internal/store"
)

// refusals reports what the gateway refused by policy and why, per
// protocol (GraphQL, gRPC, MCP, HTTP incl. WASM plugins).
func (s *Server) refusals(w http.ResponseWriter, r *http.Request) {
	label := r.URL.Query().Get("window")
	d, ok := windows[label]
	if !ok {
		label, d = "24h", 24*time.Hour
	}
	protocol := r.URL.Query().Get("protocol")
	switch protocol {
	case "", "http", "graphql", "grpc", "mcp":
	default:
		writeErr(w, http.StatusBadRequest, "invalid_protocol", "protocol must be http, graphql, grpc or mcp")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	counts, recent, err := s.store.RefusalSummary(ctx, time.Now().Add(-d), scopeFrom(r).filter(), protocol)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": label, "by_reason": counts, "recent": recent})
}

// inspectGRPCDescriptor lists the methods in an uploaded descriptor set, so
// the console can show what an API will accept before it is saved.
func (s *Server) inspectGRPCDescriptor(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DescriptorSet []byte `json:"descriptor_set"` // base64
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, store.MaxGRPCDescriptorBytes*2)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	methods, err := grpc.ParseDescriptorSet(in.DescriptorSet)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_descriptor_set", err.Error())
		return
	}
	type method struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	out := make([]method, 0, len(methods))
	for name, m := range methods {
		out = append(out, method{name, m.Kind()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"methods": out, "bytes": len(in.DescriptorSet)})
}

// wasmPlugins lists the WASM plugins loaded by this node's gateway. Every
// gateway loads its own RELAYOPS_WASM_PLUGINS_DIR; gateway-only nodes may
// differ.
func (s *Server) wasmPlugins(w http.ResponseWriter, r *http.Request) {
	names := []string{}
	if s.gw != nil {
		names = s.gw.WasmPluginNames()
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": s.nodeID, "plugins": names})
}
