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

func TestDynamicCertManager(t *testing.T) {
	defaultCert := generateTestCertificate(t, "api.default.example.com")
	exactCert := generateTestCertificate(t, "acme.tenant.example.com")
	wildcardCert := generateTestCertificate(t, "*.partner.example.com")

	mgr := NewDynamicCertManager(defaultCert)
	mgr.RegisterCertificate("acme.tenant.example.com", exactCert)
	mgr.RegisterCertificate("*.partner.example.com", wildcardCert)

	for _, tc := range []struct {
		sni  string
		want *tls.Certificate
	}{
		{"acme.tenant.example.com", exactCert},
		{"ACME.TENANT.EXAMPLE.COM.", exactCert}, // case and trailing dot
		{"sub.partner.example.com", wildcardCert},
		{"deep.sub.partner.example.com", defaultCert}, // a wildcard covers one label only
		{"unknown.domain.com", defaultCert},
		{"", defaultCert},
	} {
		got, err := mgr.GetCertificate(&tls.ClientHelloInfo{ServerName: tc.sni})
		if err != nil || got != tc.want {
			t.Errorf("SNI %q: got %v (err %v)", tc.sni, got, err)
		}
	}

	mgr.UnregisterDomain("acme.tenant.example.com")
	if got, _ := mgr.GetCertificate(&tls.ClientHelloInfo{ServerName: "acme.tenant.example.com"}); got != defaultCert {
		t.Fatal("unregistered domain still served")
	}
	none := NewDynamicCertManager(nil)
	if _, err := none.GetCertificate(&tls.ClientHelloInfo{ServerName: "x.example.com"}); err == nil {
		t.Fatal("a manager without certificates must fail the handshake")
	}
}

func TestMTLSClientCAAndExtraction(t *testing.T) {
	clientCert := generateTestCertificate(t, "client-app-1")
	caCert := generateTestCertificate(t, "ca.example.com")

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Certificate[0]})
	mgr := NewDynamicCertManager(clientCert)

	if err := mgr.SetClientCAs(caPEM); err != nil {
		t.Fatalf("SetClientCAs failed: %v", err)
	}
	mgr.SetClientAuth(tls.RequireAndVerifyClientCert)

	tlsCfg, _ := mgr.TLSConfig().GetConfigForClient(&tls.ClientHelloInfo{ServerName: "api.example.com"})
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
