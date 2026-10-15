package certs_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relayops/apim/internal/certs"
	"github.com/relayops/apim/internal/certs/certtest"
	"golang.org/x/crypto/acme/autocert"
)

func TestRevocationsOnlyTrustListsSignedByTheIssuer(t *testing.T) {
	ca := certtest.NewCA(t, "Clients CA")
	other := certtest.NewCA(t, "Other CA")
	good := ca.Issue(t, "good", certtest.Options{Client: true})
	bad := ca.Issue(t, "bad", certtest.Options{Client: true})

	lists, err := certs.ParseCRLs(ca.CRL(t, time.Now().Add(time.Hour), bad.Cert.SerialNumber))
	if err != nil {
		t.Fatal(err)
	}
	r := certs.NewRevocations(lists)
	if err := r.CheckChains([][]*x509.Certificate{{good.Cert, ca.Cert}}); err != nil {
		t.Fatalf("good certificate refused: %v", err)
	}
	if err := r.CheckChains([][]*x509.Certificate{{bad.Cert, ca.Cert}}); !errors.Is(err, certs.ErrRevoked) {
		t.Fatalf("revoked certificate accepted: %v", err)
	}

	// A list signed by another CA that merely claims the same issuer name
	// must not revoke anything.
	forged := certtest.NewCA(t, "Clients CA") // same subject, different key
	flists, _ := certs.ParseCRLs(forged.CRL(t, time.Now().Add(time.Hour), good.Cert.SerialNumber))
	if err := certs.NewRevocations(flists).CheckChains([][]*x509.Certificate{{good.Cert, ca.Cert}}); err != nil {
		t.Fatalf("a list not signed by the issuer revoked a certificate: %v", err)
	}
	// Another CA's list never applies to this CA's certificates.
	olists, _ := certs.ParseCRLs(other.CRL(t, time.Now().Add(time.Hour), bad.Cert.SerialNumber))
	if err := certs.NewRevocations(olists).CheckChains([][]*x509.Certificate{{bad.Cert, ca.Cert}}); err != nil {
		t.Fatalf("another CA's list applied: %v", err)
	}
}

func TestStaleCRLIsStillEnforced(t *testing.T) {
	ca := certtest.NewCA(t, "CA")
	bad := ca.Issue(t, "bad", certtest.Options{Client: true})
	lists, err := certs.ParseCRLs(ca.CRL(t, time.Now().Add(-time.Minute), bad.Cert.SerialNumber))
	if err != nil {
		t.Fatal(err)
	}
	r := certs.NewRevocations(lists)
	if r.Stale(time.Now()) != 1 {
		t.Fatal("expired list not reported stale")
	}
	if err := r.CheckChains([][]*x509.Certificate{{bad.Cert, ca.Cert}}); err == nil {
		t.Fatal("a stale list must still refuse revoked certificates")
	}
}

func TestParseCRLsAcceptsDER(t *testing.T) {
	ca := certtest.NewCA(t, "CA")
	pemCRL := ca.CRL(t, time.Now().Add(time.Hour))
	lists, err := certs.ParseCRLs(pemCRL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := certs.ParseCRLs(lists[0].Raw); err != nil {
		t.Fatalf("DER list: %v", err)
	}
	if _, err := certs.ParseCRLs([]byte("not a list")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestSeverity(t *testing.T) {
	now := time.Now()
	s := func(notAfter time.Time) certs.Status {
		return certs.Status{NotBefore: now.Add(-time.Hour), NotAfter: notAfter}
	}
	for _, tc := range []struct {
		st   certs.Status
		want string
	}{
		{s(now.Add(-time.Second)), "critical"},
		{s(now.Add(3 * 24 * time.Hour)), "warning"},
		{s(now.Add(20 * 24 * time.Hour)), "info"},
		{s(now.Add(90 * 24 * time.Hour)), ""},
		{certs.Status{NotBefore: now.Add(time.Hour), NotAfter: now.Add(90 * 24 * time.Hour)}, "critical"},
	} {
		if got := certs.Severity(tc.st, now); got != tc.want {
			t.Errorf("not_after %v: got %q, want %q", tc.st.NotAfter, got, tc.want)
		}
	}
}

func TestInventoryMetricsAndOrder(t *testing.T) {
	ca := certtest.NewCA(t, "CA")
	soon := ca.Issue(t, "soon", certtest.Options{Validity: time.Hour})
	later := ca.Issue(t, "later", certtest.Options{Validity: 48 * time.Hour})
	inv := certs.NewInventory()
	inv.Register(func() []certs.Status {
		return []certs.Status{
			certs.Summarize(later.Cert, certs.SourceListener, "default", certs.RoleServer),
			certs.Summarize(soon.Cert, certs.SourceListenerHost, `we"ird`, certs.RoleServer),
		}
	})
	inv.Register(func() []certs.Status { // duplicates are listed once
		return []certs.Status{certs.Summarize(later.Cert, certs.SourceListener, "default", certs.RoleServer)}
	})
	inv.CountReload("gateway", "success")
	list := inv.List()
	if len(list) != 2 || list[0].Subject != "CN=soon" {
		t.Fatalf("list = %+v", list)
	}
	var buf bytes.Buffer
	inv.WriteMetrics(&buf)
	out := buf.String()
	for _, want := range []string{
		`relayops_tls_certificate_not_after_timestamp_seconds{source="listener_host",listener="",name="we\"ird"`,
		`relayops_tls_reloads_total{listener="gateway",result="success"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %s\n%s", want, out)
		}
	}
	if strings.Contains(out, "PRIVATE") {
		t.Fatal("metrics contain key material")
	}
}

type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (k *memKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok, nil
}
func (k *memKV) Put(_ context.Context, key string, data []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = data
	return nil
}
func (k *memKV) Delete(_ context.Context, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, key)
	return nil
}

func TestEncryptedCache(t *testing.T) {
	ctx := context.Background()
	kv := &memKV{m: map[string][]byte{}}
	key := bytes.Repeat([]byte{7}, 32)
	c, err := certs.NewEncryptedCache(kv, key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("-----BEGIN EC PRIVATE KEY-----\nsecret\n-----END EC PRIVATE KEY-----\n")
	if err := c.Put(ctx, "api.example.com", secret); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(kv.m["api.example.com"], []byte("PRIVATE")) {
		t.Fatal("stored in clear text")
	}
	got, err := c.Get(ctx, "api.example.com")
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("round trip: %q %v", got, err)
	}
	if _, err := c.Get(ctx, "missing"); !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("missing key: %v", err)
	}
	// An entry moved to another key does not decrypt.
	kv.m["other.example.com"] = kv.m["api.example.com"]
	if _, err := c.Get(ctx, "other.example.com"); err == nil {
		t.Fatal("entry decrypted under another key name")
	}
	wrong, _ := certs.NewEncryptedCache(kv, bytes.Repeat([]byte{8}, 32))
	if _, err := wrong.Get(ctx, "api.example.com"); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
	if _, err := certs.NewEncryptedCache(kv, []byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	if err := c.Delete(ctx, "api.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "api.example.com"); !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatal("deleted entry still present")
	}
}

func TestNewACMEValidation(t *testing.T) {
	cache := autocert.DirCache(t.TempDir())
	if _, err := certs.NewACME(certs.ACMEConfig{Domains: []string{"api.example.com"}, Cache: cache}); err == nil {
		t.Fatal("terms of service must be accepted explicitly")
	}
	if _, err := certs.NewACME(certs.ACMEConfig{Domains: []string{"*.example.com"}, AcceptTOS: true, Cache: cache}); err == nil {
		t.Fatal("wildcards need DNS-01 and must be refused")
	}
	if _, err := certs.NewACME(certs.ACMEConfig{Domains: []string{"localhost"}, AcceptTOS: true, Cache: cache}); err == nil {
		t.Fatal("unqualified names must be refused")
	}
	a, err := certs.NewACME(certs.ACMEConfig{Domains: []string{" API.Example.com. ", "api.example.com"}, AcceptTOS: true, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Handles("api.example.com") || !a.Handles("API.EXAMPLE.COM") || a.Handles("evil.example.com") {
		t.Fatal("host policy wrong")
	}
}

func TestKeyPairFileReloads(t *testing.T) {
	ca := certtest.NewCA(t, "CA")
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key")
	write := func(l certtest.Leaf) {
		if err := os.WriteFile(certPath, l.CertPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, l.KeyPEM, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first := ca.Issue(t, "node-1", certtest.Options{Client: true})
	write(first)
	k, err := certs.LoadKeyPairFile(certPath, keyPath, certs.SourceDataplaneClient, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := certs.Leaf(k.Certificate()); got.SerialNumber.Cmp(first.Cert.SerialNumber) != 0 {
		t.Fatal("first certificate not served")
	}
	second := ca.Issue(t, "node-1", certtest.Options{Client: true})
	write(second)
	if got := certs.Leaf(k.Certificate()); got.SerialNumber.Cmp(second.Cert.SerialNumber) != 0 {
		t.Fatal("renewed certificate not picked up")
	}
	// A broken file keeps the last good certificate.
	if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := certs.Leaf(k.Certificate()); got == nil || got.SerialNumber.Cmp(second.Cert.SerialNumber) != 0 {
		t.Fatal("broken file replaced the certificate")
	}
	if st := k.Statuses(); len(st) != 1 || st[0].Source != certs.SourceDataplaneClient {
		t.Fatalf("statuses = %+v", st)
	}
}

func TestACMEOrderLockStopsDuplicateOrders(t *testing.T) {
	var asked []string
	a, err := certs.NewACME(certs.ACMEConfig{
		Domains: []string{"api.example.com"}, AcceptTOS: true, Cache: autocert.DirCache(t.TempDir()),
		DirectoryURL: "https://acme.invalid/directory", // never reached: the lock refuses first
		OrderLock: func(_ context.Context, host string) (bool, error) {
			asked = append(asked, host)
			return false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.GetCertificate(&tls.ClientHelloInfo{ServerName: "api.example.com", CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}})
	if !errors.Is(err, certs.ErrOrderInProgress) || len(asked) != 1 || asked[0] != "api.example.com" {
		t.Fatalf("err = %v, lock asked for %v", err, asked)
	}
	// A hostname outside the list is refused before the lock is consulted.
	if _, err := a.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.com"}); err == nil || len(asked) != 1 {
		t.Fatalf("unlisted host: err = %v, lock asked for %v", err, asked)
	}
}

func TestBudgetKeepsReportsSmall(t *testing.T) {
	var list []certs.Status
	for i := 0; i < 1000; i++ {
		names := make([]string, 50)
		for j := range names {
			names[j] = strings.Repeat("x", 30) + ".example.com"
		}
		list = append(list, certs.Status{Source: certs.SourceListenerHost, Name: "h", DNSNames: names, Fingerprint: strings.Repeat("a", 64)})
	}
	got := certs.Budget(list, 48<<10)
	raw, _ := json.Marshal(got)
	if len(raw) > 48<<10 || len(got) == 0 {
		t.Fatalf("budgeted report is %d bytes with %d entries", len(raw), len(got))
	}
	if n := len(got[0].DNSNames); n != 8 || !strings.Contains(got[0].DNSNames[7], "43 more") {
		t.Fatalf("DNS names = %d (%v)", n, got[0].DNSNames[7:])
	}
	if len(list[0].DNSNames) != 50 {
		t.Fatal("Budget modified its input")
	}
	if certs.Digest(got) == certs.Digest(got[1:]) {
		t.Fatal("digest does not tell reports apart")
	}
}

// A certificate already in the shared cache is served without consulting the
// order lock: before, the lock ran on every handshake, so a second node that
// did not hold it refused hostnames whose certificate it could have served.
func TestACMECachedCertificateNeedsNoOrderLock(t *testing.T) {
	dir := t.TempDir()
	cache := autocert.DirCache(dir)
	leaf := certtest.NewCA(t, "Issuer").Issue(t, "api.example.com", certtest.Options{DNSNames: []string{"api.example.com"}, Validity: 60 * 24 * time.Hour})
	if err := cache.Put(context.Background(), "api.example.com", append(append([]byte{}, leaf.KeyPEM...), leaf.CertPEM...)); err != nil {
		t.Fatal(err)
	}
	locks := 0
	a, err := certs.NewACME(certs.ACMEConfig{
		Domains: []string{"api.example.com"}, AcceptTOS: true, Cache: cache,
		DirectoryURL: "https://acme.invalid/directory", // never reached: the cache serves it
		OrderLock:    func(context.Context, string) (bool, error) { locks++; return false, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	hello := &tls.ClientHelloInfo{ServerName: "api.example.com", CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}}
	for i := 0; i < 3; i++ {
		c, err := a.GetCertificate(hello)
		if err != nil || certs.Leaf(c) == nil || certs.Leaf(c).SerialNumber.Cmp(leaf.Cert.SerialNumber) != 0 {
			t.Fatalf("handshake %d: cached certificate not served: %v", i, err)
		}
	}
	if locks != 0 {
		t.Fatalf("order lock consulted %d times for a cached certificate", locks)
	}
}
