package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/relayops/apim/internal/alerts"
	"github.com/relayops/apim/internal/certs"
	"github.com/relayops/apim/internal/store"
)

// fleetCertificate is one certificate and the nodes that hold it.
type fleetCertificate struct {
	certs.Status
	Severity string   `json:"severity,omitempty"` // critical, warning, info or empty
	Nodes    []string `json:"nodes"`
}

// certificateReport is what GET /api/system/certificates returns.
type certificateReport struct {
	Now          time.Time                `json:"now"`
	Summary      map[string]int           `json:"summary"`
	Certificates []fleetCertificate       `json:"certificates"` // unique, soonest expiry first
	Nodes        []store.NodeCertificates `json:"nodes"`
	Warning      string                   `json:"warning,omitempty"`
}

// buildCertificateReport merges the nodes' reports with this node's own
// certificates, which are always current (a control-plane node does not
// acknowledge configuration, so it never reports itself).
func (s *Server) buildCertificateReport(ctx context.Context) certificateReport {
	now := time.Now().UTC()
	rep := certificateReport{Now: now, Nodes: []store.NodeCertificates{}}
	if s.store != nil && s.store.Pool != nil {
		nodes, err := s.store.ListNodeCertificates(ctx, store.NodeLivenessWindow)
		if err != nil {
			rep.Warning = "fleet reports are unavailable; showing this node only"
			slog.Warn("certificate report", "err", err)
		} else {
			rep.Nodes = nodes
		}
	}
	if s.gw != nil {
		local := store.NodeCertificates{NodeID: s.nodeID, ReportedAt: now, Certificates: s.gw.Certificates()}
		replaced := false
		for i := range rep.Nodes {
			if rep.Nodes[i].NodeID == s.nodeID {
				rep.Nodes[i], replaced = local, true
			}
		}
		if !replaced && len(local.Certificates) > 0 {
			rep.Nodes = append(rep.Nodes, local)
		}
	}
	// One row per place a certificate is used (role, listener, hostname or
	// API), listing each node holding it there once.
	byKey := map[string]*fleetCertificate{}
	seen := map[string]bool{}
	for _, n := range rep.Nodes {
		for _, c := range n.Certificates {
			k := c.Fingerprint + "|" + c.Source + "|" + c.Listener + "|" + c.Name + "|" + c.APIID
			fc, ok := byKey[k]
			if !ok {
				fc = &fleetCertificate{Status: c, Severity: certs.Severity(c, now)}
				byKey[k] = fc
			}
			if !seen[k+"|"+n.NodeID] {
				seen[k+"|"+n.NodeID] = true
				fc.Nodes = append(fc.Nodes, n.NodeID)
			}
		}
	}
	// The summary counts distinct certificates, not places of use.
	severity := map[string]string{}
	for _, fc := range byKey {
		sort.Strings(fc.Nodes)
		rep.Certificates = append(rep.Certificates, *fc)
		severity[fc.Fingerprint] = fc.Severity
	}
	rep.Summary = map[string]int{"total": len(severity), "expired": 0, "expiring_14d": 0, "expiring_30d": 0}
	for _, sev := range severity {
		switch sev {
		case "critical":
			rep.Summary["expired"]++
		case "warning":
			rep.Summary["expiring_14d"]++
		case "info":
			rep.Summary["expiring_30d"]++
		}
	}
	sort.SliceStable(rep.Certificates, func(i, j int) bool {
		a, b := rep.Certificates[i], rep.Certificates[j]
		if !a.NotAfter.Equal(b.NotAfter) {
			return a.NotAfter.Before(b.NotAfter)
		}
		return a.Name < b.Name
	})
	if rep.Certificates == nil {
		rep.Certificates = []fleetCertificate{}
	}
	return rep
}

func (s *Server) listCertificates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildCertificateReport(r.Context()))
}

// certificateLabel names a certificate for people.
func certificateLabel(c certs.Status) string {
	switch c.Source {
	case certs.SourceUpstreamCA:
		return fmt.Sprintf("upstream CA %q of API %s", c.Subject, c.Name)
	case certs.SourceUpstreamClient:
		return fmt.Sprintf("upstream client certificate of API %s", c.Name)
	case certs.SourceClientCA:
		return fmt.Sprintf("%s client CA %q (%s)", c.Listener, c.Subject, c.Name)
	case certs.SourceDataplaneClient:
		return "gateway node certificate for the control plane"
	}
	return fmt.Sprintf("%s certificate for %s", c.Listener, c.Name)
}

func expiryPhrase(c certs.Status, now time.Time) string {
	left := c.NotAfter.Sub(now)
	switch {
	case now.Before(c.NotBefore):
		return "is not valid until " + c.NotBefore.Format(time.RFC3339)
	case left <= 0:
		return "expired " + c.NotAfter.Format("2 Jan 2006")
	case left < 48*time.Hour:
		return fmt.Sprintf("expires in %d hours", int(left.Hours()))
	}
	return fmt.Sprintf("expires in %d days (%s)", int(left.Hours()/24), c.NotAfter.Format("2 Jan 2006"))
}

// StartCertificateMonitor alerts (console and webhook) about certificates
// that have expired or expire within certs.WarnWithin. Each certificate is
// alerted at most once a day per severity across control-plane replicas.
func (s *Server) StartCertificateMonitor(ctx context.Context, interval time.Duration) {
	timer := time.NewTimer(time.Minute) // let gateways report first
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.checkCertificateExpiry(ctx)
		timer.Reset(interval)
	}
}

func (s *Server) checkCertificateExpiry(ctx context.Context) {
	rep := s.buildCertificateReport(ctx)
	// One alert per certificate, naming every place it is used.
	type group struct {
		first fleetCertificate
		uses  []string
		nodes map[string]bool
	}
	var order []string
	groups := map[string]*group{}
	for _, c := range rep.Certificates {
		if c.Severity != "critical" && c.Severity != "warning" {
			continue
		}
		g, ok := groups[c.Fingerprint]
		if !ok {
			g = &group{first: c, nodes: map[string]bool{}}
			groups[c.Fingerprint] = g
			order = append(order, c.Fingerprint)
		}
		g.uses = append(g.uses, certificateLabel(c.Status))
		for _, n := range c.Nodes {
			g.nodes[n] = true
		}
	}
	for _, fp := range order {
		g := groups[fp]
		c := g.first
		claimed, err := s.store.ClaimCertificateAlert(ctx, fp, c.Severity)
		if err != nil || !claimed {
			continue
		}
		nodes := make([]string, 0, len(g.nodes))
		for n := range g.nodes {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		s.alerts.Notify(alerts.Alert{
			Type:     "certificate_expiring",
			Severity: c.Severity,
			Title:    "Certificate " + expiryPhrase(c.Status, rep.Now),
			Text: fmt.Sprintf("The certificate %q %s. Used as: %s. Nodes: %s. Serial %s, issuer %s.",
				c.Subject, expiryPhrase(c.Status, rep.Now), strings.Join(g.uses, "; "), strings.Join(nodes, ", "), c.Serial, c.Issuer),
			Link: "#/fleet",
			Details: map[string]any{
				"subject": c.Subject, "uses": g.uses, "not_after": c.NotAfter,
				"fingerprint_sha256": fp, "nodes": nodes,
			},
		})
	}
}

// certificateSignals turns the certificate report into Orbit signals: one
// per severity, naming the certificates concerned.
func certificateSignals(rep certificateReport) []Signal {
	groups := map[string][]fleetCertificate{}
	for _, c := range rep.Certificates {
		if c.Severity != "" {
			groups[c.Severity] = append(groups[c.Severity], c)
		}
	}
	titles := map[string]string{
		"critical": "%d certificate(s) expired or not yet valid",
		"warning":  "%d certificate(s) expire within 14 days",
		"info":     "%d certificate(s) expire within 30 days",
	}
	var out []Signal
	for _, sev := range []string{"critical", "warning", "info"} {
		list := groups[sev]
		if len(list) == 0 {
			continue
		}
		var parts []string
		for i, c := range list {
			if i == 4 {
				parts = append(parts, fmt.Sprintf("and %d more", len(list)-4))
				break
			}
			parts = append(parts, certificateLabel(c.Status)+" "+expiryPhrase(c.Status, rep.Now))
		}
		distinct := map[string]bool{}
		for _, c := range list {
			distinct[c.Fingerprint] = true
		}
		out = append(out, Signal{
			ID: "certificates:" + sev, Severity: sev, Kind: "certificates",
			Title:  fmt.Sprintf(titles[sev], len(distinct)),
			Detail: strings.Join(parts, "; ") + ".",
			Link:   "#/fleet",
			Ask:    "Which certificates are close to expiry or expired, where are they used, and how do I renew them?",
			Evidence: map[string]any{
				"certificates": len(distinct),
				"uses":         len(list),
			},
		})
	}
	return out
}
