package gateway

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
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
