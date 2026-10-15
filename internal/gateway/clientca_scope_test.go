package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relayops/apim/internal/certs/certtest"
	"github.com/relayops/apim/internal/store"
)

// fakeScope stands in for the TLS listener's certificate manager.
type fakeScope struct{ hosts map[string]bool }

func (f fakeScope) HasHostClientCAs() bool { return len(f.hosts) > 0 }
func (f fakeScope) HostScopedClientCA(name string) bool {
	return f.hosts[strings.TrimSuffix(strings.ToLower(name), ".")]
}

// mtlsRequest builds a request as if it arrived on a TLS connection for sni
// with a verified client certificate.
func mtlsRequest(t *testing.T, host, sni string, cert *x509.Certificate) *http.Request {
	t.Helper()
	r := httptest.NewRequest("GET", "/b-api/orders", nil)
	r.Host = host
	r.TLS = &tls.ConnectionState{
		ServerName:       sni,
		PeerCertificates: []*x509.Certificate{cert},
		VerifiedChains:   [][]*x509.Certificate{{cert}},
	}
	return r
}

// A hostname's own client CA can issue any common name, so a certificate it
// verified must not satisfy another API's common-name allowance; and a Host
// that differs from the TLS server name is refused, because the handshake
// chose the CA for the server name.
func TestHostScopedClientCAIsolation(t *testing.T) {
	up := newUpstream(t)
	g := newTestGateway(t)
	a := testAPI("tenant-b", "/b-api", up.srv.URL)
	a.AuthType = "mtls"
	a.RequestHeaders = map[string]string{"Allowed-Client-CN": "b-client"}
	g.load(t, store.SnapshotData{APIs: []store.API{a}})
	g.SetClientCAScope(fakeScope{hosts: map[string]bool{"a.example.com": true}})

	// Tenant A's CA mints a certificate carrying tenant B's allowed CN.
	forged := certtest.NewCA(t, "Tenant A clients").Issue(t, "b-client", certtest.Options{Client: true}).Cert

	serve := func(r *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, r)
		return rec
	}
	if rec := serve(mtlsRequest(t, "a.example.com", "a.example.com", forged)); rec.Code != http.StatusForbidden {
		t.Errorf("host-scoped certificate accepted by common name: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(mtlsRequest(t, "b.example.com", "a.example.com", forged)); rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("Host differing from the TLS server name: %d, want 421", rec.Code)
	}
	// The listener-wide CA (no host scope) keeps the common-name allowance.
	if rec := serve(mtlsRequest(t, "c.example.com", "c.example.com", forged)); rec.Code != http.StatusOK {
		t.Errorf("listener-wide CA certificate with the allowed CN: %d %s", rec.Code, rec.Body.String())
	}
	// A caller cannot clear the scope by sending the header itself.
	r := mtlsRequest(t, "a.example.com", "a.example.com", forged)
	r.Header.Set("X-Client-Cert-Scope", "default")
	if rec := serve(r); rec.Code != http.StatusForbidden {
		t.Errorf("caller-supplied scope header was trusted: %d", rec.Code)
	}
}
