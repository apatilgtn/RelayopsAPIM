package dataplane_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/relayops/apim/internal/admin"
	"github.com/relayops/apim/internal/dataplane"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/testdb"
)

// Control plane -> regional relay -> gateway-only node, with signed
// configuration and per-node credentials.
func TestIntegrationRegionalRelay(t *testing.T) {
	dsn := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	v1, v2 := backend(t, "v1"), backend(t, "v2")
	wd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	api, _, err := db.CreateAPIAtomic(ctx, store.API{Name: "svc", BasePath: "/svc", UpstreamURL: v1.URL, StripPath: true,
		AuthType: "none", TimeoutMS: 2000, Enabled: true}, "t", "v1")
	if err != nil {
		t.Fatal(err)
	}
	seed, pub, _ := dataplane.GenerateSigningKey()
	signer, _ := dataplane.ParseSigningKey(seed)
	verifier, _ := dataplane.ParseVerifyKeys(pub)

	cp := admin.New(db, nil, realtime.NewHub(), "admin-token", "cp-1", fstest.MapFS{}, admin.WithDataplane(admin.DataplaneOptions{Signer: signer}))
	go cp.StartDataplaneNotifier(ctx)
	cpHandler := cp.Handler()
	var cpDown atomic.Bool
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cpDown.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		cpHandler.ServeHTTP(w, r)
	}))
	_, relayTok := issueNodeToken(t, cpSrv.URL, "relay-syd")
	_, edgeTok := issueNodeToken(t, cpSrv.URL, "edge-syd-1")

	relayClient, err := dataplane.NewClient(dataplane.ClientConfig{BaseURL: cpSrv.URL, Token: relayTok, NodeID: "relay-syd", NodeGroup: "default", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := dataplane.NewRelay(ctx, relayClient)
	if err != nil {
		t.Fatal(err)
	}
	relaySrv := httptest.NewServer(relay.Handler())
	defer func() {
		cancel()
		relaySrv.CloseClientConnections()
		relaySrv.Close()
		cpSrv.CloseClientConnections()
		cpSrv.Close()
	}()

	edge, err := startEdgeWith(t, ctx, relaySrv.URL, "edge-syd-1", edgeTok, verifier)
	if err != nil {
		t.Fatalf("gateway through the relay: %v", err)
	}
	if code, body := edge.get("/svc"); code != 200 || body != "v1" {
		t.Fatalf("initial: %d %q", code, body)
	}

	api.UpstreamURL = v2.URL
	if _, _, err := db.UpdateAPIAtomic(ctx, api, "t", "v2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "change propagated through the relay", func() bool { _, b := edge.get("/svc"); return b == "v2" })

	// Acknowledgements are forwarded with the gateway's credentials: the
	// gateway (not the relay) is in fleet status.
	eventually(t, 5*time.Second, "edge-syd-1 in fleet status", func() bool {
		fs, _ := db.GetFleetStatus(ctx)
		for _, n := range fs.Nodes {
			if n.NodeID == "relay-syd" {
				t.Fatal("the relay itself appeared in fleet status")
			}
			if n.NodeID == "edge-syd-1" {
				return true
			}
		}
		return false
	})
	edge.get("/svc")
	eventually(t, 5*time.Second, "logs through the relay", func() bool {
		var n int
		_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM request_logs WHERE node_id = 'edge-syd-1'`).Scan(&n)
		return n > 0
	})

	get := func(token, node string) int {
		req, _ := http.NewRequest("GET", relaySrv.URL+dataplane.PathConfig, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(dataplane.HeaderNodeID, node)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("rlnode_not-a-real-token", "edge-x"); code != http.StatusUnauthorized {
		t.Fatalf("unknown token at the relay: %d (want 401)", code)
	}
	if code := get(edgeTok, "edge-other"); code != http.StatusForbidden {
		t.Fatalf("credential used as another node at the relay: %d (want 403)", code)
	}

	// Control plane unreachable: a recently verified gateway keeps getting
	// configuration from the relay; an unknown one cannot be verified.
	relay.SetAuthTTL(time.Millisecond)
	cpDown.Store(true)
	time.Sleep(5 * time.Millisecond)
	if code := get(edgeTok, "edge-syd-1"); code != http.StatusOK {
		t.Fatalf("verified gateway during a control-plane outage: %d (want 200)", code)
	}
	if code := get(relayTok, "relay-syd"); code != http.StatusServiceUnavailable {
		t.Fatalf("never-verified credential during an outage: %d (want 503)", code)
	}
	cpDown.Store(false)
}
