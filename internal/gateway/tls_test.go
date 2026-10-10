package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"
)

func generateTestCertificate(t *testing.T, dnsNames ...string) *tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate ecdsa key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			Organization: []string{"RelayOps Test"},
			CommonName:   dnsNames[0],
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	cert := &tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  priv,
	}
	return cert
}

type mockDynamicProvider struct {
	cert *tls.Certificate
}

func (m *mockDynamicProvider) FetchCertificate(domain, tenantSlug string) (*tls.Certificate, error) {
	if domain == "on-demand.tenant.com" {
		return m.cert, nil
	}
	return nil, ErrNoMatchingCertificate
}

func TestDynamicCertManager(t *testing.T) {
	defaultCert := generateTestCertificate(t, "api.default.example.com")
	acmeCert := generateTestCertificate(t, "acme.tenant.example.com")
	wildcardCert := generateTestCertificate(t, "*.partner.example.com")
	onDemandCert := generateTestCertificate(t, "on-demand.tenant.com")

	provider := &mockDynamicProvider{cert: onDemandCert}
	mgr := NewDynamicCertManager(defaultCert, provider)

	mgr.RegisterCertificate("acme.tenant.example.com", "acme-tenant", acmeCert)
	mgr.RegisterCertificate("*.partner.example.com", "partner-tenant", wildcardCert)

	// 1. Exact match
	cert, err := mgr.GetCertificate(&tls.ClientHelloInfo{ServerName: "acme.tenant.example.com"})
	if err != nil || cert != acmeCert {
		t.Fatalf("expected acmeCert, got %v (err: %v)", cert, err)
	}

	// 2. Case-insensitive exact match
	certUpper, err := mgr.GetCertificate(&tls.ClientHelloInfo{ServerName: "ACME.TENANT.EXAMPLE.COM"})
	if err != nil || certUpper != acmeCert {
		t.Fatalf("expected acmeCert for uppercase ServerName, got %v (err: %v)", certUpper, err)
	}

	// 3. Wildcard match
	wCert, err := mgr.GetCertificate(&tls.ClientHelloInfo{ServerName: "sub.partner.example.com"})
	if err != nil || wCert != wildcardCert {
		t.Fatalf("expected wildcardCert, got %v (err: %v)", wCert, err)
	}

	// 4. On-demand dynamic resolution via provider
	odCert, err := mgr.GetCertificate(&tls.ClientHelloInfo{ServerName: "on-demand.tenant.com"})
	if err != nil || odCert != onDemandCert {
		t.Fatalf("expected onDemandCert, got %v (err: %v)", odCert, err)
	}

	// 5. Fallback to default certificate
	fbCert, err := mgr.GetCertificate(&tls.ClientHelloInfo{ServerName: "unknown.domain.com"})
	if err != nil || fbCert != defaultCert {
		t.Fatalf("expected defaultCert fallback, got %v (err: %v)", fbCert, err)
	}

	// 6. Tenant domain resolution
	tenant, ok := mgr.GetTenantForDomain("acme.tenant.example.com")
	if !ok || tenant != "acme-tenant" {
		t.Fatalf("expected tenant 'acme-tenant', got %q (found: %v)", tenant, ok)
	}

	wTenant, wOk := mgr.GetTenantForDomain("deep.partner.example.com")
	if !wOk || wTenant != "partner-tenant" {
		t.Fatalf("expected tenant 'partner-tenant' via wildcard, got %q (found: %v)", wTenant, wOk)
	}
}

func TestMTLSClientCAAndExtraction(t *testing.T) {
	clientCert := generateTestCertificate(t, "client-app-1")
	caCert := generateTestCertificate(t, "ca.example.com")

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Certificate[0]})
	mgr := NewDynamicCertManager(clientCert, nil)

	if err := mgr.SetClientCAs(caPEM); err != nil {
		t.Fatalf("SetClientCAs failed: %v", err)
	}
	mgr.SetClientAuth(tls.RequireAndVerifyClientCert)

	tlsCfg := mgr.TLSConfig()
	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("expected ClientAuth RequireAndVerifyClientCert, got %v", tlsCfg.ClientAuth)
	}
	if tlsCfg.ClientCAs == nil {
		t.Fatal("expected non-nil ClientCAs")
	}

	// 1. Direct TLS extraction
	parsedClientCert, err := x509.ParseCertificate(clientCert.Certificate[0])
	if err != nil {
		t.Fatalf("failed to parse client cert: %v", err)
	}

	reqDirect, _ := http.NewRequest("GET", "https://api.example.com/orders", nil)
	reqDirect.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{parsedClientCert},
	}
	infoDirect := ExtractClientCertificate(reqDirect)
	if infoDirect == nil || infoDirect.CommonName != "client-app-1" || infoDirect.FingerprintSHA256 == "" || !infoDirect.Verified {
		t.Fatalf("direct extraction failed: %+v", infoDirect)
	}

	// 2. Header extraction (X-Client-Cert-Fingerprint)
	reqHeader, _ := http.NewRequest("GET", "http://api.example.com/orders", nil)
	reqHeader.Header.Set("X-Client-Cert-Fingerprint", "AA:BB:CC:DD")
	reqHeader.Header.Set("X-Client-Cert-CN", "header-client")
	infoHdr := ExtractClientCertificate(reqHeader)
	if infoHdr == nil || infoHdr.FingerprintSHA256 != "AABBCCDD" || infoHdr.CommonName != "header-client" {
		t.Fatalf("header extraction failed: %+v", infoHdr)
	}
}
