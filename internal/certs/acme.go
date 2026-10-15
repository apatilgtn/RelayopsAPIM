package certs

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/net/idna"
)

// ACMEConfig configures certificate issuance with ACME (RFC 8555).
type ACMEConfig struct {
	Domains      []string // the only hostnames certificates are requested for
	Email        string
	AcceptTOS    bool   // the operator accepted the CA's terms of service
	DirectoryURL string // empty: Let's Encrypt production
	Cache        autocert.Cache
	// OrderLock, when set, is taken before a node orders a new certificate,
	// so nodes sharing a cache do not order the same certificate at once
	// (renewals already coordinate through the shared cache). It reports
	// false while another node holds the order.
	OrderLock func(ctx context.Context, host string) (bool, error)
}

// ErrOrderInProgress means another node is obtaining the certificate; it
// will be served from the shared cache once issued.
var ErrOrderInProgress = errors.New("another node is obtaining this certificate; it will be served from the shared cache once issued")

// ACME issues and renews certificates for a fixed list of hostnames.
type ACME struct {
	m     *autocert.Manager
	hosts map[string]bool

	mu     sync.Mutex
	issued map[string]Status // hostname -> last certificate served
}

// NewACME validates cfg and returns an issuer.
func NewACME(cfg ACMEConfig) (*ACME, error) {
	if !cfg.AcceptTOS {
		return nil, errors.New("ACME needs RELAYOPS_ACME_ACCEPT_TOS=true: accepting the certificate authority's terms of service is the operator's decision")
	}
	if cfg.Cache == nil {
		return nil, errors.New("ACME needs a certificate cache")
	}
	hosts := map[string]bool{}
	var list []string
	for _, d := range cfg.Domains {
		h, err := normalizeHost(d)
		if err != nil {
			return nil, fmt.Errorf("RELAYOPS_ACME_DOMAINS: %q: %w", d, err)
		}
		if strings.HasPrefix(h, "*.") {
			return nil, fmt.Errorf("RELAYOPS_ACME_DOMAINS: %q: wildcard certificates need a DNS-01 challenge, which is not supported; list each hostname", d)
		}
		if !hosts[h] {
			hosts[h] = true
			list = append(list, h)
		}
	}
	if len(list) == 0 {
		return nil, errors.New("RELAYOPS_ACME_DOMAINS lists no hostnames")
	}
	allowed := autocert.HostWhitelist(list...)
	policy := allowed
	if cfg.OrderLock != nil {
		// Called only when the certificate is in neither memory nor cache,
		// that is, right before an order.
		policy = func(ctx context.Context, host string) error {
			if err := allowed(ctx, host); err != nil {
				return err
			}
			ok, err := cfg.OrderLock(ctx, host)
			if err != nil {
				return fmt.Errorf("ACME order lock: %w", err)
			}
			if !ok {
				return ErrOrderInProgress
			}
			return nil
		}
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      cfg.Cache,
		HostPolicy: policy,
		Email:      cfg.Email,
	}
	if cfg.DirectoryURL != "" {
		m.Client = &acme.Client{DirectoryURL: cfg.DirectoryURL}
	}
	return &ACME{m: m, hosts: hosts, issued: map[string]Status{}}, nil
}

func normalizeHost(h string) (string, error) {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if h == "" {
		return "", errors.New("empty hostname")
	}
	wild := strings.HasPrefix(h, "*.")
	ascii, err := idna.Lookup.ToASCII(strings.TrimPrefix(h, "*."))
	if err != nil || !strings.Contains(ascii, ".") {
		return "", errors.New("not a fully qualified hostname")
	}
	if wild {
		ascii = "*." + ascii
	}
	return ascii, nil
}

// Handles reports whether host is one of the configured hostnames.
func (a *ACME) Handles(host string) bool {
	if a == nil {
		return false
	}
	h, err := normalizeHost(host)
	return err == nil && a.hosts[h]
}

// IsChallenge reports whether hello is an ACME TLS-ALPN-01 validation.
func IsChallenge(hello *tls.ClientHelloInfo) bool {
	return hello != nil && slices.Contains(hello.SupportedProtos, acme.ALPNProto)
}

// ALPNProto is the protocol a TLS listener must offer for TLS-ALPN-01.
const ALPNProto = acme.ALPNProto

// GetCertificate returns the certificate for hello, obtaining or renewing it
// when needed, and answers TLS-ALPN-01 validations.
func (a *ACME) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c, err := a.m.GetCertificate(hello)
	if err != nil || IsChallenge(hello) {
		return c, err
	}
	if leaf := Leaf(c); leaf != nil {
		name, _ := normalizeHost(hello.ServerName)
		a.mu.Lock()
		if prev, ok := a.issued[name]; !ok || prev.Serial != leaf.SerialNumber.Text(16) {
			a.issued[name] = Summarize(leaf, SourceACME, name, RoleServer)
		}
		a.mu.Unlock()
	}
	return c, nil
}

// HTTPHandler answers HTTP-01 validations and redirects other requests to HTTPS.
func (a *ACME) HTTPHandler() http.Handler { return a.m.HTTPHandler(nil) }

// Statuses lists the certificates served so far.
func (a *ACME) Statuses() []Status {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Status, 0, len(a.issued))
	for _, s := range a.issued {
		out = append(out, s)
	}
	return out
}

// Warm requests every configured hostname's certificate in the background,
// so the first caller does not wait for issuance and expiry is reported
// before traffic arrives.
func (a *ACME) Warm(ctx context.Context, report func(host string, err error)) {
	for h := range a.hosts {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, err := a.GetCertificate(&tls.ClientHelloInfo{ServerName: h, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}})
		if report != nil {
			report(h, err)
		}
	}
}

// DirCache returns a directory cache, creating the directory with mode 0700.
func DirCache(dir string) (autocert.Cache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ACME cache directory: %w", err)
	}
	return autocert.DirCache(dir), nil
}

// KV is a byte store for the shared ACME cache, such as a database table.
type KV interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Put(ctx context.Context, key string, data []byte) error
	Delete(ctx context.Context, key string) error
}

// EncryptedCache stores ACME state (account key, certificates and their
// private keys) in kv, sealed with AES-256-GCM so the store never holds a
// usable private key.
type EncryptedCache struct {
	kv   KV
	aead cipher.AEAD
}

// NewEncryptedCache returns a cache sealed with a 32-byte key.
func NewEncryptedCache(kv KV, key []byte) (*EncryptedCache, error) {
	if len(key) != 32 {
		return nil, errors.New("RELAYOPS_ACME_CACHE_KEY must be 32 bytes (base64)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &EncryptedCache{kv: kv, aead: aead}, nil
}

// Get implements autocert.Cache.
func (c *EncryptedCache) Get(ctx context.Context, key string) ([]byte, error) {
	sealed, ok, err := c.kv.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, autocert.ErrCacheMiss
	}
	n := c.aead.NonceSize()
	if len(sealed) < n {
		return nil, fmt.Errorf("ACME cache entry %q is corrupt", key)
	}
	// The key name is authenticated, so an entry cannot be moved to another key.
	plain, err := c.aead.Open(nil, sealed[:n], sealed[n:], []byte(key))
	if err != nil {
		return nil, fmt.Errorf("ACME cache entry %q cannot be decrypted (wrong RELAYOPS_ACME_CACHE_KEY?)", key)
	}
	return plain, nil
}

// Put implements autocert.Cache.
func (c *EncryptedCache) Put(ctx context.Context, key string, data []byte) error {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	return c.kv.Put(ctx, key, c.aead.Seal(nonce, nonce, data, []byte(key)))
}

// Delete implements autocert.Cache.
func (c *EncryptedCache) Delete(ctx context.Context, key string) error {
	return c.kv.Delete(ctx, key)
}
