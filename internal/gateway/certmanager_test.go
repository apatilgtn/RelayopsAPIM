package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relayops/apim/internal/certs"
	"github.com/relayops/apim/internal/certs/certtest"
	"golang.org/x/crypto/acme/autocert"
)

// tlsServer accepts connections with cfg and writes "ok" after a successful
// handshake, so a client sees a refused client certificate on its first read
// under TLS 1.3 too.
func tlsServer(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if c.(*tls.Conn).Handshake() == nil {
					_, _ = c.Write([]byte("ok"))
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// dial connects with SNI and an optional client certificate, returning the
// served leaf, or an error when the server refused the connection.
func dial(t *testing.T, addr, sni string, roots *x509.CertPool, client *tls.Certificate) (*x509.Certificate, error) {
	t.Helper()
	cfg := &tls.Config{ServerName: sni, RootCAs: roots}
	if client != nil {
		// Always present the certificate: a client given plain Certificates
		// sends none the server's CA list does not name.
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return client, nil }
	}
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil {
		return nil, err
	}
	return c.ConnectionState().PeerCertificates[0], nil
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCertManagerLoadsAndReloadsFiles(t *testing.T) {
	ca := certtest.NewCA(t, "Server CA")
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts")
	if err := os.Mkdir(hosts, 0o700); err != nil {
		t.Fatal(err)
	}
	def := ca.Issue(t, "default", certtest.Options{DNSNames: []string{"default.example.com"}})
	api1 := ca.Issue(t, "api-1", certtest.Options{DNSNames: []string{"api.example.com"}})
	wild := ca.Issue(t, "wild", certtest.Options{DNSNames: []string{"*.apps.example.com"}})
	writeFile(t, filepath.Join(dir, "default.crt"), def.CertPEM)
	writeFile(t, filepath.Join(dir, "default.key"), def.KeyPEM)
	writeFile(t, filepath.Join(hosts, "api.example.com.crt"), api1.CertPEM)
	writeFile(t, filepath.Join(hosts, "api.example.com.key"), api1.KeyPEM)
	writeFile(t, filepath.Join(hosts, "_wildcard.apps.example.com.crt"), wild.CertPEM)
	writeFile(t, filepath.Join(hosts, "_wildcard.apps.example.com.key"), wild.KeyPEM)

	inv := certs.NewInventory()
	m := NewDynamicCertManager(nil)
	m.SetName("gateway")
	m.SetInventory(inv)
	files := TLSFiles{CertFile: filepath.Join(dir, "default.crt"), KeyFile: filepath.Join(dir, "default.key"), Dir: hosts}
	if err := m.LoadFiles(files); err != nil {
		t.Fatal(err)
	}
	addr := tlsServer(t, m.TLSConfig())
	roots := certs.Pool([]*x509.Certificate{ca.Cert})

	served := func(sni string) string {
		t.Helper()
		leaf, err := dial(t, addr, sni, roots, nil)
		if err != nil {
			t.Fatalf("SNI %s: %v", sni, err)
		}
		return leaf.Subject.CommonName
	}
	if got := served("api.example.com"); got != "api-1" {
		t.Fatalf("api.example.com served %s", got)
	}
	if got := served("x.apps.example.com"); got != "wild" {
		t.Fatalf("wildcard served %s", got)
	}
	if got := served("default.example.com"); got != "default" {
		t.Fatalf("default served %s", got)
	}

	// Unchanged files: nothing to do.
	if changed, err := m.Reload(); changed || err != nil {
		t.Fatalf("reload of unchanged files: changed=%v err=%v", changed, err)
	}

	// Renewal: the new certificate is served on new connections.
	api2 := ca.Issue(t, "api-2", certtest.Options{DNSNames: []string{"api.example.com"}})
	writeFile(t, filepath.Join(hosts, "api.example.com.crt"), api2.CertPEM)
	writeFile(t, filepath.Join(hosts, "api.example.com.key"), api2.KeyPEM)
	if changed, err := m.Reload(); !changed || err != nil {
		t.Fatalf("reload after renewal: changed=%v err=%v", changed, err)
	}
	if got := served("api.example.com"); got != "api-2" {
		t.Fatalf("after renewal served %s", got)
	}

	// A half-written renewal (certificate without its key) is refused as a
	// whole and the previous certificates stay.
	api3 := ca.Issue(t, "api-3", certtest.Options{DNSNames: []string{"api.example.com"}})
	writeFile(t, filepath.Join(hosts, "api.example.com.crt"), api3.CertPEM)
	if _, err := m.Reload(); err == nil {
		t.Fatal("mismatched certificate and key accepted")
	}
	if got := served("api.example.com"); got != "api-2" {
		t.Fatalf("after a failed reload served %s", got)
	}

	// SIGHUP-style reload through Watch.
	writeFile(t, filepath.Join(hosts, "api.example.com.key"), api3.KeyPEM)
	ctx, cancel := contextWithCancel(t)
	reload := make(chan struct{}, 1)
	go m.Watch(ctx, 0, reload)
	reload <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for served("api.example.com") != "api-3" {
		if time.Now().After(deadline) {
			t.Fatal("Watch did not reload on signal")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	var buf bytes.Buffer
	inv.WriteMetrics(&buf)
	for _, want := range []string{
		`relayops_tls_reloads_total{listener="gateway",result="success"} 3`,
		`relayops_tls_reloads_total{listener="gateway",result="error"} 1`,
		`source="listener_host",listener="gateway",name="*.apps.example.com"`,
		`source="listener",listener="gateway",name="default"`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("metrics missing %s\n%s", want, buf.String())
		}
	}
}

func TestCertManagerClientCAPerHostAndCRL(t *testing.T) {
	serverCA := certtest.NewCA(t, "Server CA")
	globalCA := certtest.NewCA(t, "Global clients")
	partnerCA := certtest.NewCA(t, "Partner clients")
	dir := t.TempDir()
	srv := serverCA.Issue(t, "gw", certtest.Options{DNSNames: []string{"partner.example.com", "api.example.com"}})
	writeFile(t, filepath.Join(dir, "partner.example.com.crt"), srv.CertPEM)
	writeFile(t, filepath.Join(dir, "partner.example.com.key"), srv.KeyPEM)
	writeFile(t, filepath.Join(dir, "partner.example.com.client-ca.pem"), partnerCA.PEM)
	writeFile(t, filepath.Join(dir, "api.example.com.crt"), srv.CertPEM)
	writeFile(t, filepath.Join(dir, "api.example.com.key"), srv.KeyPEM)
	globalCAFile := filepath.Join(dir, "global-ca.pem")
	writeFile(t, globalCAFile, globalCA.PEM)

	revoked := globalCA.Issue(t, "revoked-client", certtest.Options{Client: true})
	crlFile := filepath.Join(t.TempDir(), "clients.crl")
	writeFile(t, crlFile, globalCA.CRL(t, time.Now().Add(time.Hour), revoked.Cert.SerialNumber))

	m := NewDynamicCertManager(nil)
	m.SetInventory(certs.NewInventory())
	m.RequireClientCertificates(true)
	if err := m.LoadFiles(TLSFiles{Dir: dir, ClientCAFile: globalCAFile, CRLFile: crlFile}); err != nil {
		t.Fatal(err)
	}
	addr := tlsServer(t, m.TLSConfig())
	roots := certs.Pool([]*x509.Certificate{serverCA.Cert})
	globalClient := globalCA.Issue(t, "global-client", certtest.Options{Client: true}).TLS(t)
	partnerClient := partnerCA.Issue(t, "partner-client", certtest.Options{Client: true}).TLS(t)
	revokedClient := revoked.TLS(t)

	for _, tc := range []struct {
		name   string
		sni    string
		client *tls.Certificate
		ok     bool
	}{
		{"global client on the default CA", "api.example.com", &globalClient, true},
		{"partner client on the default CA", "api.example.com", &partnerClient, false},
		{"partner client on its hostname", "partner.example.com", &partnerClient, true},
		{"global client on the partner hostname", "partner.example.com", &globalClient, false},
		{"no client certificate (required)", "api.example.com", nil, false},
		{"revoked client", "api.example.com", &revokedClient, false},
	} {
		_, err := dial(t, addr, tc.sni, roots, tc.client)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	var buf bytes.Buffer
	m.writeMetrics(&buf)
	if !strings.Contains(buf.String(), `relayops_tls_revoked_refusals_total{listener="gateway"} 1`) {
		t.Errorf("revocation refusal not counted:\n%s", buf.String())
	}
	st := m.Statuses()
	var cas []string
	for _, s := range st {
		if s.Source == certs.SourceClientCA {
			cas = append(cas, s.Name)
		}
	}
	if strings.Join(cas, ",") != "default,partner.example.com" {
		t.Errorf("client CA statuses = %v", cas)
	}
}

func TestCertManagerOptionalClientCertificates(t *testing.T) {
	serverCA := certtest.NewCA(t, "Server CA")
	clientCA := certtest.NewCA(t, "Clients")
	srv := serverCA.Issue(t, "gw", certtest.Options{DNSNames: []string{"api.example.com"}})
	pair := srv.TLS(t)
	m := NewDynamicCertManager(&pair)
	if err := m.SetClientCAs(clientCA.PEM); err != nil {
		t.Fatal(err)
	}
	addr := tlsServer(t, m.TLSConfig())
	roots := certs.Pool([]*x509.Certificate{serverCA.Cert})
	if _, err := dial(t, addr, "api.example.com", roots, nil); err != nil {
		t.Fatalf("optional mode refused a client without a certificate: %v", err)
	}
	stranger := certtest.NewCA(t, "Stranger").Issue(t, "x", certtest.Options{Client: true}).TLS(t)
	if _, err := dial(t, addr, "api.example.com", roots, &stranger); err == nil {
		t.Fatal("an unverifiable client certificate was accepted")
	}
	if err := m.SetHostClientCAs("api.example.com", []byte("junk")); err == nil {
		t.Fatal("junk client CA accepted")
	}
}

func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}

func TestCertManagerServesHostnamesEndingInCA(t *testing.T) {
	ca := certtest.NewCA(t, "Server CA")
	dir := t.TempDir()
	leaf := ca.Issue(t, "example.ca", certtest.Options{DNSNames: []string{"example.ca"}})
	writeFile(t, filepath.Join(dir, "example.ca.crt"), leaf.CertPEM)
	writeFile(t, filepath.Join(dir, "example.ca.key"), leaf.KeyPEM)
	m := NewDynamicCertManager(nil)
	if err := m.LoadFiles(TLSFiles{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "example.ca"})
	if err != nil || certs.Leaf(got).Subject.CommonName != "example.ca" {
		t.Fatalf("example.ca: %v", err)
	}
	cfg, _ := m.TLSConfig().GetConfigForClient(&tls.ClientHelloInfo{ServerName: "example.ca"})
	if cfg.ClientAuth != tls.NoClientCert {
		t.Fatal("a server certificate was taken for a client CA")
	}
}

func TestCertManagerACMEValidationNeedsNoClientCertificate(t *testing.T) {
	clientCA := certtest.NewCA(t, "Clients")
	a, err := certs.NewACME(certs.ACMEConfig{Domains: []string{"api.example.com"}, AcceptTOS: true, Cache: mustDirCache(t)})
	if err != nil {
		t.Fatal(err)
	}
	m := NewDynamicCertManager(nil)
	m.SetACME(a)
	m.RequireClientCertificates(true)
	if err := m.SetClientCAs(clientCA.PEM); err != nil {
		t.Fatal(err)
	}
	get := m.TLSConfig().GetConfigForClient
	normal, _ := get(&tls.ClientHelloInfo{ServerName: "api.example.com", SupportedProtos: []string{"h2"}})
	if normal.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("normal handshake client auth = %v", normal.ClientAuth)
	}
	challenge, _ := get(&tls.ClientHelloInfo{ServerName: "api.example.com", SupportedProtos: []string{certs.ALPNProto}})
	if challenge.ClientAuth != tls.NoClientCert || len(challenge.NextProtos) != 1 || challenge.NextProtos[0] != certs.ALPNProto {
		t.Fatalf("validation config = %v %v", challenge.ClientAuth, challenge.NextProtos)
	}
	if !strings.Contains(strings.Join(normal.NextProtos, ","), certs.ALPNProto) {
		t.Fatal("listener does not offer acme-tls/1")
	}
}

func mustDirCache(t *testing.T) autocert.Cache {
	t.Helper()
	c, err := certs.DirCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Requiring client certificates applies only where a client CA applies: with
// a CA for one hostname and no listener-wide CA, other hostnames must still
// accept clients without a certificate.
func TestRequireClientCertificatesOnlyWhereACAApplies(t *testing.T) {
	serverCA := certtest.NewCA(t, "Server CA")
	partnerCA := certtest.NewCA(t, "Partner clients")
	dir := t.TempDir()
	srv := serverCA.Issue(t, "gw", certtest.Options{DNSNames: []string{"partner.example.com", "open.example.com"}})
	for _, h := range []string{"partner.example.com", "open.example.com"} {
		writeFile(t, filepath.Join(dir, h+".crt"), srv.CertPEM)
		writeFile(t, filepath.Join(dir, h+".key"), srv.KeyPEM)
	}
	writeFile(t, filepath.Join(dir, "partner.example.com.client-ca.pem"), partnerCA.PEM)

	m := NewDynamicCertManager(nil)
	m.SetInventory(certs.NewInventory())
	m.RequireClientCertificates(true)
	if err := m.LoadFiles(TLSFiles{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	addr := tlsServer(t, m.TLSConfig())
	roots := certs.Pool([]*x509.Certificate{serverCA.Cert})
	partnerClient := partnerCA.Issue(t, "partner-client", certtest.Options{Client: true}).TLS(t)
	for _, tc := range []struct {
		name   string
		sni    string
		client *tls.Certificate
		ok     bool
	}{
		{"hostname without a client CA, no certificate", "open.example.com", nil, true},
		{"hostname with a client CA, no certificate", "partner.example.com", nil, false},
		{"hostname with a client CA, its client", "partner.example.com", &partnerClient, true},
	} {
		_, err := dial(t, addr, tc.sni, roots, tc.client)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	if !m.HasHostClientCAs() || !m.HostScopedClientCA("PARTNER.example.com.") || m.HostScopedClientCA("open.example.com") {
		t.Error("host-scoped client CA lookup is wrong")
	}
}
