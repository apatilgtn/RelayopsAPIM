package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/relayops/apim/internal/store"
)

// mtls authentication trusts the X-Client-Cert-* headers, so the gateway must
// only ever derive them from a verified certificate: a caller sending a known
// (public) fingerprint as a header was once authenticated as its owner.
func TestClientCertHeadersCannotBeSpoofed(t *testing.T) {
	const fp = "AABBCCDDEEFF00112233445566778899AABBCCDDEEFF00112233445566778899"
	up := newUpstream(t, 200)
	g := newTestGateway(t)
	api := testAPI("partner", "/partner", up.srv.URL)
	api.AuthType = "mtls"
	api.RequestHeaders = map[string]string{"Allowed-Client-Fingerprint": fp}
	g.load(t, store.SnapshotData{APIs: []store.API{api}})

	send := func(remote string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/partner/orders", nil)
		req.RemoteAddr = remote
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		return rec
	}

	spoof := map[string]string{"X-Client-Cert-Fingerprint": fp, "X-Client-Cert-CN": "partner"}
	if rec := send("203.0.113.9:4000", spoof); rec.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed fingerprint header over plain HTTP: %d (want 401)", rec.Code)
	}
	if rec := send("203.0.113.9:4000", map[string]string{"X-SSL-Client-SHA256": fp}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed ingress header from an untrusted address: %d (want 401)", rec.Code)
	}
	if up.hits.Load() != 0 {
		t.Fatal("a spoofed request reached the upstream")
	}

	// A TLS-terminating ingress in a trusted network may forward the verified certificate.
	nets, err := ParseCIDRs("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	g.SetTrustedProxies(nets)
	if rec := send("10.1.2.3:4000", map[string]string{"X-SSL-Client-SHA256": fp, "X-SSL-Client-CN": "partner"}); rec.Code != http.StatusOK {
		t.Fatalf("verified certificate from a trusted proxy: %d %s", rec.Code, rec.Body)
	}
	// The trusted proxy reporting a failed verification is not accepted.
	if rec := send("10.1.2.3:4000", map[string]string{"X-SSL-Client-Cert": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----", "X-SSL-Client-Verify": "FAILED"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unverified certificate from a trusted proxy: %d (want 401)", rec.Code)
	}
	// Still refused from outside the trusted network.
	if rec := send("198.51.100.7:4000", map[string]string{"X-SSL-Client-SHA256": fp}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ingress header from outside the trusted network: %d (want 401)", rec.Code)
	}
}

func TestParseCIDRs(t *testing.T) {
	nets, err := ParseCIDRs("10.0.0.0/8, 192.168.1.5, ::1")
	if err != nil || len(nets) != 3 {
		t.Fatalf("%v %v", nets, err)
	}
	if _, err := ParseCIDRs("not-an-ip"); err == nil {
		t.Fatal("invalid CIDR accepted")
	}
}
