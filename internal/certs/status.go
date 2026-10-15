// Package certs describes, loads and checks the X.509 certificates a node
// holds: what each certificate is, when it expires, whether a client
// certificate was revoked, and how public certificates are issued with ACME.
// It never reports or logs private keys.
package certs

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Certificate sources.
const (
	SourceListener        = "listener"         // a listener's default certificate
	SourceListenerHost    = "listener_host"    // a per-hostname listener certificate (SNI)
	SourceClientCA        = "client_ca"        // a CA client certificates must chain to
	SourceACME            = "acme"             // issued by the ACME CA
	SourceUpstreamCA      = "upstream_ca"      // a CA an API trusts for its upstream
	SourceUpstreamClient  = "upstream_client"  // the client certificate an API presents upstream
	SourceDataplaneClient = "dataplane_client" // a gateway node's certificate for the control plane
)

// Certificate roles.
const (
	RoleServer = "server"
	RoleClient = "client"
	RoleCA     = "ca"
)

// Status describes one certificate. It carries no key material.
type Status struct {
	Source      string    `json:"source"`
	Listener    string    `json:"listener,omitempty"` // gateway, admin or relay
	Name        string    `json:"name"`               // hostname, API name or "default"
	APIID       string    `json:"api_id,omitempty"`
	Role        string    `json:"role"`
	Subject     string    `json:"subject"`
	DNSNames    []string  `json:"dns_names,omitempty"`
	Issuer      string    `json:"issuer"`
	Serial      string    `json:"serial"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	Fingerprint string    `json:"fingerprint_sha256"`
}

// Summarize describes c.
func Summarize(c *x509.Certificate, source, name, role string) Status {
	fp := sha256.Sum256(c.Raw)
	return Status{
		Source:      source,
		Name:        name,
		Role:        role,
		Subject:     c.Subject.String(),
		DNSNames:    c.DNSNames,
		Issuer:      c.Issuer.String(),
		Serial:      c.SerialNumber.Text(16),
		NotBefore:   c.NotBefore.UTC(),
		NotAfter:    c.NotAfter.UTC(),
		Fingerprint: hex.EncodeToString(fp[:]),
	}
}

// Leaf returns the parsed leaf of c, or nil.
func Leaf(c *tls.Certificate) *x509.Certificate {
	if c == nil || len(c.Certificate) == 0 {
		return nil
	}
	if c.Leaf != nil {
		return c.Leaf
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

// ParseCertificates returns every certificate in a PEM bundle.
func ParseCertificates(pemData []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for rest := pemData; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no certificates found")
	}
	return out, nil
}

// Pool builds a certificate pool from parsed certificates.
func Pool(list []*x509.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	for _, c := range list {
		p.AddCert(c)
	}
	return p
}

// Inventory collects the certificates a node holds from several sources and
// counts certificate reloads. It is safe for concurrent use.
type Inventory struct {
	mu      sync.Mutex
	sources []func() []Status
	reloads map[[2]string]*atomic.Int64 // listener, result
	extra   []func(io.Writer)
}

// NewInventory returns an empty inventory.
func NewInventory() *Inventory {
	return &Inventory{reloads: map[[2]string]*atomic.Int64{}}
}

// Register adds a source of certificates.
func (inv *Inventory) Register(src func() []Status) {
	if inv == nil || src == nil {
		return
	}
	inv.mu.Lock()
	inv.sources = append(inv.sources, src)
	inv.mu.Unlock()
}

// RegisterMetrics adds a writer of extra Prometheus series.
func (inv *Inventory) RegisterMetrics(fn func(io.Writer)) {
	if inv == nil || fn == nil {
		return
	}
	inv.mu.Lock()
	inv.extra = append(inv.extra, fn)
	inv.mu.Unlock()
}

// CountReload records a reload attempt of a listener's certificates.
func (inv *Inventory) CountReload(listener, result string) {
	if inv == nil {
		return
	}
	k := [2]string{listener, result}
	inv.mu.Lock()
	c, ok := inv.reloads[k]
	if !ok {
		c = new(atomic.Int64)
		inv.reloads[k] = c
	}
	inv.mu.Unlock()
	c.Add(1)
}

// MaxReported bounds the certificates one node reports, so a large
// certificate directory cannot grow a heartbeat without limit.
const MaxReported = 256

// List returns every certificate, soonest expiry first, without duplicates.
func (inv *Inventory) List() []Status {
	if inv == nil {
		return nil
	}
	inv.mu.Lock()
	srcs := slices.Clone(inv.sources)
	inv.mu.Unlock()
	seen := map[string]bool{}
	var out []Status
	for _, src := range srcs {
		for _, s := range src() {
			k := s.Source + "|" + s.Listener + "|" + s.Name + "|" + s.APIID + "|" + s.Fingerprint
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].NotAfter.Equal(out[j].NotAfter) {
			return out[i].NotAfter.Before(out[j].NotAfter)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Reported returns at most MaxReported certificates, soonest expiry first.
func (inv *Inventory) Reported() []Status {
	l := inv.List()
	if len(l) > MaxReported {
		l = l[:MaxReported]
	}
	return l
}

// WriteMetrics renders the inventory as Prometheus series.
func (inv *Inventory) WriteMetrics(w io.Writer) {
	if inv == nil {
		return
	}
	list := inv.List()
	if len(list) > 0 {
		fmt.Fprintf(w, "# HELP relayops_tls_certificate_not_after_timestamp_seconds Expiry (notAfter) of each certificate this node holds, as a Unix timestamp\n")
		fmt.Fprintf(w, "# TYPE relayops_tls_certificate_not_after_timestamp_seconds gauge\n")
		for _, s := range list {
			// The fingerprint keeps series unique: certificates in one
			// bundle can share a serial number (different issuers).
			fmt.Fprintf(w, "relayops_tls_certificate_not_after_timestamp_seconds{source=%s,listener=%s,name=%s,api_id=%s,role=%s,serial=%s,fingerprint_sha256=%s} %d\n",
				label(s.Source), label(s.Listener), label(s.Name), label(s.APIID), label(s.Role), label(s.Serial), label(s.Fingerprint), s.NotAfter.Unix())
		}
		fmt.Fprintln(w)
	}
	inv.mu.Lock()
	type kv struct {
		k [2]string
		v int64
	}
	var reloads []kv
	for k, v := range inv.reloads {
		reloads = append(reloads, kv{k, v.Load()})
	}
	extra := slices.Clone(inv.extra)
	inv.mu.Unlock()
	if len(reloads) > 0 {
		sort.Slice(reloads, func(i, j int) bool {
			return reloads[i].k[0]+reloads[i].k[1] < reloads[j].k[0]+reloads[j].k[1]
		})
		fmt.Fprintf(w, "# HELP relayops_tls_reloads_total Certificate reloads by listener and result (polls that find unchanged files are not counted)\n")
		fmt.Fprintf(w, "# TYPE relayops_tls_reloads_total counter\n")
		for _, r := range reloads {
			fmt.Fprintf(w, "relayops_tls_reloads_total{listener=%s,result=%s} %d\n", label(r.k[0]), label(r.k[1]), r.v)
		}
		fmt.Fprintln(w)
	}
	for _, fn := range extra {
		fn(w)
	}
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// label quotes a Prometheus label value.
func label(v string) string {
	return `"` + labelEscaper.Replace(v) + `"`
}

// Expiry thresholds for warnings.
const (
	WarnWithin = 14 * 24 * time.Hour
	InfoWithin = 30 * 24 * time.Hour
)

// Severity classifies a certificate's expiry at now: "critical" when expired
// or not yet valid, "warning" within WarnWithin, "info" within InfoWithin,
// and "" otherwise.
func Severity(s Status, now time.Time) string {
	left := s.NotAfter.Sub(now)
	switch {
	case left <= 0 || now.Before(s.NotBefore):
		return "critical"
	case left <= WarnWithin:
		return "warning"
	case left <= InfoWithin:
		return "info"
	}
	return ""
}

// maxReportedDNSNames bounds the names reported per certificate.
const maxReportedDNSNames = 8

// Budget returns a prefix of list (soonest expiry first, as List orders it)
// whose JSON encoding stays within maxBytes, with at most a few DNS names
// per certificate, so a report always fits the request that carries it.
func Budget(list []Status, maxBytes int) []Status {
	out := make([]Status, 0, len(list))
	used := 2 // []
	for _, s := range list {
		if len(s.DNSNames) > maxReportedDNSNames {
			s.DNSNames = append(append([]string(nil), s.DNSNames[:maxReportedDNSNames-1]...),
				fmt.Sprintf("(%d more)", len(s.DNSNames)-maxReportedDNSNames+1))
		}
		raw, err := json.Marshal(s)
		if err != nil {
			continue
		}
		if used+len(raw)+1 > maxBytes {
			break
		}
		used += len(raw) + 1
		out = append(out, s)
	}
	return out
}

// Digest identifies a certificate report, to tell whether it changed.
func Digest(list []Status) [32]byte {
	raw, _ := json.Marshal(list)
	return sha256.Sum256(raw)
}
