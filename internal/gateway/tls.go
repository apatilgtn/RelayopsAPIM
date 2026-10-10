package gateway

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

var (
	ErrNoMatchingCertificate = errors.New("no matching TLS certificate found for ServerName")
	ErrInvalidCertificate    = errors.New("invalid TLS certificate or private key")
	ErrInvalidClientCA       = errors.New("failed to parse client CA certificates PEM")
)

// ClientCertInfo holds verified mTLS client identity attributes.
type ClientCertInfo struct {
	Subject           string   `json:"subject"`
	CommonName        string   `json:"common_name"`
	Organization      []string `json:"organization,omitempty"`
	DNSNames          []string `json:"dns_names,omitempty"`
	EmailAddresses    []string `json:"email_addresses,omitempty"`
	SerialNumber      string   `json:"serial_number"`
	FingerprintSHA256 string   `json:"fingerprint_sha256"`
	Verified          bool     `json:"verified"`
}

// DynamicCertProvider enables on-demand fetching of certificates (e.g. from Supabase Vault, AWS Secrets Manager, or ACME).
type DynamicCertProvider interface {
	FetchCertificate(domain, tenantSlug string) (*tls.Certificate, error)
}

// DynamicCertManager manages dynamic TLS certificates and SNI routing for tenant vanity domains.
type DynamicCertManager struct {
	mu            sync.RWMutex
	certs         map[string]*tls.Certificate // domain -> *tls.Certificate
	tenantDomains map[string]string           // domain -> tenantSlug
	defaultCert   *tls.Certificate
	provider      DynamicCertProvider
	clientCAs     *x509.CertPool
	clientAuth    tls.ClientAuthType
	tenantCAs     map[string]*x509.CertPool
}

// NewDynamicCertManager creates a new dynamic TLS certificate manager.
func NewDynamicCertManager(defaultCert *tls.Certificate, provider DynamicCertProvider) *DynamicCertManager {
	return &DynamicCertManager{
		certs:         make(map[string]*tls.Certificate),
		tenantDomains: make(map[string]string),
		defaultCert:   defaultCert,
		provider:      provider,
	}
}

// SetDefaultCertificate sets the fallback certificate when no specific SNI matches.
func (m *DynamicCertManager) SetDefaultCertificate(cert *tls.Certificate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultCert = cert
}

// RegisterCertificate associates a pre-parsed certificate with a domain and optional tenant.
func (m *DynamicCertManager) RegisterCertificate(domain, tenantSlug string, cert *tls.Certificate) {
	if cert == nil {
		return
	}
	normalized := strings.ToLower(strings.TrimSpace(domain))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.certs[normalized] = cert
	if tenantSlug != "" {
		m.tenantDomains[normalized] = tenantSlug
	}
}

// RegisterPEM parses and stores a PEM certificate and private key for a tenant domain.
func (m *DynamicCertManager) RegisterPEM(domain, tenantSlug string, certPEM, keyPEM []byte) error {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCertificate, err)
	}
	if len(cert.Certificate) > 0 {
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err == nil {
			cert.Leaf = parsed
		}
	}
	m.RegisterCertificate(domain, tenantSlug, &cert)
	return nil
}

// UnregisterDomain removes a domain and its certificate.
func (m *DynamicCertManager) UnregisterDomain(domain string) {
	normalized := strings.ToLower(strings.TrimSpace(domain))
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.certs, normalized)
	delete(m.tenantDomains, normalized)
}

// GetTenantForDomain returns the tenant slug mapped to a specific hostname.
func (m *DynamicCertManager) GetTenantForDomain(domain string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(domain))
	m.mu.RLock()
	defer m.mu.RUnlock()
	tenant, ok := m.tenantDomains[normalized]
	if !ok {
		// Check wildcard match (*.example.com)
		if idx := strings.Index(normalized, "."); idx != -1 {
			wildcard := "*" + normalized[idx:]
			tenant, ok = m.tenantDomains[wildcard]
		}
	}
	return tenant, ok
}

// GetCertificate implements tls.Config.GetCertificate for dynamic SNI resolution.
func (m *DynamicCertManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello == nil || hello.ServerName == "" {
		m.mu.RLock()
		defer m.mu.RUnlock()
		if m.defaultCert != nil {
			return m.defaultCert, nil
		}
		return nil, ErrNoMatchingCertificate
	}

	serverName := strings.ToLower(strings.TrimSpace(hello.ServerName))

	// 1. Check exact domain match in in-memory cache
	m.mu.RLock()
	cert, exists := m.certs[serverName]
	m.mu.RUnlock()
	if exists {
		return cert, nil
	}

	// 2. Check wildcard domain (*.tenant.com)
	if idx := strings.Index(serverName, "."); idx != -1 {
		wildcard := "*" + serverName[idx:]
		m.mu.RLock()
		wCert, wExists := m.certs[wildcard]
		m.mu.RUnlock()
		if wExists {
			return wCert, nil
		}
	}

	// 3. Attempt dynamic on-demand resolution via provider if configured
	if m.provider != nil {
		m.mu.RLock()
		tenant := m.tenantDomains[serverName]
		m.mu.RUnlock()

		fetched, err := m.provider.FetchCertificate(serverName, tenant)
		if err == nil && fetched != nil {
			m.RegisterCertificate(serverName, tenant, fetched)
			return fetched, nil
		}
		slog.Debug("dynamic cert provider lookup missed", "server_name", serverName, "err", err)
	}

	// 4. Fallback to default certificate
	m.mu.RLock()
	fallback := m.defaultCert
	m.mu.RUnlock()
	if fallback != nil {
		return fallback, nil
	}

	return nil, fmt.Errorf("%w: %s", ErrNoMatchingCertificate, serverName)
}

// SetClientCAs configures the trusted Certificate Authority pool for client certificate verification (mTLS).
func (m *DynamicCertManager) SetClientCAs(caPEM []byte) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return ErrInvalidClientCA
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clientCAs = pool
	return nil
}

// SetClientAuth configures the TLS client authentication mode (e.g. tls.RequireAndVerifyClientCert).
func (m *DynamicCertManager) SetClientAuth(authType tls.ClientAuthType) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clientAuth = authType
}

// AddClientCA registers or appends a client CA for a specific tenant.
func (m *DynamicCertManager) AddClientCA(tenantSlug string, caPEM []byte) error {
	normalized := strings.ToLower(strings.TrimSpace(tenantSlug))
	m.mu.Lock()
	defer m.mu.Unlock()
	pool, ok := m.tenantCAs[normalized]
	if !ok {
		pool = x509.NewCertPool()
		m.tenantCAs[normalized] = pool
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		return ErrInvalidClientCA
	}
	return nil
}

// clientCertHeaders carry client-certificate identity. Callers must never be
// able to set them: mtls authentication trusts them.
var clientCertHeaders = []string{
	"X-Client-Cert-Fingerprint", "X-Client-Cert-Subject", "X-Client-Cert-CN", "X-Client-Cert-Verified",
	"X-SSL-Client-SHA256", "X-SSL-Client-CN", "X-SSL-Client-S-DN", "X-SSL-Client-Cert", "X-SSL-Client-Verify",
}

var canonicalClientCertHeaders = func() []string {
	out := make([]string, len(clientCertHeaders))
	for i, h := range clientCertHeaders {
		out[i] = http.CanonicalHeaderKey(h)
	}
	return out
}()

// SetTrustedProxies lists the networks of reverse proxies (TLS-terminating
// ingresses) whose forwarded client-certificate headers the gateway accepts.
func (g *Gateway) SetTrustedProxies(nets []*net.IPNet) { g.trustedProxies = nets }

// ParseCIDRs parses a comma-separated list of CIDRs or single IPs.
func ParseCIDRs(csv string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
				s += "/32"
			} else {
				s += "/128"
			}
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: %w", s, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func (g *Gateway) fromTrustedProxy(r *http.Request) bool {
	if len(g.trustedProxies) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	for _, n := range g.trustedProxies {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// bindClientCertHeaders replaces any client-certificate headers the caller
// sent with ones derived from a verified source: a TLS client certificate
// this gateway verified, or the headers of a trusted reverse proxy that
// reports successful verification. Otherwise the headers are removed, so a
// caller cannot claim a certificate it did not present.
func (g *Gateway) bindClientCertHeaders(r *http.Request) {
	var cert *ClientCertInfo
	switch {
	case r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.PeerCertificates) > 0:
		cert = ExtractClientCertificate(r) // the TLS path is checked first
	case g.fromTrustedProxy(r):
		if c := ExtractClientCertificate(r); c != nil && c.Verified {
			cert = c
		}
	}
	for _, h := range canonicalClientCertHeaders {
		delete(r.Header, h) // keys are pre-canonicalised: Del would redo it per request
	}
	if cert == nil {
		return
	}
	r.Header.Set("X-Client-Cert-Fingerprint", cert.FingerprintSHA256)
	if cert.Subject != "" {
		r.Header.Set("X-Client-Cert-Subject", cert.Subject)
	}
	if cert.CommonName != "" {
		r.Header.Set("X-Client-Cert-CN", cert.CommonName)
	}
}

// ExtractClientCertificate extracts verified client certificate details from the TLS connection or forwarded ingress headers.
func ExtractClientCertificate(r *http.Request) *ClientCertInfo {
	if r == nil {
		return nil
	}

	// 1. Direct TLS connection
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		peer := r.TLS.PeerCertificates[0]
		fp := sha256.Sum256(peer.Raw)
		return &ClientCertInfo{
			Subject:           peer.Subject.String(),
			CommonName:        peer.Subject.CommonName,
			Organization:      peer.Subject.Organization,
			DNSNames:          peer.DNSNames,
			EmailAddresses:    peer.EmailAddresses,
			SerialNumber:      peer.SerialNumber.String(),
			FingerprintSHA256: strings.ToUpper(hex.EncodeToString(fp[:])),
			Verified:          true,
		}
	}

	// 2. Reverse proxy / ingress headers: X-SSL-Client-Cert (PEM)
	if rawPEM := r.Header.Get("X-SSL-Client-Cert"); rawPEM != "" {
		decoded, err := url.QueryUnescape(rawPEM)
		if err != nil {
			decoded = rawPEM
		}
		block, _ := pem.Decode([]byte(decoded))
		if block != nil {
			parsed, err := x509.ParseCertificate(block.Bytes)
			if err == nil {
				fp := sha256.Sum256(parsed.Raw)
				return &ClientCertInfo{
					Subject:           parsed.Subject.String(),
					CommonName:        parsed.Subject.CommonName,
					Organization:      parsed.Subject.Organization,
					DNSNames:          parsed.DNSNames,
					EmailAddresses:    parsed.EmailAddresses,
					SerialNumber:      parsed.SerialNumber.String(),
					FingerprintSHA256: strings.ToUpper(hex.EncodeToString(fp[:])),
					Verified:          r.Header.Get("X-SSL-Client-Verify") == "SUCCESS" || r.Header.Get("X-Client-Cert-Verified") == "true",
				}
			}
		}
	}

	// 3. Reverse proxy / ingress header: X-SSL-Client-SHA256 or X-Client-Cert-Fingerprint
	fp := strings.TrimSpace(r.Header.Get("X-SSL-Client-SHA256"))
	if fp == "" {
		fp = strings.TrimSpace(r.Header.Get("X-Client-Cert-Fingerprint"))
	}
	if fp != "" {
		fp = strings.ToUpper(strings.ReplaceAll(fp, ":", ""))
		subj := r.Header.Get("X-SSL-Client-S-DN")
		cn := r.Header.Get("X-SSL-Client-CN")
		if cn == "" {
			cn = r.Header.Get("X-Client-Cert-CN")
		}
		return &ClientCertInfo{
			Subject:           subj,
			CommonName:        cn,
			FingerprintSHA256: fp,
			Verified:          true,
		}
	}

	return nil
}

// TLSConfig returns a production-ready *tls.Config equipped with dynamic SNI resolution and mTLS client authentication.
func (m *DynamicCertManager) TLSConfig() *tls.Config {
	m.mu.RLock()
	clientCAs := m.clientCAs
	clientAuth := m.clientAuth
	m.mu.RUnlock()

	return &tls.Config{
		GetCertificate: m.GetCertificate,
		MinVersion:     tls.VersionTLS12,
		ClientAuth:     clientAuth,
		ClientCAs:      clientCAs,
	}
}
