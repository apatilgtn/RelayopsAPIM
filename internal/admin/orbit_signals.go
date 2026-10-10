package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relayops/apim/internal/store"
)

// Orbit signals: what needs attention, found by fixed rules over data the
// asking user may already see. No model is involved in deciding what is a
// signal; Orbit only explains one when the user asks.

// Signal is one finding shown in the console.
type Signal struct {
	ID       string         `json:"id"`
	Severity string         `json:"severity"` // critical, warning, info
	Kind     string         `json:"kind"`
	Title    string         `json:"title"`
	Detail   string         `json:"detail"`
	Link     string         `json:"link"` // console route, e.g. #/refusals
	Ask      string         `json:"ask"`  // question to hand to Orbit
	Evidence map[string]any `json:"evidence,omitempty"`
}

type signalCache struct {
	mu      sync.Mutex
	entries map[string]signalEntry
}

type signalEntry struct {
	at      time.Time
	signals []Signal
}

const signalCacheTTL = time.Minute

func (s *Server) orbitSignals(w http.ResponseWriter, r *http.Request) {
	// Per credential and tenant: different users and tenants see different data.
	key := store.HashKey(r.Header.Get("Authorization") + "|" + r.Header.Get(TenantHeader))
	if r.URL.Query().Get("refresh") == "" {
		s.signals.mu.Lock()
		if e, ok := s.signals.entries[key]; ok && time.Since(e.at) < signalCacheTTL {
			s.signals.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"signals": e.signals, "generated_at": e.at})
			return
		}
		s.signals.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	out := s.detectSignals(ctx, r)
	now := time.Now()
	s.signals.mu.Lock()
	if s.signals.entries == nil || len(s.signals.entries) > 500 {
		s.signals.entries = map[string]signalEntry{}
	}
	s.signals.entries[key] = signalEntry{at: now, signals: out}
	s.signals.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"signals": out, "generated_at": now})
}

// fetch reads one API as the user; a permission error or failure yields nil,
// so a role that cannot see a source simply gets no signals from it.
func (s *Server) fetchAs(ctx context.Context, r *http.Request, path string, into any) bool {
	raw, err := s.orbitGet(ctx, r, path, nil)
	if err != nil {
		return false
	}
	return json.Unmarshal([]byte(raw), into) == nil
}

type topItem struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Count      float64 `json:"count"`
	Errors     float64 `json:"errors"`
	AvgLatency float64 `json:"avg_latency_ms"`
}

type apiStatsLite struct {
	APIs []topItem `json:"apis"`
}

func (s *Server) detectSignals(ctx context.Context, r *http.Request) []Signal {
	var out []Signal
	add := func(sig Signal) { out = append(out, sig) }

	// 1. Error and latency spikes per API: last 15 minutes against 24 hours.
	var recent, base apiStatsLite
	if s.fetchAs(ctx, r, "/api/analytics/apis?window=15m", &recent) && s.fetchAs(ctx, r, "/api/analytics/apis?window=24h", &base) {
		baseline := map[string]topItem{}
		for _, a := range base.APIs {
			baseline[a.ID] = a
		}
		for _, a := range recent.APIs {
			b := baseline[a.ID]
			out = append(out, apiSpikeSignals(a, b)...)
		}
	}

	// 2. Refusal surges in the last hour.
	var ref struct {
		ByReason []struct {
			Protocol string  `json:"protocol"`
			Reason   string  `json:"reason"`
			APIName  string  `json:"api_name"`
			Count    float64 `json:"count"`
		} `json:"by_reason"`
	}
	if s.fetchAs(ctx, r, "/api/analytics/refusals?window=1h", &ref) {
		for _, g := range ref.ByReason {
			if g.Count < 20 {
				continue
			}
			sev := "info"
			if g.Count >= 200 {
				sev = "warning"
			}
			api := g.APIName
			if api == "" {
				api = "an unmatched route"
			}
			add(Signal{ID: "refusals:" + g.APIName + ":" + g.Reason, Severity: sev, Kind: "refusals",
				Title:    fmt.Sprintf("%s refused %.0f requests in the last hour", api, g.Count),
				Detail:   fmt.Sprintf("Reason: %s (%s). Refusals are policy decisions, not outages, but a surge can mean a misconfigured client or an attack.", g.Reason, strings.ToUpper(g.Protocol)),
				Link:     "#/refusals",
				Ask:      fmt.Sprintf("Why did %s refuse %.0f requests with %s in the last hour, and should we change anything?", api, g.Count, g.Reason),
				Evidence: map[string]any{"api": g.APIName, "reason": g.Reason, "count": g.Count, "window": "1h"}})
		}
	}

	// 3. Releases: recent automatic rollback, canary in flight.
	var ar struct {
		LastRev    int64     `json:"last_triggered_revision"`
		LastAt     time.Time `json:"last_triggered_at"`
		LastReason string    `json:"last_triggered_reason"`
		Canary     int64     `json:"current_canary_revision"`
		Enabled    bool      `json:"enabled"`
	}
	if s.fetchAs(ctx, r, "/api/revisions/auto-rollback/config", &ar) {
		if !ar.LastAt.IsZero() && time.Since(ar.LastAt) < 24*time.Hour {
			add(Signal{ID: fmt.Sprintf("rollback:%d", ar.LastRev), Severity: "warning", Kind: "rollback",
				Title:  fmt.Sprintf("Revision %d was rolled back automatically %s", ar.LastRev, ago(ar.LastAt)),
				Detail: firstSentence(ar.LastReason), Link: "#/fleet",
				Ask: fmt.Sprintf("Why was revision %d rolled back automatically, and is it safe to try that change again?", ar.LastRev)})
		}
		if ar.Canary > 0 {
			add(Signal{ID: fmt.Sprintf("canary:%d", ar.Canary), Severity: "info", Kind: "canary",
				Title:  fmt.Sprintf("Canary revision %d is in flight", ar.Canary),
				Detail: "A share of callers receives the candidate. Promote or abort it under Releases & fleet.", Link: "#/fleet",
				Ask: fmt.Sprintf("Is canary revision %d healthy enough to promote?", ar.Canary)})
		}
		if !ar.Enabled {
			add(Signal{ID: "rollback:disabled", Severity: "info", Kind: "rollback",
				Title:  "Automatic rollback is turned off",
				Detail: "Bad releases will keep serving until someone rolls them back by hand.", Link: "#/fleet",
				Ask: "Should we turn automatic rollback on, and with what threshold?"})
		}
	}
	var fleet struct {
		Target    int64 `json:"target_revision"`
		Converged bool  `json:"converged"`
		Nodes     []struct {
			NodeID   string `json:"node_id"`
			Revision int64  `json:"revision"`
			IsCanary bool   `json:"is_canary"`
		} `json:"nodes"`
	}
	if s.fetchAs(ctx, r, "/api/fleet/status", &fleet) && !fleet.Converged && len(fleet.Nodes) > 0 {
		var behind []string
		for _, n := range fleet.Nodes {
			if !n.IsCanary && n.Revision != fleet.Target {
				behind = append(behind, fmt.Sprintf("%s (rev %d)", n.NodeID, n.Revision))
			}
		}
		if len(behind) > 0 {
			add(Signal{ID: "fleet:drift", Severity: "warning", Kind: "fleet",
				Title:  fmt.Sprintf("%d gateway(s) are not on revision %d", len(behind), fleet.Target),
				Detail: "Behind: " + strings.Join(behind, ", ") + ". They may be disconnected from the control plane.", Link: "#/fleet",
				Ask: "Why are some gateways not on the current revision?"})
		}
	}

	// 4. Governance: MCP definitions waiting for review.
	var pending []struct {
		APIID string `json:"api_id"`
		Name  string `json:"name"`
		Kind  string `json:"kind"`
	}
	if s.fetchAs(ctx, r, "/api/mcp/catalog?status=pending", &pending) && len(pending) > 0 {
		names := make([]string, 0, 3)
		for i, p := range pending {
			if i == 3 {
				break
			}
			names = append(names, p.Name)
		}
		add(Signal{ID: "mcp:pending", Severity: "warning", Kind: "governance",
			Title:  fmt.Sprintf("%d MCP definition(s) held for review", len(pending)),
			Detail: "Calls to them are refused until approved: " + strings.Join(names, ", ") + ". Read the definitions first: a changed description can carry instructions to the model.",
			Link:   "#/approvals", Ask: "Which MCP definitions are held for review and what changed in them?"})
	}

	// 5. Exposure: enabled APIs with neither authentication nor a rate limit.
	var apis []struct {
		Name     string `json:"name"`
		AuthType string `json:"auth_type"`
		Rate     int    `json:"rate_limit_per_minute"`
		Enabled  bool   `json:"enabled"`
		IsDraft  bool   `json:"is_draft"`
		Protocol string `json:"protocol"`
	}
	if s.fetchAs(ctx, r, "/api/apis", &apis) {
		var open []string
		for _, a := range apis {
			if a.Enabled && !a.IsDraft && (a.AuthType == "" || a.AuthType == "none") && a.Rate == 0 {
				open = append(open, a.Name)
			}
		}
		if len(open) > 0 {
			shown := open
			if len(shown) > 4 {
				shown = append(append([]string{}, open[:4]...), fmt.Sprintf("and %d more", len(open)-4))
			}
			add(Signal{ID: "exposure:open", Severity: "info", Kind: "security",
				Title:  fmt.Sprintf("%d API(s) have no authentication and no rate limit", len(open)),
				Detail: "Anyone can call " + strings.Join(shown, ", ") + " as fast as they like. Fine for public catalogues; risky for anything with cost or side effects.",
				Link:   "#/apis", Ask: "Which of our APIs with no authentication and no rate limit should get a rate limit, and what limit would you propose?"})
		}
	}

	rank := map[string]int{"critical": 0, "warning": 1, "info": 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Severity] < rank[out[j].Severity] })
	if out == nil {
		out = []Signal{}
	}
	return out
}

// apiSpikeSignals compares one API's last 15 minutes with its last 24 hours.
func apiSpikeSignals(a, b topItem) []Signal {
	var out []Signal
	if a.Count < 20 {
		return nil // too little traffic to judge
	}
	rate := a.Errors / a.Count
	// The 24-hour window includes the last 15 minutes; compare against the
	// rest of it, or a sustained outage hides in its own baseline.
	priorCount, priorErrors := b.Count-a.Count, b.Errors-a.Errors
	newAPI := priorCount < 20 // no meaningful history: judge on the absolute rate
	baseRate := 0.0
	if !newAPI {
		baseRate = max(priorErrors, 0) / priorCount
	}
	if a.Errors >= 5 && rate >= 0.02 && (rate >= 3*baseRate && (!newAPI || rate >= 0.05)) {
		sev := "warning"
		if rate >= 0.10 {
			sev = "critical"
		}
		out = append(out, Signal{ID: "errors:" + a.ID, Severity: sev, Kind: "errors",
			Title:    fmt.Sprintf("%s: %.1f%% of requests failing (last 15 min)", a.Name, rate*100),
			Detail:   spikeDetail(a, baseRate, newAPI),
			Link:     "#/logs",
			Ask:      fmt.Sprintf("Why is %s returning 5xx errors in the last 15 minutes, and what should we do?", a.Name),
			Evidence: map[string]any{"api_id": a.ID, "errors": a.Errors, "requests": a.Count, "baseline_rate": baseRate}})
	}
	// Latency baseline also excludes the recent window (weighted averages).
	priorLatency := 0.0
	if !newAPI {
		priorLatency = (b.AvgLatency*b.Count - a.AvgLatency*a.Count) / priorCount
	}
	if !newAPI && priorLatency > 0 && a.AvgLatency >= 2*priorLatency && a.AvgLatency-priorLatency >= 100 {
		b.AvgLatency = priorLatency
		out = append(out, Signal{ID: "latency:" + a.ID, Severity: "warning", Kind: "latency",
			Title:    fmt.Sprintf("%s is slower than usual: %.0f ms average (last 15 min)", a.Name, a.AvgLatency),
			Detail:   fmt.Sprintf("Its 24-hour average is %.0f ms. Slowness usually comes from the upstream service.", b.AvgLatency),
			Link:     "#/analytics",
			Ask:      fmt.Sprintf("Why is %s slower than usual in the last 15 minutes?", a.Name),
			Evidence: map[string]any{"api_id": a.ID, "avg_latency_ms": a.AvgLatency, "baseline_latency_ms": b.AvgLatency}})
	}
	return out
}

func spikeDetail(a topItem, baseRate float64, newAPI bool) string {
	d := fmt.Sprintf("%.0f of %.0f requests returned 5xx.", a.Errors, a.Count)
	if newAPI {
		return d + " There is no earlier traffic to compare with: a new or newly used API that is failing."
	}
	return d + fmt.Sprintf(" Before that, over the last 24 hours, the rate was %.2f%%.", baseRate*100)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Format("2 Jan 15:04")
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 && i < 220 {
		return s[:i+1]
	}
	if len(s) > 220 {
		return s[:220] + "…"
	}
	return s
}
