package gateway

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/certs"
	"github.com/relayops/apim/internal/config"
	"github.com/relayops/apim/internal/store"
)

// upstreamTLSError refuses requests to an API whose upstream TLS settings
// cannot be resolved. The gateway never falls back to default trust.
type upstreamTLSError struct{ msg string }

func (e *upstreamTLSError) Error() string { return e.msg }

// upstreamTLS is an API's resolved upstream TLS configuration. Values with
// the same settings and material are shared, so APIs (and the stable and
// canary revisions of one API) with identical settings share connections.
type upstreamTLS struct {
	key      string      // digest of settings and resolved material
	cfg      *tls.Config // never modified after construction
	err      error       // set when the settings cannot be resolved
	statuses []certs.Status
}

// resolveUpstreamTLS reads the secrets named by t. Secrets are read on every
// configuration reload, so a rotated _FILE secret is used from the next
// reload on.
func resolveUpstreamTLS(t *store.UpstreamTLS, cache map[string]*upstreamTLS) *upstreamTLS {
	if t == nil {
		return nil
	}
	fail := func(format string, a ...any) *upstreamTLS {
		return &upstreamTLS{err: &upstreamTLSError{msg: "upstream TLS: " + fmt.Sprintf(format, a...)}}
	}
	read := func(field, ref string) ([]byte, error) {
		if ref == "" {
			return nil, nil
		}
		m := store.SecretReference.FindStringSubmatch(ref)
		if m == nil {
			return nil, fmt.Errorf("%s is not a secret reference", field)
		}
		v, err := config.SecretValue(m[1])
		if err != nil {
			return nil, fmt.Errorf("%s: %v", field, err)
		}
		return []byte(v), nil
	}
	caPEM, err := read("ca", t.CA)
	if err != nil {
		return fail("%v", err)
	}
	certPEM, err := read("client_certificate", t.ClientCertificate)
	if err != nil {
		return fail("%v", err)
	}
	keyPEM, err := read("client_key", t.ClientKey)
	if err != nil {
		return fail("%v", err)
	}

	h := sha256.New()
	for _, part := range [][]byte{[]byte(t.ServerName), []byte(t.MinVersion), caPEM, certPEM, keyPEM} {
		fmt.Fprintf(h, "%d:", len(part))
		h.Write(part)
	}
	key := hex.EncodeToString(h.Sum(nil))
	if u, ok := cache[key]; ok {
		return u
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: t.ServerName}
	if t.MinVersion == "1.3" {
		cfg.MinVersion = tls.VersionTLS13
	}
	var statuses []certs.Status
	if len(caPEM) > 0 {
		list, err := certs.ParseCertificates(caPEM)
		if err != nil {
			return fail("ca: the secret does not contain a PEM certificate")
		}
		cfg.RootCAs = certs.Pool(list)
		for _, c := range list {
			statuses = append(statuses, certs.Summarize(c, certs.SourceUpstreamCA, "", certs.RoleCA))
		}
	}
	if len(certPEM) > 0 {
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			// The parser's message names no key material.
			return fail("client_certificate and client_key: %v", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
		if leaf := certs.Leaf(&pair); leaf != nil {
			statuses = append(statuses, certs.Summarize(leaf, certs.SourceUpstreamClient, "", certs.RoleClient))
		}
	}
	u := &upstreamTLS{key: key, cfg: cfg, statuses: statuses}
	cache[key] = u
	return u
}

// upstreamTLSFor resolves an API's upstream TLS settings, reusing a cached
// configuration when the settings and material are unchanged.
func (r *upstreamRegistry) upstreamTLSFor(t *store.UpstreamTLS) *upstreamTLS {
	r.mu.Lock()
	defer r.mu.Unlock()
	return resolveUpstreamTLS(t, r.tlsCache)
}

// retainTLS drops cached configurations no loaded snapshot uses.
func (r *upstreamRegistry) retainTLS(keep map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.tlsCache {
		if !keep[k] {
			delete(r.tlsCache, k)
		}
	}
}

// tlsTransport returns the shared transport for an API's upstream TLS.
func (g *Gateway) tlsTransport(timeoutMS int, u *upstreamTLS) *http.Transport {
	if u == nil || u.cfg == nil {
		return g.transportFor(timeoutMS)
	}
	return g.transport(transportKey{timeoutMS: timeoutMS, tls: u.key}, u.cfg)
}

// pruneTransports closes transports for upstream TLS configurations no
// loaded snapshot uses any more, such as one replaced by a rotated
// certificate. Requests in flight keep their connections; only idle ones
// are closed.
func (g *Gateway) pruneTransports(keep map[string]bool) {
	g.tmu.Lock()
	defer g.tmu.Unlock()
	for k, t := range g.transports {
		if k.tls != "" && !keep[k.tls] {
			t.CloseIdleConnections()
			delete(g.transports, k)
		}
	}
}

func (g *Gateway) countUpstreamTLSError(apiID string) {
	v, _ := g.upstreamTLSErrors.LoadOrStore(apiID, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// upstreamTLSStatuses lists the upstream CAs and client certificates of the
// loaded snapshots, labelled with their APIs.
func (g *Gateway) upstreamTLSStatuses() []certs.Status {
	snap := g.snap.Load()
	if snap == nil {
		return nil
	}
	var out []certs.Status
	for _, s := range []*Snapshot{snap, snap.Canary} {
		if s == nil {
			continue
		}
		for _, r := range s.Routes {
			u := r.Pool.tls
			if u == nil {
				continue
			}
			for _, st := range u.statuses {
				st.APIID, st.Name = r.API.ID, r.API.Name
				out = append(out, st)
			}
		}
	}
	return out
}

// writeUpstreamTLSMetrics renders refused requests per API.
func (g *Gateway) writeUpstreamTLSMetrics(w io.Writer) {
	type kv struct {
		id string
		n  int64
	}
	var rows []kv
	g.upstreamTLSErrors.Range(func(k, v any) bool {
		rows = append(rows, kv{k.(string), v.(*atomic.Int64).Load()})
		return true
	})
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	fmt.Fprintf(w, "# HELP relayops_upstream_tls_errors_total Requests refused because an API's upstream TLS settings could not be resolved\n# TYPE relayops_upstream_tls_errors_total counter\n")
	for _, r := range rows {
		fmt.Fprintf(w, "relayops_upstream_tls_errors_total{api_id=%q} %d\n", r.id, r.n)
	}
	fmt.Fprintln(w)
}

// Certificates lists every certificate this node holds, soonest expiry first.
func (g *Gateway) Certificates() []certs.Status { return g.certs.List() }

// CertificateInventory is where listeners and clients register their
// certificates so they are reported and exported as metrics.
func (g *Gateway) CertificateInventory() *certs.Inventory { return g.certs }

// probeClient returns a health-check client that uses an API's upstream TLS.
func (g *Gateway) probeClient(base *http.Client, u *upstreamTLS) *http.Client {
	if u == nil || u.cfg == nil {
		return base
	}
	return &http.Client{
		Transport:     g.tlsTransport(int((30 * time.Second).Milliseconds()), u),
		CheckRedirect: base.CheckRedirect,
	}
}
