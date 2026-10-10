package gateway

import (
	"bytes"
	"context"
	"log/slog"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/relayops/apim/internal/store"
)

// mcpRouteState is an MCP API's catalog state from the control plane.
type mcpRouteState struct {
	// blocked holds, per kind|name, definitions that are awaiting review or
	// were rejected. A pinned item whose server now offers one of them is
	// refused on every node.
	blocked map[string][]string
	// schemas are the approved tools' input schemas (for the pinned
	// definition), used to validate tools/call arguments.
	schemas map[string]*jsonschema.Schema
}

func (m *mcpRouteState) isBlocked(kind, name, pin string) bool {
	if m == nil || pin == "" {
		return false
	}
	for _, fp := range m.blocked[kind+"|"+name] {
		if fp != pin {
			return true
		}
	}
	return false
}

func (m *mcpRouteState) schema(tool string) *jsonschema.Schema {
	if m == nil {
		return nil
	}
	return m.schemas[tool]
}

// attachMCPCatalog gives each MCP route its catalog state.
func attachMCPCatalog(s *Snapshot, recs []store.MCPCatalogRecord) {
	if s == nil {
		return
	}
	byAPI := map[string][]store.MCPCatalogRecord{}
	for _, r := range recs {
		byAPI[r.APIID] = append(byAPI[r.APIID], r)
	}
	for _, rt := range s.Routes {
		if rt.API.Protocol != "mcp" {
			continue
		}
		st := &mcpRouteState{blocked: map[string][]string{}, schemas: map[string]*jsonschema.Schema{}}
		for _, r := range byAPI[rt.API.ID] {
			switch r.Status {
			case "pending", "rejected":
				st.blocked[r.Kind+"|"+r.Name] = append(st.blocked[r.Kind+"|"+r.Name], r.Fingerprint)
			case "approved":
				if r.Kind != "tool" || len(r.InputSchema) == 0 || rt.API.MCPPolicy.PinnedTools[r.Name] != r.Fingerprint {
					continue
				}
				sch, err := compileSchema(r.InputSchema)
				if err != nil {
					slog.Warn("MCP tool input schema does not compile; arguments are not validated", "api", rt.API.Name, "tool", r.Name, "err", err)
					continue
				}
				st.schemas[r.Name] = sch
			}
		}
		rt.mcp = st
	}
}

func compileSchema(raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("mem:///input.json", doc); err != nil {
		return nil, err
	}
	return c.Compile("mem:///input.json")
}

// MCPReporter receives MCP definitions that need review (changed or new
// tools and prompts on APIs with an approved catalog).
type MCPReporter interface {
	ReportMCPObservations(ctx context.Context, obs []store.MCPObservation) error
}

// SetMCPReporter starts reporting observed definitions to the control
// plane. Reports are queued, de-duplicated (each definition at most once an
// hour per node) and sent in the background, off the request path.
func (g *Gateway) SetMCPReporter(r MCPReporter) {
	g.mcpObs = make(chan store.MCPObservation, 1000)
	go g.runMCPReporter(r)
}

func (g *Gateway) observeMCP(o store.MCPObservation) {
	if g.mcpObs == nil {
		return
	}
	select {
	case g.mcpObs <- o:
	default: // queue full: the next tools/list reports it again
	}
}

func (g *Gateway) runMCPReporter(r MCPReporter) {
	sent := map[string]time.Time{}
	var pending []store.MCPObservation
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case o := <-g.mcpObs:
			key := o.APIID + "|" + o.Kind + "|" + o.Name + "|" + o.Fingerprint
			if t, ok := sent[key]; ok && time.Since(t) < time.Hour {
				continue
			}
			sent[key] = time.Now()
			if len(pending) < 500 {
				pending = append(pending, o)
			}
		case <-tick.C:
			if len(pending) == 0 {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := r.ReportMCPObservations(ctx, pending)
			cancel()
			if err != nil {
				slog.Warn("report MCP definitions for review", "count", len(pending), "err", err)
				continue // retried on the next tick
			}
			pending = nil
			if len(sent) > 10000 {
				clear(sent)
			}
		}
	}
}
