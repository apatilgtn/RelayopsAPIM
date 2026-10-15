package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relayops/apim/internal/certs"
	"github.com/relayops/apim/internal/certs/certtest"
	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/store"
)

func TestIntegrationCertificateReportSignalsAndAlerts(t *testing.T) {
	s := openAdminTestStore(t)
	ctx := context.Background()
	ca := certtest.NewCA(t, "Fleet CA")
	soon := ca.Issue(t, "api.example.com", certtest.Options{DNSNames: []string{"api.example.com"}, Validity: 3 * 24 * time.Hour})
	fine := ca.Issue(t, "ok.example.com", certtest.Options{DNSNames: []string{"ok.example.com"}, Validity: 200 * 24 * time.Hour})

	// A gateway reports through its acknowledgement (DBSource), as gateways do.
	gwStatus := certs.Summarize(soon.Cert, certs.SourceListenerHost, "api.example.com", certs.RoleServer)
	gwStatus.Listener = "gateway"
	src := gateway.NewDBSource(s)
	// The same certificate serves a second hostname: one row each, one alert.
	gwStatus2 := gwStatus
	gwStatus2.Name = "www.example.com"
	if err := src.Ack(ctx, gateway.NodeAck{NodeID: "gw-1", Revision: 1, NodeGroup: "default",
		Certificates: []certs.Status{gwStatus, gwStatus2}, CertificatesIncluded: true}); err != nil {
		t.Fatal(err)
	}
	// The control plane's own listener certificate is read live.
	a := newControlPlane(t, s, "cp-a")
	a.gw.CertificateInventory().Register(func() []certs.Status {
		st := certs.Summarize(fine.Cert, certs.SourceListener, "default", certs.RoleServer)
		st.Listener = "admin"
		return []certs.Status{st}
	})

	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	get := func(path string, into any) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer test-admin-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if into != nil && resp.StatusCode == 200 {
			if err := json.Unmarshal(raw, into); err != nil {
				t.Fatalf("%s: %v: %s", path, err, raw)
			}
		}
		return resp.StatusCode
	}

	var rep certificateReport
	if code := get("/api/system/certificates", &rep); code != 200 {
		t.Fatalf("status %d", code)
	}
	if rep.Summary["total"] != 2 || rep.Summary["expiring_14d"] != 1 || rep.Summary["expired"] != 0 || len(rep.Certificates) != 3 {
		t.Fatalf("summary = %v", rep.Summary)
	}
	if first := rep.Certificates[0]; first.Name != "api.example.com" || first.Severity != "warning" || strings.Join(first.Nodes, ",") != "gw-1" {
		t.Fatalf("soonest certificate = %+v", first)
	}
	var nodeIDs []string
	for _, n := range rep.Nodes {
		nodeIDs = append(nodeIDs, n.NodeID)
	}
	if strings.Join(nodeIDs, ",") != "gw-1,cp-a" {
		t.Fatalf("nodes = %v", nodeIDs)
	}

	var sig struct {
		Signals []Signal `json:"signals"`
	}
	if code := get("/api/orbit/signals", &sig); code != 200 {
		t.Fatalf("signals status %d", code)
	}
	found := false
	for _, x := range sig.Signals {
		if x.ID == "certificates:warning" && x.Kind == "certificates" && strings.Contains(x.Detail, "api.example.com") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no certificate signal in %+v", sig.Signals)
	}

	// Alerts: one webhook post per certificate and severity per day, however
	// many control planes check.
	var mu sync.Mutex
	var posts []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, string(b))
		mu.Unlock()
	}))
	t.Cleanup(hook.Close)
	b := newControlPlane(t, s, "cp-b")
	a.alerts.WebhookURL, b.alerts.WebhookURL = hook.URL, hook.URL
	a.checkCertificateExpiry(ctx)
	b.checkCertificateExpiry(ctx)
	a.checkCertificateExpiry(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(posts)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // a duplicate would arrive by now
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 1 || !strings.Contains(posts[0], "certificate_expiring") || !strings.Contains(posts[0], "api.example.com") ||
		!strings.Contains(posts[0], "www.example.com") || strings.Contains(posts[0], "gw-1, gw-1") {
		t.Fatalf("webhook posts = %v", posts)
	}
	if strings.Contains(posts[0], "PRIVATE KEY") {
		t.Fatal("alert contains key material")
	}
}

func TestIntegrationCertificateReportIsPlatformOnly(t *testing.T) {
	if !platformOnly("/api/system/certificates") {
		t.Fatal("certificate report must be platform-only: it spans every tenant's upstreams")
	}
}

func TestIntegrationACMECacheInDatabaseIsEncrypted(t *testing.T) {
	s := openAdminTestStore(t)
	ctx := context.Background()
	c, err := certs.NewEncryptedCache(s.ACMECache(), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("-----BEGIN EC PRIVATE KEY-----\nabc\n-----END EC PRIVATE KEY-----\n")
	if err := c.Put(ctx, "api.example.com", key); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := s.Pool.QueryRow(ctx, `SELECT data FROM acme_cache WHERE cache_key = 'api.example.com'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("PRIVATE")) {
		t.Fatal("ACME cache stored in clear text")
	}
	got, err := c.Get(ctx, "api.example.com")
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("round trip: %v", err)
	}
	if err := c.Put(ctx, "api.example.com", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get(ctx, "api.example.com"); string(got) != "v2" {
		t.Fatal("overwrite lost")
	}
	if ok, err := s.TryACMEOrderLease(ctx, "api.example.com", "gw-1", time.Minute); !ok || err != nil {
		t.Fatalf("first lease: %v %v", ok, err)
	}
	if ok, _ := s.TryACMEOrderLease(ctx, "api.example.com", "gw-2", time.Minute); ok {
		t.Fatal("a second node took a held lease")
	}
	if ok, _ := s.TryACMEOrderLease(ctx, "api.example.com", "gw-1", time.Minute); !ok {
		t.Fatal("the holder could not renew its lease")
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE acme_order_leases SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.TryACMEOrderLease(ctx, "api.example.com", "gw-2", time.Minute); !ok {
		t.Fatal("an expired lease was not taken over")
	}
	if ok, err := s.ClaimCertificateAlert(ctx, "fp", "warning"); !ok || err != nil {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if ok, _ := s.ClaimCertificateAlert(ctx, "fp", "warning"); ok {
		t.Fatal("second claim within a day succeeded")
	}
	if ok, _ := s.ClaimCertificateAlert(ctx, "fp", "critical"); !ok {
		t.Fatal("a new severity must alert again")
	}
	if err := s.RecordNodeCertificates(ctx, "gw-x", nil); err != nil {
		t.Fatal(err)
	}
	if nodes, _ := s.ListNodeCertificates(ctx, store.NodeLivenessWindow); len(nodes) != 0 {
		t.Fatal("a node that has not acknowledged recently must not be listed")
	}
	if err := s.AcknowledgeRevision(ctx, "gw-x", 1, 0, 0, 1); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.ListNodeCertificates(ctx, store.NodeLivenessWindow)
	if err != nil || len(nodes) != 1 || nodes[0].Certificates == nil {
		t.Fatalf("nodes = %+v err = %v", nodes, err)
	}
}
