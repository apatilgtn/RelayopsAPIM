package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relayops/apim/internal/certs"
	"github.com/relayops/apim/internal/certs/certtest"
	"github.com/relayops/apim/internal/store"
)

// mtlsUpstream is an HTTPS backend with a private CA that requires a client
// certificate from clientCA and reports the client's common name.
type mtlsUpstream struct {
	srv      *httptest.Server
	serverCA *certtest.CA
	clientCA *certtest.CA
	lastCN   atomic.Value
	hits     atomic.Int64
}

func newMTLSUpstream(t *testing.T) *mtlsUpstream {
	t.Helper()
	u := &mtlsUpstream{serverCA: certtest.NewCA(t, "Backend CA"), clientCA: certtest.NewCA(t, "Gateway clients")}
	leaf := u.serverCA.Issue(t, "billing.internal", certtest.Options{DNSNames: []string{"billing.internal"}})
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		cn := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		u.lastCN.Store(cn)
		w.Header().Set("X-Client-CN", cn)
		w.Write([]byte("ok"))
	}))
	u.srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{leaf.TLS(t)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    certs.Pool([]*x509.Certificate{u.clientCA.Cert}),
	}
	u.srv.StartTLS()
	t.Cleanup(u.srv.Close)
	return u
}

func tlsAPI(u *mtlsUpstream, tlsPolicy *store.UpstreamTLS) store.API {
	a := testAPI("billing", "/billing", u.srv.URL)
	a.TrafficPolicy.UpstreamTLS = tlsPolicy
	return a
}

// secretEnv makes a secret available to the gateway: as a value or, with
// file set, through RELAYOPS_SECRET_<NAME>_FILE.
func secretEnv(t *testing.T, name string, value []byte, file bool) string {
	t.Helper()
	if !file {
		t.Setenv("RELAYOPS_SECRET_"+name, string(value))
		return ""
	}
	p := filepath.Join(t.TempDir(), strings.ToLower(name)+".pem")
	writeFile(t, p, value)
	t.Setenv("RELAYOPS_SECRET_"+name+"_FILE", p)
	return p
}

func TestUpstreamTLSPrivateCAAndClientCertificate(t *testing.T) {
	u := newMTLSUpstream(t)
	client := u.clientCA.Issue(t, "gateway-1", certtest.Options{Client: true})
	secretEnv(t, "UT_CA", u.serverCA.PEM, false)
	certFile := secretEnv(t, "UT_CERT", client.CertPEM, true)
	keyFile := secretEnv(t, "UT_KEY", client.KeyPEM, true)
	policy := &store.UpstreamTLS{
		CA: "${secret:UT_CA}", ClientCertificate: "${secret:UT_CERT}", ClientKey: "${secret:UT_KEY}",
		ServerName: "billing.internal", MinVersion: "1.3",
	}

	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{tlsAPI(u, policy)}})
	rec := do(g, "GET", "/billing/invoices", nil, "")
	if rec.Code != 200 || rec.Header().Get("X-Client-CN") != "gateway-1" {
		t.Fatalf("status %d, client CN %q: %s", rec.Code, rec.Header().Get("X-Client-CN"), rec.Body.String())
	}

	// Reported, labelled with the API, without key material.
	var sources []string
	for _, s := range g.Certificates() {
		if s.APIID == "id-billing" {
			sources = append(sources, s.Source)
		}
	}
	if strings.Join(sources, ",") != "upstream_ca,upstream_client" && strings.Join(sources, ",") != "upstream_client,upstream_ca" {
		t.Fatalf("reported sources = %v", sources)
	}

	// Rotation: a renewed client certificate written to the _FILE secrets is
	// used after the next configuration reload, on a new transport.
	renewed := u.clientCA.Issue(t, "gateway-1-renewed", certtest.Options{Client: true})
	writeFile(t, certFile, renewed.CertPEM)
	writeFile(t, keyFile, renewed.KeyPEM)
	before := transportCount(g)
	g.load(t, store.SnapshotData{APIs: []store.API{tlsAPI(u, policy)}})
	rec = do(g, "GET", "/billing/invoices", nil, "")
	if rec.Code != 200 || rec.Header().Get("X-Client-CN") != "gateway-1-renewed" {
		t.Fatalf("after rotation: status %d, client CN %q", rec.Code, rec.Header().Get("X-Client-CN"))
	}
	if after := transportCount(g); after != before {
		t.Fatalf("the replaced transport was not pruned: %d transports before, %d after", before, after)
	}
}

func transportCount(g *Gateway) int {
	g.tmu.Lock()
	defer g.tmu.Unlock()
	return len(g.transports)
}

func TestUpstreamTLSWithoutSettingsDoesNotTrustPrivateCA(t *testing.T) {
	u := newMTLSUpstream(t)
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{tlsAPI(u, nil)}})
	if rec := do(g, "GET", "/billing/x", nil, ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: a private CA must not be trusted by default", rec.Code)
	}
	if u.hits.Load() != 0 {
		t.Fatal("upstream reached")
	}
}

func TestUpstreamTLSFailsClosedWhenSecretsMissing(t *testing.T) {
	u := newMTLSUpstream(t)
	g := newTestGateway(t)
	api := tlsAPI(u, &store.UpstreamTLS{CA: "${secret:UT_MISSING_CA}"})
	loadWithErrors := func(api store.API) []error {
		snap, errs := buildSnapshot(store.SnapshotData{APIs: []store.API{api}}, 10, g.upstreams)
		g.swap(snap)
		return errs
	}
	if errs := loadWithErrors(api); len(errs) != 1 || !strings.Contains(errs[0].Error(), "UT_MISSING_CA") {
		t.Fatalf("the reload must report the unresolved secret: %v", errs)
	}
	rec := do(g, "GET", "/billing/x", nil, "")
	if rec.Code != http.StatusBadGateway || rec.Header().Get("X-RelayOps-Decision-Reason") != "upstream_tls_unavailable" {
		t.Fatalf("status %d reason %q, want 502 upstream_tls_unavailable", rec.Code, rec.Header().Get("X-RelayOps-Decision-Reason"))
	}
	if u.hits.Load() != 0 {
		t.Fatal("upstream called without the configured trust")
	}
	if strings.Contains(rec.Body.String(), "BEGIN") {
		t.Fatal("response leaks secret material")
	}
	var buf bytes.Buffer
	g.CertificateInventory().WriteMetrics(&buf)
	if !strings.Contains(buf.String(), `relayops_upstream_tls_errors_total{api_id="id-billing"} 1`) {
		t.Fatalf("refusal not counted:\n%s", buf.String())
	}

	// A secret that is not PEM fails closed too, without echoing its value.
	t.Setenv("RELAYOPS_SECRET_UT_NOT_PEM", "hunter2-not-a-certificate")
	api.TrafficPolicy.UpstreamTLS = &store.UpstreamTLS{CA: "${secret:UT_NOT_PEM}"}
	if errs := loadWithErrors(api); len(errs) != 1 || strings.Contains(errs[0].Error(), "hunter2") {
		t.Fatalf("errors = %v", errs)
	}
	rec = do(g, "GET", "/billing/x", nil, "")
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	for _, s := range g.Snapshot().Routes {
		if s.Pool.tls == nil || s.Pool.tls.err == nil || strings.Contains(s.Pool.tls.err.Error(), "hunter2") {
			t.Fatalf("error = %v", s.Pool.tls.err)
		}
	}
}

func TestUpstreamTLSIsPerRevision(t *testing.T) {
	u := newMTLSUpstream(t)
	client := u.clientCA.Issue(t, "canary-gw", certtest.Options{Client: true})
	secretEnv(t, "UT_CA", u.serverCA.PEM, false)
	secretEnv(t, "UT_CERT", client.CertPEM, false)
	secretEnv(t, "UT_KEY", client.KeyPEM, false)

	stable := tlsAPI(u, &store.UpstreamTLS{CA: "${secret:UT_CA}", ServerName: "billing.internal"}) // no client certificate: refused by the backend
	canary := tlsAPI(u, &store.UpstreamTLS{CA: "${secret:UT_CA}", ServerName: "billing.internal",
		ClientCertificate: "${secret:UT_CERT}", ClientKey: "${secret:UT_KEY}"})
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{
		Revision: 10, APIs: []store.API{stable},
		Canary: &store.CanaryData{Revision: 11, APIs: []store.API{canary}, Header: "X-Canary"},
	})
	if rec := do(g, "GET", "/billing/x", map[string]string{"X-Canary": "1"}, ""); rec.Code != 200 {
		t.Fatalf("canary: status %d", rec.Code)
	}
	if rec := do(g, "GET", "/billing/x", nil, ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("stable must keep its own TLS settings (no client certificate): status %d", rec.Code)
	}
}

func TestUpstreamTLSHealthChecksUseAPISettings(t *testing.T) {
	u := newMTLSUpstream(t)
	client := u.clientCA.Issue(t, "prober", certtest.Options{Client: true})
	secretEnv(t, "UT_CA", u.serverCA.PEM, false)
	secretEnv(t, "UT_CERT", client.CertPEM, false)
	secretEnv(t, "UT_KEY", client.KeyPEM, false)
	api := tlsAPI(u, &store.UpstreamTLS{CA: "${secret:UT_CA}", ServerName: "billing.internal",
		ClientCertificate: "${secret:UT_CERT}", ClientKey: "${secret:UT_KEY}"})
	api.TrafficPolicy.HealthCheck = &store.HealthCheckPolicy{Path: "/healthz", IntervalSeconds: 1, TimeoutMS: 2000, UnhealthyThreshold: 1, HealthyThreshold: 1}
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{api}})

	base := &http.Client{Transport: &http.Transport{}}
	g.probeDue(context.Background(), base, time.Now())
	deadline := time.Now().Add(5 * time.Second)
	for u.hits.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("health check never reached the upstream")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for g.Snapshot().Routes[0].Pool.targets[0].probeRunning.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	if cn, _ := u.lastCN.Load().(string); cn != "prober" {
		t.Fatalf("probe presented %q", cn)
	}
	if !g.Snapshot().Routes[0].Pool.targets[0].healthy.Load() {
		t.Fatal("target marked unhealthy: the probe did not use the API's TLS settings")
	}
}

func TestUpstreamTLSValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		tls  store.UpstreamTLS
		ok   bool
	}{
		{"ca reference", store.UpstreamTLS{CA: "${secret:CA}"}, true},
		{"client pair", store.UpstreamTLS{ClientCertificate: "${secret:C}", ClientKey: "${secret:K}"}, true},
		{"server name only", store.UpstreamTLS{ServerName: "billing.internal"}, true},
		{"tls 1.3 only", store.UpstreamTLS{MinVersion: "1.3"}, true},
		{"empty", store.UpstreamTLS{}, false},
		{"literal PEM", store.UpstreamTLS{CA: "-----BEGIN CERTIFICATE-----"}, false},
		{"reference with prefix", store.UpstreamTLS{CA: "Bearer ${secret:CA}"}, false},
		{"certificate without key", store.UpstreamTLS{ClientCertificate: "${secret:C}"}, false},
		{"bad server name", store.UpstreamTLS{ServerName: "bad name"}, false},
		{"tls 1.1", store.UpstreamTLS{CA: "${secret:CA}", MinVersion: "1.1"}, false},
	} {
		p := store.TrafficPolicy{UpstreamTLS: &tc.tls}
		p.Normalize()
		if err := p.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
}
