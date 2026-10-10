package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/graphql"
	"github.com/relayops/apim/internal/store"
)

type gatewayTargetStatus = gateway.TargetStatus

// ---------------------------------------------------------------------------
// OpenAPI Contract Difference Endpoints
// ---------------------------------------------------------------------------

func (s *Server) apiContractDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, ok := s.scopedAPI(w, r, id)
	if !ok {
		return
	}

	raw, err := ioReadAllLimit(r.Body, 10<<20)
	if err != nil || len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_spec", "proposed spec is required in body")
		return
	}

	if existing.Protocol == "graphql" {
		proposedSchema := string(raw)
		var wrapper struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &wrapper) == nil && wrapper.Schema != "" {
			proposedSchema = wrapper.Schema
		}
		diff := graphql.DiffSchemas(existing.GraphQLSchema, proposedSchema)
		writeJSON(w, http.StatusOK, map[string]any{
			"protocol":         "graphql",
			"has_breaking":     diff.HasBreaking,
			"breaking_changes": diff.BreakingChanges,
			"safe_changes":     diff.SafeChanges,
			"total_changes":    len(diff.BreakingChanges) + len(diff.SafeChanges),
		})
		return
	}

	propDoc, err := ParseSpec(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "parse_error", err.Error())
		return
	}

	baseDoc := existing.OpenAPISpec
	if baseDoc == nil || len(baseDoc) == 0 {
		baseDoc = map[string]any{
			"openapi": "3.0.0",
			"paths":   map[string]any{},
		}
	}

	report := CompareOpenAPISpecs(baseDoc, propDoc)
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) validateContract(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BaseSpec     any `json:"base_spec"`
		ProposedSpec any `json:"proposed_spec"`
	}
	if !decode(w, r, &in) {
		return
	}

	baseMap, _ := in.BaseSpec.(map[string]any)
	propMap, _ := in.ProposedSpec.(map[string]any)

	report := CompareOpenAPISpecs(baseMap, propMap)
	writeJSON(w, http.StatusOK, report)
}

// ---------------------------------------------------------------------------
// Prometheus Metrics Exporter (GET /metrics)
// ---------------------------------------------------------------------------

func (s *Server) prometheusMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	var snap *gateway.Snapshot
	if s.gw != nil {
		snap = s.gw.Snapshot()
	}
	var rev int64 = 0
	routes := 0
	if snap != nil {
		rev = snap.Version
		routes = len(snap.Routes)
	}

	// Fleet state comes from the database. A scrape must never hang on, or fail
	// because of, a database outage: bound the queries and report availability.
	var fleet store.FleetStatus
	dbUp := 0
	if s.dbAvailable() {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		if f, err := s.store.GetFleetStatus(ctx); err == nil {
			fleet, dbUp = f, 1
		}
		cancel()
	}
	nodeCount := len(fleet.Nodes)

	fmt.Fprintf(w, "# HELP relayops_control_plane_database_up Whether this control plane can reach its database (1=up)\n")
	fmt.Fprintf(w, "# TYPE relayops_control_plane_database_up gauge\n")
	fmt.Fprintf(w, "relayops_control_plane_database_up %d\n\n", dbUp)

	fmt.Fprintf(w, "# HELP relayops_config_revision Current cluster configuration revision number\n")
	fmt.Fprintf(w, "# TYPE relayops_config_revision gauge\n")
	fmt.Fprintf(w, "relayops_config_revision %d\n\n", rev)

	fmt.Fprintf(w, "# HELP relayops_gateway_routes Total active routes configured in gateway snapshot\n")
	fmt.Fprintf(w, "# TYPE relayops_gateway_routes gauge\n")
	fmt.Fprintf(w, "relayops_gateway_routes %d\n\n", routes)

	fmt.Fprintf(w, "# HELP relayops_fleet_nodes_count Total registered gateway nodes in fleet\n")
	fmt.Fprintf(w, "# TYPE relayops_fleet_nodes_count gauge\n")
	fmt.Fprintf(w, "relayops_fleet_nodes_count %d\n\n", nodeCount)

	fmt.Fprintf(w, "# HELP relayops_fleet_converged Fleet convergence state (1=converged, 0=syncing)\n")
	fmt.Fprintf(w, "# TYPE relayops_fleet_converged gauge\n")
	convergedVal := 0
	if fleet.Converged {
		convergedVal = 1
	}
	fmt.Fprintf(w, "relayops_fleet_converged %d\n\n", convergedVal)
	s.writeOrbitMetrics(w)

	// Trustworthy cumulative counters & standard latency histograms from Collector
	if s.gw != nil && s.gw.Collector() != nil {
		s.gw.Collector().WritePrometheusMetrics(w)
	}
	if s.gw != nil {
		s.gw.WriteUpstreamMetrics(w)
	}
	if fleet.CanaryRevision > 0 {
		fmt.Fprintf(w, "# HELP relayops_canary_revision In-flight canary revision (0 when none)\n")
		fmt.Fprintf(w, "# TYPE relayops_canary_revision gauge\n")
		fmt.Fprintf(w, "relayops_canary_revision %d\n\n", fleet.CanaryRevision)
	}
}

func ioReadAllLimit(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}
