package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/relayops/apim/internal/config"
	"github.com/relayops/apim/internal/mcpgw"
	"github.com/relayops/apim/internal/store"
)

// discoverMCPTools lists the tools an MCP API's server offers now and
// compares them with the API's approved (pinned) catalog. Approving tools is
// an ordinary API update of mcp_policy.pinned_tools, so it goes through
// revisions, approvals and canaries like any other change.
func (s *Server) discoverMCPTools(w http.ResponseWriter, r *http.Request) {
	a, ok := s.scopedAPI(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if a.Protocol != "mcp" {
		writeErr(w, http.StatusBadRequest, "not_mcp", "this API's protocol is not mcp")
		return
	}
	var in struct {
		// Path is appended to the upstream URL when the server's MCP
		// endpoint is below it (for example "/mcp").
		Path string `json:"path"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil && err != io.EOF {
			writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	endpoint := strings.TrimRight(a.UpstreamURL, "/")
	if in.Path != "" {
		if !strings.HasPrefix(in.Path, "/") || strings.Contains(in.Path, "..") || strings.ContainsAny(in.Path, "?#") {
			writeErr(w, http.StatusBadRequest, "invalid_path", "path must be an absolute path below the upstream URL")
			return
		}
		endpoint += in.Path
	}
	headers := make(map[string]string, len(a.RequestHeaders))
	for k, v := range a.RequestHeaders {
		headers[k] = config.ResolveSecrets(v)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	found, err := mcpgw.DiscoverAll(ctx, &http.Client{Timeout: 20 * time.Second}, endpoint, headers)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "mcp_discovery_failed", err.Error())
		return
	}
	entries, removedTools := mcpgw.CatalogOf(mcpgw.KindTool, found.Tools, a.MCPPolicy.PinnedTools)
	prompts, removedPrompts := mcpgw.CatalogOf(mcpgw.KindPrompt, found.Prompts, a.MCPPolicy.PinnedPrompts)
	entries = append(entries, prompts...)
	if entries == nil {
		entries = []mcpgw.CatalogEntry{}
	}
	// Approved items the server no longer offers.
	type gone struct {
		Kind        string `json:"kind"`
		Name        string `json:"name"`
		Fingerprint string `json:"fingerprint"`
	}
	removed := []gone{}
	for _, n := range removedTools {
		removed = append(removed, gone{mcpgw.KindTool, n, a.MCPPolicy.PinnedTools[n]})
	}
	for _, n := range removedPrompts {
		removed = append(removed, gone{mcpgw.KindPrompt, n, a.MCPPolicy.PinnedPrompts[n]})
	}
	// Record what the server offers, so approvals carry the definition (and
	// the input schema used to validate arguments).
	var obs []store.MCPObservation
	for _, e := range entries {
		pins := a.MCPPolicy.PinnedTools
		if e.Kind == mcpgw.KindPrompt {
			pins = a.MCPPolicy.PinnedPrompts
		}
		status := "resolved"
		switch {
		case e.Status == "pinned":
			status = "approved"
		case len(pins) > 0:
			status = "pending"
		}
		obs = append(obs, store.MCPObservation{APIID: a.ID, Kind: e.Kind, Name: e.Name, Fingerprint: e.Fingerprint,
			Definition: e.Definition, Status: status, Source: "discovered"})
	}
	if err := s.RecordMCPObservations(r.Context(), obs, "discovery:"+s.getActor(r)); err != nil {
		slog.Warn("record discovered MCP definitions", "api", a.ID, "err", err)
	}
	s.audit(r, "api:mcp_discover", "api", a.ID, map[string]any{"definitions": len(entries), "removed": len(removed)})
	if found.Warnings == nil {
		found.Warnings = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_id": a.ID, "endpoint": endpoint, "tools": entries, "removed": removed, "warnings": found.Warnings})
}
