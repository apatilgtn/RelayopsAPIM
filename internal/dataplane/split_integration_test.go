package dataplane_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/relayops/apim/internal/admin"
	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/dataplane"
	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/testdb"
)

const nodeToken = "node-token-0123456789-0123456789-abcdef"

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", within, what)
}

func backend(t *testing.T, name string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, name) }))
	t.Cleanup(srv.Close)
	return srv
}

type edgeNode struct {
	gw  *gateway.Gateway
	src *dataplane.RemoteSource
}

func (n *edgeNode) get(path string) (int, string) {
	rec := httptest.NewRecorder()
	n.gw.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code, rec.Body.String()
}

// startEdge starts a gateway-only node exactly as runGateway wires one: no
// store, everything through the control plane's node API.
func startEdge(t *testing.T, ctx context.Context, cpURL, id string) (*edgeNode, error) {
	return startEdgeWith(t, ctx, cpURL, id, nodeToken, nil)
}

// startEdgeWith starts a gateway-only node with a given token, requiring
// signed configuration when verifier is set.
func startEdgeWith(t *testing.T, ctx context.Context, cpURL, id, token string, verifier *dataplane.Verifier) (*edgeNode, error) {
	t.Helper()
	client, err := dataplane.NewClient(dataplane.ClientConfig{BaseURL: cpURL, Token: token, NodeID: id, NodeGroup: "default", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	hub := realtime.NewHub()
	collector := analytics.NewCollectorWithWriter(dataplane.NewLogWriter(client), hub, id)
	if err := collector.EnableSpool("data/request_log_spool."+id+".jsonl", 1<<20); err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(collector, nil, id)
	gw.SetAIBudgetLedger(dataplane.NewLedger(client))
	src := dataplane.NewRemoteSource(client)
	if verifier != nil {
		src.RequireSignatures(verifier)
	}
	w := gateway.NewWatcherWithSource(gw, src, hub, time.Minute)
	if err := w.Reload(ctx, "startup"); err != nil {
		return nil, err
	}
	ticks := dataplane.NewTickForwarder(client)
	collector.OnTick(ticks.Forward)
	go ticks.Run(ctx)
	go collector.Run(ctx)
	go w.Run(ctx)
	return &edgeNode{gw: gw, src: src}, nil
}

func TestIntegrationGatewayOnlyNodeWithoutDatabase(t *testing.T) {
	dsn := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v1, v2 := backend(t, "v1"), backend(t, "v2")

	wd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil { // caches and spools go to ./data
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

	// Control plane, reachable over HTTP; `down` simulates an outage.
	cpHub := realtime.NewHub()
	console := cpHub.Subscribe() // what the console's live stream receives
	defer cpHub.Unsubscribe(console)
	cp := admin.New(db, nil, cpHub, "admin-token", "cp-1", fstest.MapFS{}, admin.WithDataplaneToken(nodeToken))
	go cp.StartDataplaneNotifier(ctx)
	cpHandler := cp.Handler()
	var down atomic.Bool
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		cpHandler.ServeHTTP(w, r)
	}))
	// Stop the notifier and long-polls before the pool closes: pgxpool.Close
	// waits for the notifier's held LISTEN connection.
	defer func() {
		cancel()
		cpSrv.CloseClientConnections()
		cpSrv.Close()
	}()

	edge, err := startEdge(t, ctx, cpSrv.URL, "edge-1")
	if err != nil {
		t.Fatal(err)
	}
	if code, body := edge.get("/svc"); code != 200 || body != "v1" {
		t.Fatalf("initial: %d %q", code, body)
	}

	// With nothing changing, the long-poll must hold rather than return the
	// same configuration again (which would reload the gateway in a loop).
	idleStart := edge.src.Updates()
	time.Sleep(1500 * time.Millisecond)
	if n := edge.src.Updates() - idleStart; n > 1 {
		t.Fatalf("idle gateway received %d configuration updates in 1.5s; the long-poll is not holding", n)
	}

	// A published change reaches the gateway through the long-poll, not the resync.
	api.UpstreamURL = v2.URL
	start := time.Now()
	if _, _, err := db.UpdateAPIAtomic(ctx, api, "t", "v2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "gateway-only node applies v2", func() bool { _, b := edge.get("/svc"); return b == "v2" })
	t.Logf("change propagated to the gateway-only node in %v", time.Since(start))

	// The node appears in fleet status through its acknowledgement.
	eventually(t, 5*time.Second, "edge-1 in fleet status", func() bool {
		fs, err := db.GetFleetStatus(ctx)
		if err != nil {
			return false
		}
		for _, n := range fs.Nodes {
			if n.NodeID == "edge-1" {
				return true
			}
		}
		return false
	})

	// Request logs reach request_logs through the control plane.
	logCount := func() (total, distinct int) {
		_ = db.Pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT log_id) FROM request_logs WHERE node_id = 'edge-1'`).Scan(&total, &distinct)
		return
	}
	for i := 0; i < 5; i++ {
		edge.get("/svc")
	}
	eventually(t, 5*time.Second, "logs persisted", func() bool { n, _ := logCount(); return n >= 5 })

	// The control plane serves no traffic, but its console shows the gateway's.
	deadline := time.After(5 * time.Second)
	for sawTraffic := false; !sawTraffic; {
		select {
		case msg := <-console:
			var tick struct {
				Node string `json:"node"`
				RPS  int    `json:"rps"`
			}
			if msg.Event == "tick" && json.Unmarshal(msg.Data, &tick) == nil && tick.Node == "edge-1" && tick.RPS > 0 {
				sawTraffic = true
			}
		case <-deadline:
			t.Fatal("no live-traffic tick from edge-1 reached the control plane's console")
		}
	}
	before, _ := logCount()

	// Control-plane outage: the node keeps serving and spools its logs.
	down.Store(true)
	for i := 0; i < 10; i++ {
		if code, body := edge.get("/svc"); code != 200 || body != "v2" {
			t.Fatalf("during outage: %d %q (must keep serving last-known-good)", code, body)
		}
	}
	time.Sleep(1500 * time.Millisecond) // at least one failed flush

	// A node restarting during the outage starts from its own cache.
	cold, err := startEdge(t, ctx, cpSrv.URL, "edge-1")
	if err != nil {
		t.Fatalf("cold start during outage: %v", err)
	}
	if code, body := cold.get("/svc"); code != 200 || body != "v2" {
		t.Fatalf("cold start from cache: %d %q", code, body)
	}

	// Recovery: spooled logs are replayed exactly once per log_id.
	down.Store(false)
	eventually(t, 15*time.Second, "spooled logs replayed", func() bool { n, _ := logCount(); return n >= before+10 })
	total, distinct := logCount()
	if total != distinct {
		t.Fatalf("duplicate request logs after replay: %d rows, %d distinct", total, distinct)
	}
}
