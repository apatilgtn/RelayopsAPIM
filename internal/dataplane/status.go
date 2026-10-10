package dataplane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/relayops/apim/internal/gateway"
)

// StatusHandler serves /metrics, /healthz and /readyz for a gateway-only node,
// which runs no admin listener. It never needs the control plane to answer.
func StatusHandler(gw *gateway.Gateway, src *RemoteSource) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeStatus(w, http.StatusOK, statusBody(gw, src, "ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if gw.Snapshot() == nil {
			writeStatus(w, http.StatusServiceUnavailable, statusBody(gw, src, "no_configuration"))
			return
		}
		writeStatus(w, http.StatusOK, statusBody(gw, src, "ready"))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		var rev int64
		routes := 0
		if snap := gw.Snapshot(); snap != nil {
			rev, routes = snap.Version, len(snap.Routes)
		}
		fmt.Fprintf(w, "# HELP relayops_config_revision Configuration revision this node serves\n")
		fmt.Fprintf(w, "# TYPE relayops_config_revision gauge\n")
		fmt.Fprintf(w, "relayops_config_revision %d\n\n", rev)
		fmt.Fprintf(w, "# HELP relayops_gateway_routes Active routes in this node's snapshot\n")
		fmt.Fprintf(w, "# TYPE relayops_gateway_routes gauge\n")
		fmt.Fprintf(w, "relayops_gateway_routes %d\n\n", routes)
		var last float64
		if t := src.LastSync(); !t.IsZero() {
			last = float64(t.Unix())
		}
		fmt.Fprintf(w, "# HELP relayops_dataplane_last_sync_timestamp_seconds When the control plane last answered this node\n")
		fmt.Fprintf(w, "# TYPE relayops_dataplane_last_sync_timestamp_seconds gauge\n")
		fmt.Fprintf(w, "relayops_dataplane_last_sync_timestamp_seconds %.0f\n\n", last)
		fmt.Fprintf(w, "# HELP relayops_dataplane_sync_errors_total Failed exchanges with the control plane\n")
		fmt.Fprintf(w, "# TYPE relayops_dataplane_sync_errors_total counter\n")
		fmt.Fprintf(w, "relayops_dataplane_sync_errors_total %d\n\n", src.SyncErrors())
		fmt.Fprintf(w, "# HELP relayops_dataplane_config_updates_total Changed configurations received from the control plane\n")
		fmt.Fprintf(w, "# TYPE relayops_dataplane_config_updates_total counter\n")
		fmt.Fprintf(w, "relayops_dataplane_config_updates_total %d\n\n", src.Updates())
		if c := gw.Collector(); c != nil {
			c.WritePrometheusMetrics(w)
		}
		gw.WriteUpstreamMetrics(w)
	})
	return mux
}

func statusBody(gw *gateway.Gateway, src *RemoteSource, status string) map[string]any {
	body := map[string]any{"status": status, "role": "gateway", "node_id": gw.NodeID()}
	if snap := gw.Snapshot(); snap != nil {
		body["config_revision"] = snap.Version
		body["routes"] = len(snap.Routes)
	}
	if t := src.LastSync(); !t.IsZero() {
		body["control_plane_last_sync"] = t.UTC().Format(time.RFC3339)
	}
	return body
}

func writeStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
