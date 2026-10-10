package admin

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/relayops/apim/internal/alerts"
	"github.com/relayops/apim/internal/mcpgw"
	"github.com/relayops/apim/internal/store"
)

// WithAlertWebhook posts alerts (such as MCP definitions held for review) to
// a webhook, for example a Slack or Teams incoming webhook.
func WithAlertWebhook(url string) Option {
	return func(s *Server) { s.alerts.WebhookURL = url }
}

// RecordMCPObservations stores MCP definitions reported by a gateway (or by
// discovery). New definitions awaiting review are audited and alerted.
func (s *Server) RecordMCPObservations(ctx context.Context, obs []store.MCPObservation, seenBy string) error {
	created, err := s.store.RecordMCPObservations(ctx, obs, seenBy)
	for _, e := range created {
		if e.Status != "pending" {
			continue
		}
		what := "new"
		if a, err := s.store.GetAPI(ctx, e.APIID); err == nil && pinnedFor(a.MCPPolicy, e.Kind, e.Name) != "" {
			what = "changed"
		}
		_ = s.store.CreateRichAuditLog(ctx, store.AuditLog{
			Actor: "gateway:" + seenBy, Action: "mcp_" + e.Kind + ":" + what, ResourceType: "api", ResourceID: e.APIID,
			ResourceName: e.APIName, TenantID: e.TenantID,
			Details: map[string]any{"name": e.Name, "fingerprint": e.Fingerprint, "catalog_entry": e.ID, "source": e.Source},
		})
		title := fmt.Sprintf("MCP %s %q on %s is %s and held for review", e.Kind, e.Name, e.APIName, what)
		if what == "new" {
			title = fmt.Sprintf("New MCP %s %q on %s is held for review", e.Kind, e.Name, e.APIName)
		}
		s.alerts.Notify(alerts.Alert{
			Type: "mcp_definition_" + what, Severity: "warning", Title: title, TenantID: e.TenantID,
			Text:    "Calls to it are refused on every gateway until it is approved. Review it under APIs → MCP tools or Approvals.",
			Link:    consoleLink("/#/approvals"),
			Details: map[string]any{"api_id": e.APIID, "kind": e.Kind, "name": e.Name, "fingerprint": e.Fingerprint, "catalog_entry": e.ID},
		})
	}
	return err
}

func pinnedFor(p store.MCPPolicy, kind, name string) string {
	if kind == mcpgw.KindPrompt {
		return p.PinnedPrompts[name]
	}
	return p.PinnedTools[name]
}

// MCPReporter is the in-process reporter for a gateway running alongside
// this control plane.
func (s *Server) MCPReporter(nodeID string) interface {
	ReportMCPObservations(ctx context.Context, obs []store.MCPObservation) error
} {
	return localMCPReporter{s: s, node: nodeID}
}

type localMCPReporter struct {
	s    *Server
	node string
}

func (l localMCPReporter) ReportMCPObservations(ctx context.Context, obs []store.MCPObservation) error {
	return l.s.RecordMCPObservations(ctx, obs, l.node)
}

// dpMCPObservations receives a gateway-only node's MCP observations.
func (s *Server) dpMCPObservations(w http.ResponseWriter, r *http.Request) {
	var obs []store.MCPObservation
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", "invalid gzip body")
			return
		}
		defer zr.Close()
		body = zr
	}
	if err := json.NewDecoder(io.LimitReader(body, 8<<20)).Decode(&obs); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "invalid MCP observation batch")
		return
	}
	if len(obs) > 500 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "at most 500 observations per batch")
		return
	}
	for i := range obs {
		// A gateway can only report what it observed; approval is a person's.
		obs[i].Status, obs[i].Source = "pending", "observed"
	}
	node := dpCaller(r).NodeID
	if node == "" {
		node = r.Header.Get("X-RelayOps-Node-Id")
	}
	s.noContent(w, s.RecordMCPObservations(r.Context(), obs, node))
}

// listMCPCatalog is the MCP review queue and history.
func (s *Server) listMCPCatalog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entries, err := s.store.ListMCPCatalog(r.Context(), scopeFrom(r).filter(), q.Get("api_id"), q.Get("status"))
	if err != nil {
		s.fail(w, err)
		return
	}
	apis := map[string]store.API{}
	for i, e := range entries {
		a, ok := apis[e.APIID]
		if !ok {
			if a, err = s.store.GetAPI(r.Context(), e.APIID); err != nil {
				continue
			}
			apis[e.APIID] = a
		}
		entries[i].Pinned = pinnedFor(a.MCPPolicy, e.Kind, e.Name)
	}
	writeJSON(w, http.StatusOK, entries)
}

// decideMCPCatalog approves, rejects or resolves a catalog entry. Approving
// pins the definition on the API, which publishes a new revision.
func (s *Server) decideMCPCatalog(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	status := map[string]string{"approve": "approved", "reject": "rejected", "resolve": "resolved"}[action]
	if status == "" {
		writeErr(w, http.StatusNotFound, "not_found", "unknown action")
		return
	}
	e, err := s.store.GetMCPCatalogEntry(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	a, ok := s.scopedAPI(w, r, e.APIID)
	if !ok {
		return
	}
	actor := s.getActor(r)
	rev := int64(0)
	if status == "approved" {
		pol := a.MCPPolicy
		if e.Kind == mcpgw.KindPrompt {
			pol.PinnedPrompts = cloneMap(pol.PinnedPrompts)
			pol.PinnedPrompts[e.Name] = e.Fingerprint
		} else {
			pol.PinnedTools = cloneMap(pol.PinnedTools)
			pol.PinnedTools[e.Name] = e.Fingerprint
		}
		pol.Normalize()
		if err := pol.Validate(); err != nil {
			writeErr(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		a.MCPPolicy = pol
		if _, rev, err = s.store.UpdateAPIAtomic(r.Context(), a, actor, fmt.Sprintf("Approved MCP %s %s", e.Kind, e.Name)); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := s.store.SetMCPCatalogStatus(r.Context(), e.ID, status, actor); err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, e.TenantID, "mcp_"+e.Kind+":"+action, "api", e.APIID, map[string]any{
		"name": e.Name, "fingerprint": e.Fingerprint, "catalog_entry": e.ID, "revision": rev})
	e.Status = status
	writeJSON(w, http.StatusOK, map[string]any{"entry": e, "revision": rev})
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// syncMCPApprovals keeps the catalog in step with pins edited directly
// (console dialog, API, APIOps).
func (s *Server) syncMCPApprovals(ctx context.Context, a store.API, actor string) {
	if a.Protocol != "mcp" {
		return
	}
	for kind, pins := range map[string]map[string]string{mcpgw.KindTool: a.MCPPolicy.PinnedTools, mcpgw.KindPrompt: a.MCPPolicy.PinnedPrompts} {
		if err := s.store.SyncMCPApprovals(ctx, a.ID, kind, pins, actor); err != nil {
			slog.Warn("sync MCP catalog approvals", "api", a.ID, "err", err)
		}
	}
}

// consoleLink is an absolute console URL when RELAYOPS_PUBLIC_URL is set.
func consoleLink(fragment string) string {
	if base := strings.TrimRight(os.Getenv("RELAYOPS_PUBLIC_URL"), "/"); base != "" {
		return base + fragment
	}
	return ""
}
