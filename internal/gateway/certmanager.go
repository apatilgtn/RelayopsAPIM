package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/certs"
	"github.com/relayops/apim/internal/config"
)

var (
	ErrNoMatchingCertificate = errors.New("no matching TLS certificate found for ServerName")
	ErrInvalidCertificate    = errors.New("invalid TLS certificate or private key")
	ErrInvalidClientCA       = errors.New("failed to parse client CA certificates PEM")
)

// TLSFiles names the files a listener's certificates are read from. Every
// field is optional.
type TLSFiles struct {
	CertFile string // default certificate
	KeyFile  string
	// Dir holds <host>.crt with <host>.key per hostname, and optionally
	// <host>.client-ca.pem, the CA that client certificates for that
	// hostname must chain to (replacing ClientCAFile there). A distinct
	// extension keeps it apart from server certificates, whose hostnames may
	// end in any label (example.ca). "_wildcard.example.com" names
	// "*.example.com".
	Dir          string
	ClientCAFile string // CA that client certificates must chain to
	CRLFile      string // revocation lists for client certificates (PEM or DER)
}

// Empty reports whether no file is configured.
func (f TLSFiles) Empty() bool {
	return f.CertFile == "" && f.KeyFile == "" && f.Dir == "" && f.ClientCAFile == "" && f.CRLFile == ""
}

// maxTLSFileBytes bounds one certificate, key, CA or CRL file.
const maxTLSFileBytes = 4 << 20

// clientCASuffix names a per-hostname client CA file in TLSFiles.Dir.
const clientCASuffix = ".client-ca.pem"

// certSet is an immutable set of listener certificates. Handshakes read the
// current set without locks; changes build a new set and swap it in.
type certSet struct {
	def       *tls.Certificate
	byHost    map[string]*tls.Certificate
	clientCAs *x509.CertPool
	hostCAs   map[string]*x509.CertPool
	crl       *certs.Revocations
	caLists   map[string][]*x509.Certificate // client CAs by hostname ("default": global), for reports
	statuses  []certs.Status
	digest    [32]byte // of the files the set was read from

	// built by finalize
	base      *tls.Config
	hostCfgs  map[string]*tls.Config
	challenge *tls.Config // ACME TLS-ALPN-01 validations: no client certificate
}

func (s *certSet) clone() *certSet {
	n := *s
	n.byHost = make(map[string]*tls.Certificate, len(s.byHost))
	for k, v := range s.byHost {
		n.byHost[k] = v
	}
	n.hostCAs = make(map[string]*x509.CertPool, len(s.hostCAs))
	for k, v := range s.hostCAs {
		n.hostCAs[k] = v
	}
	n.caLists = make(map[string][]*x509.Certificate, len(s.caLists))
	for k, v := range s.caLists {
		n.caLists[k] = v
	}
	n.statuses = nil
	return &n
}

// DynamicCertManager selects listener certificates by SNI, verifies client
// certificates against a CA chosen by hostname, refuses revoked client
// certificates, issues certificates with ACME, and reloads its files when
// they change.
type DynamicCertManager struct {
	name string // listener: gateway, admin or relay

	mu            sync.Mutex // serialises changes
	set           atomic.Pointer[certSet]
	requireClient bool
	clientAuthSet bool // SetClientAuth was called: it overrides requireClient
	clientAuth    tls.ClientAuthType
	acme          atomic.Pointer[certs.ACME]
	files         TLSFiles
	inv           *certs.Inventory
	lastErr       string

	revokedRefusals atomic.Int64
}

// NewDynamicCertManager returns a manager serving defaultCert (may be nil)
// when no hostname matches.
func NewDynamicCertManager(defaultCert *tls.Certificate) *DynamicCertManager {
	m := &DynamicCertManager{name: "gateway"}
	s := &certSet{def: defaultCert, byHost: map[string]*tls.Certificate{}, hostCAs: map[string]*x509.CertPool{}, caLists: map[string][]*x509.Certificate{}}
	m.finalize(s)
	m.set.Store(s)
	return m
}

// SetName names the listener in metrics and certificate reports.
func (m *DynamicCertManager) SetName(name string) {
	m.mu.Lock()
	m.name = name
	s := m.set.Load().clone()
	m.finalize(s)
	m.set.Store(s)
	m.mu.Unlock()
}

// SetInventory reports this listener's certificates and reloads to inv.
func (m *DynamicCertManager) SetInventory(inv *certs.Inventory) {
	m.mu.Lock()
	m.inv = inv
	m.mu.Unlock()
	inv.Register(m.Statuses)
	inv.RegisterMetrics(m.writeMetrics)
}

// SetACME issues certificates for a's hostnames when no file certificate
// matches, and answers its TLS-ALPN-01 validations.
func (m *DynamicCertManager) SetACME(a *certs.ACME) {
	m.update(func(*certSet) { m.acme.Store(a) })
}

// RequireClientCertificates makes a client certificate mandatory wherever a
// client CA applies. Otherwise one is verified when presented.
func (m *DynamicCertManager) RequireClientCertificates(require bool) {
	m.update(func(*certSet) { m.requireClient = require })
}

// update applies fn to a copy of the current set and swaps it in.
func (m *DynamicCertManager) update(fn func(*certSet)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.set.Load().clone()
	fn(s)
	m.finalize(s)
	m.set.Store(s)
}

// SetDefaultCertificate sets the fallback certificate when no specific SNI matches.
func (m *DynamicCertManager) SetDefaultCertificate(cert *tls.Certificate) {
	m.update(func(s *certSet) { s.def = cert })
}

// RegisterCertificate serves cert for domain ("*.example.com" for a wildcard).
func (m *DynamicCertManager) RegisterCertificate(domain string, cert *tls.Certificate) {
	if cert == nil {
		return
	}
	m.update(func(s *certSet) { s.byHost[normalizeSNI(domain)] = cert })
}

// RegisterPEM parses and serves a PEM certificate and private key for domain.
func (m *DynamicCertManager) RegisterPEM(domain string, certPEM, keyPEM []byte) error {
	cert, err := parseKeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	m.RegisterCertificate(domain, cert)
	return nil
}

// UnregisterDomain removes a domain and its certificate.
func (m *DynamicCertManager) UnregisterDomain(domain string) {
	m.update(func(s *certSet) { delete(s.byHost, normalizeSNI(domain)) })
}

// SetClientCAs sets the CA that client certificates must chain to.
func (m *DynamicCertManager) SetClientCAs(caPEM []byte) error {
	list, err := certs.ParseCertificates(caPEM)
	if err != nil {
		return ErrInvalidClientCA
	}
	m.update(func(s *certSet) { s.clientCAs, s.caLists["default"] = certs.Pool(list), list })
	return nil
}

// SetHostClientCAs sets the client CA for one hostname, replacing the
// global one there.
func (m *DynamicCertManager) SetHostClientCAs(host string, caPEM []byte) error {
	list, err := certs.ParseCertificates(caPEM)
	if err != nil {
		return ErrInvalidClientCA
	}
	h := normalizeSNI(host)
	m.update(func(s *certSet) { s.hostCAs[h], s.caLists[h] = certs.Pool(list), list })
	return nil
}

// SetClientAuth fixes the client authentication mode for every hostname,
// overriding the mode derived from the configured CAs.
func (m *DynamicCertManager) SetClientAuth(authType tls.ClientAuthType) {
	m.update(func(*certSet) { m.clientAuth, m.clientAuthSet = authType, true })
}

func normalizeSNI(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

func wildcardOf(host string) string {
	if i := strings.Index(host, "."); i > 0 {
		return "*" + host[i:]
	}
	return ""
}

func parseKeyPair(certPEM, keyPEM []byte) (*tls.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCertificate, err)
	}
	if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
		cert.Leaf = leaf
	}
	return &cert, nil
}

// GetCertificate implements tls.Config.GetCertificate: an ACME validation,
// then a file certificate for the exact hostname, then a wildcard, then an
// ACME certificate for a configured hostname, then the default.
func (m *DynamicCertManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	s := m.set.Load()
	a := m.acme.Load()
	if a != nil && certs.IsChallenge(hello) {
		return a.GetCertificate(hello)
	}
	name := ""
	if hello != nil {
		name = normalizeSNI(hello.ServerName)
	}
	if name != "" {
		if c, ok := s.byHost[name]; ok {
			return c, nil
		}
		if w := wildcardOf(name); w != "" {
			if c, ok := s.byHost[w]; ok {
				return c, nil
			}
		}
		if a != nil && a.Handles(name) {
			c, err := a.GetCertificate(hello)
			if err == nil {
				return c, nil
			}
			if s.def == nil {
				return nil, err
			}
			slog.Warn("ACME certificate unavailable; serving the default certificate", "listener", m.name, "host", name, "err", err)
		}
	}
	if s.def != nil {
		return s.def, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNoMatchingCertificate, name)
}

// clientCAFor returns the client CA pool for an SNI hostname.
func (s *certSet) clientCAFor(host string) *x509.CertPool {
	if p, ok := s.hostCAs[host]; ok {
		return p
	}
	if w := wildcardOf(host); w != "" {
		if p, ok := s.hostCAs[w]; ok {
			return p
		}
	}
	return s.clientCAs
}

// finalize builds the handshake configurations and status list of s.
// Callers hold m.mu (or own s exclusively).
func (m *DynamicCertManager) finalize(s *certSet) {
	anyCA := s.clientCAs != nil || len(s.hostCAs) > 0
	mode := tls.NoClientCert
	switch {
	case m.clientAuthSet:
		mode = m.clientAuth
	case anyCA && m.requireClient:
		mode = tls.RequireAndVerifyClientCert
	case anyCA:
		mode = tls.VerifyClientCertIfGiven
	}
	protos := []string{"h2", "http/1.1"}
	if m.acme.Load() != nil {
		protos = append(protos, certs.ALPNProto)
	}
	build := func(pool *x509.CertPool) *tls.Config {
		c := &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: m.GetCertificate,
			NextProtos:     protos,
			ClientAuth:     mode,
			ClientCAs:      pool,
		}
		if mode != tls.NoClientCert && pool == nil {
			if mode == tls.RequireAndVerifyClientCert || mode == tls.RequireAnyClientCert {
				c.ClientCAs = x509.NewCertPool() // nothing can verify: refuse rather than accept unverified
			} else {
				c.ClientAuth = tls.NoClientCert
			}
		}
		if s.crl.Len() > 0 && c.ClientAuth != tls.NoClientCert {
			crl := s.crl
			c.VerifyConnection = func(cs tls.ConnectionState) error {
				if err := crl.CheckChains(cs.VerifiedChains); err != nil {
					m.revokedRefusals.Add(1)
					return err
				}
				return nil
			}
		}
		return c
	}
	s.base = build(s.clientCAs)
	// The CA's validator presents no client certificate, so a validation
	// must not be subject to client authentication, or certificates could
	// never be issued where client certificates are required.
	s.challenge = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: m.GetCertificate, NextProtos: []string{certs.ALPNProto}}
	s.hostCfgs = make(map[string]*tls.Config, len(s.hostCAs))
	for h, p := range s.hostCAs {
		s.hostCfgs[h] = build(p)
	}

	var st []certs.Status
	add := func(c *tls.Certificate, source, name string) {
		if leaf := certs.Leaf(c); leaf != nil {
			x := certs.Summarize(leaf, source, name, certs.RoleServer)
			x.Listener = m.name
			st = append(st, x)
		}
	}
	add(s.def, certs.SourceListener, "default")
	hosts := make([]string, 0, len(s.byHost))
	for h := range s.byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		add(s.byHost[h], certs.SourceListenerHost, h)
	}
	names := make([]string, 0, len(s.caLists))
	for n := range s.caLists {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, c := range s.caLists[n] {
			x := certs.Summarize(c, certs.SourceClientCA, n, certs.RoleCA)
			x.Listener = m.name
			st = append(st, x)
		}
	}
	s.statuses = st
}

// TLSConfig returns the listener configuration. Each handshake picks the
// client CA for its hostname from the current certificate set, so reloads
// apply to new connections without a restart.
func (m *DynamicCertManager) TLSConfig() *tls.Config {
	s := m.set.Load()
	cfg := s.base.Clone()
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		cur := m.set.Load()
		if certs.IsChallenge(hello) && m.acme.Load() != nil {
			return cur.challenge, nil
		}
		if hello != nil && len(cur.hostCfgs) > 0 {
			name := normalizeSNI(hello.ServerName)
			if c, ok := cur.hostCfgs[name]; ok {
				return c, nil
			}
			if w := wildcardOf(name); w != "" {
				if c, ok := cur.hostCfgs[w]; ok {
					return c, nil
				}
			}
		}
		return cur.base, nil
	}
	return cfg
}

// Statuses lists the certificates this listener serves and trusts.
func (m *DynamicCertManager) Statuses() []certs.Status {
	out := append([]certs.Status(nil), m.set.Load().statuses...)
	if a := m.acme.Load(); a != nil {
		for _, s := range a.Statuses() {
			s.Listener = m.name
			out = append(out, s)
		}
	}
	return out
}

func (m *DynamicCertManager) writeMetrics(w io.Writer) {
	s := m.set.Load()
	if s.crl.Len() == 0 {
		return
	}
	fmt.Fprintf(w, "# HELP relayops_tls_crl_stale Client-certificate revocation lists past their next update (still enforced)\n# TYPE relayops_tls_crl_stale gauge\n")
	fmt.Fprintf(w, "relayops_tls_crl_stale{listener=%q} %d\n\n", m.name, s.crl.Stale(time.Now()))
	fmt.Fprintf(w, "# HELP relayops_tls_revoked_refusals_total TLS handshakes refused because a client certificate was revoked\n# TYPE relayops_tls_revoked_refusals_total counter\n")
	fmt.Fprintf(w, "relayops_tls_revoked_refusals_total{listener=%q} %d\n\n", m.name, m.revokedRefusals.Load())
}

// ---------------------------------------------------------------------------
// Files and reloading
// ---------------------------------------------------------------------------

// readFiles reads every configured file and returns their combined digest.
func readFiles(f TLSFiles) (map[string][]byte, [32]byte, error) {
	var paths []string
	for _, p := range []string{f.CertFile, f.KeyFile, f.ClientCAFile, f.CRLFile} {
		if p != "" {
			paths = append(paths, p)
		}
	}
	if f.Dir != "" {
		entries, err := os.ReadDir(f.Dir)
		if err != nil {
			return nil, [32]byte{}, fmt.Errorf("certificate directory: %w", err)
		}
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, ".") || (!strings.HasSuffix(n, ".crt") && !strings.HasSuffix(n, ".key") && !strings.HasSuffix(n, clientCASuffix)) {
				continue // e.g. Kubernetes' ..data symlink and dot-files
			}
			paths = append(paths, filepath.Join(f.Dir, n))
		}
	}
	sort.Strings(paths)
	h := sha256.New()
	out := map[string][]byte{}
	for _, p := range paths {
		data, err := config.ReadFileLimited(p, maxTLSFileBytes)
		if err != nil {
			return nil, [32]byte{}, err
		}
		out[p] = data
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(data))
		h.Write(data)
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return out, d, nil
}

// hostFromFile turns "_wildcard.example.com" into "*.example.com".
func hostFromFile(base string) string {
	return normalizeSNI(strings.Replace(base, "_wildcard", "*", 1))
}

// buildFromFiles parses files into a complete certificate set. It fails as a
// whole: a reload never serves half of a change.
func buildFromFiles(f TLSFiles, data map[string][]byte, digest [32]byte) (*certSet, error) {
	s := &certSet{byHost: map[string]*tls.Certificate{}, hostCAs: map[string]*x509.CertPool{},
		caLists: map[string][]*x509.Certificate{}, digest: digest}
	if f.CertFile != "" || f.KeyFile != "" {
		if f.CertFile == "" || f.KeyFile == "" {
			return nil, errors.New("a certificate file needs its key file, and a key file its certificate")
		}
		c, err := parseKeyPair(data[f.CertFile], data[f.KeyFile])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.CertFile, err)
		}
		s.def = c
	}
	if f.Dir != "" {
		var names []string
		for p := range data {
			if filepath.Dir(p) != filepath.Clean(f.Dir) {
				continue
			}
			if base, ok := strings.CutSuffix(filepath.Base(p), clientCASuffix); ok {
				host := hostFromFile(base)
				list, err := certs.ParseCertificates(data[p])
				if err != nil {
					return nil, fmt.Errorf("%s: %w", p, ErrInvalidClientCA)
				}
				s.hostCAs[host], s.caLists[host] = certs.Pool(list), list
			} else if strings.HasSuffix(p, ".crt") {
				names = append(names, p)
			}
		}
		sort.Strings(names)
		for _, p := range names {
			base := strings.TrimSuffix(filepath.Base(p), ".crt")
			keyPath := filepath.Join(filepath.Dir(p), base+".key")
			key, ok := data[keyPath]
			if !ok {
				return nil, fmt.Errorf("certificate %s has no matching key %s", p, keyPath)
			}
			c, err := parseKeyPair(data[p], key)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			s.byHost[hostFromFile(base)] = c
		}
	}
	if f.ClientCAFile != "" {
		list, err := certs.ParseCertificates(data[f.ClientCAFile])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.ClientCAFile, ErrInvalidClientCA)
		}
		s.clientCAs, s.caLists["default"] = certs.Pool(list), list
	}
	if f.CRLFile != "" {
		lists, err := certs.ParseCRLs(data[f.CRLFile])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.CRLFile, err)
		}
		s.crl = certs.NewRevocations(lists)
	}
	return s, nil
}

// LoadFiles reads f and serves its certificates. It replaces every file
// certificate, client CA and CRL; ACME and the client-auth mode are kept.
func (m *DynamicCertManager) LoadFiles(f TLSFiles) error {
	m.mu.Lock()
	m.files = f
	m.mu.Unlock()
	_, err := m.Reload()
	return err
}

// Reload re-reads the configured files and swaps in the new certificates
// when anything changed. On an error the current certificates stay.
func (m *DynamicCertManager) Reload() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.files
	if f.Empty() {
		return false, nil
	}
	data, digest, err := readFiles(f)
	if err == nil && digest == m.set.Load().digest {
		return false, nil
	}
	var s *certSet
	if err == nil {
		s, err = buildFromFiles(f, data, digest)
	}
	if err != nil {
		m.inv.CountReload(m.name, "error")
		return false, err
	}
	m.finalize(s)
	m.set.Store(s)
	m.lastErr = ""
	m.inv.CountReload(m.name, "success")
	return true, nil
}

// Watch reloads the files every interval (0 disables polling) and whenever
// a value arrives on reload, until ctx ends.
func (m *DynamicCertManager) Watch(ctx context.Context, interval time.Duration, reload <-chan struct{}) {
	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		case <-reload:
		}
		changed, err := m.Reload()
		switch {
		case err != nil:
			// Log a failure once, not at every poll, until it changes.
			m.mu.Lock()
			repeat := m.lastErr == err.Error()
			m.lastErr = err.Error()
			m.mu.Unlock()
			if !repeat {
				slog.Error("certificate reload failed; keeping the current certificates", "listener", m.name, "err", err)
			}
		case changed:
			slog.Info("certificates reloaded", "listener", m.name, "certificates", len(m.set.Load().statuses))
		}
	}
}
