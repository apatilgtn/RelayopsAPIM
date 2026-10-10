package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/relayops/apim/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

// ---------------------------------------------------------------------------
// Harness: throwaway database and a TCP proxy that can partition one node
// ---------------------------------------------------------------------------

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	return testdb.New(t)
}

// partitionProxy forwards TCP to Postgres until Cut, and again after Restore.
type partitionProxy struct {
	target string
	addr   string
	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]bool
}

func newPartitionProxy(t *testing.T, target string) *partitionProxy {
	t.Helper()
	p := &partitionProxy{target: target, conns: map[net.Conn]bool{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.serve(ln)
	t.Cleanup(p.Cut)
	return p
}

func (p *partitionProxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", p.target)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			if p.ln != ln { // cut while this connection was being set up
				p.mu.Unlock()
				c.Close()
				up.Close()
				continue
			}
			p.conns[c], p.conns[up] = true, true
			p.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); up.Close() }()
			go func() { _, _ = io.Copy(c, up); c.Close() }()
		}
	}()
}

// Cut drops the listener and every open connection: the node loses the database.
func (p *partitionProxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	for c := range p.conns {
		c.Close()
	}
	p.conns = map[net.Conn]bool{}
}

func (p *partitionProxy) Restore(t *testing.T) {
	t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // the port may linger briefly after Cut
		if ln, err = net.Listen("tcp", p.addr); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	p.serve(ln)
}

type node struct {
	gw      *Gateway
	watcher *Watcher
	store   *store.Store
	cancel  context.CancelFunc
}

func startNode(t *testing.T, dsn, id string) *node {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	hub := realtime.NewHub()
	gw := New(analytics.NewCollector(nil, hub, id), nil, id)
	w := NewWatcher(gw, s, hub, time.Second)
	if err := w.Reload(ctx, "startup"); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	go w.Run(rctx)
	n := &node{gw: gw, watcher: w, store: s, cancel: cancel}
	t.Cleanup(func() { cancel(); s.Close() })
	return n
}

func (n *node) get(path string) (int, string) {
	rec := httptest.NewRecorder()
	n.gw.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code, rec.Body.String()
}

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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, name) }))
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestIntegrationPartitionedNodeKeepsServingAndCatchesUp(t *testing.T) {
	dsn := testDatabaseURL(t)
	ctx := context.Background()
	v1, v2 := backend(t, "v1"), backend(t, "v2")

	// Run in a temp dir: nodes persist their last-known-good config to ./data.
	wd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	direct, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	if err := direct.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	api, _, err := direct.CreateAPIAtomic(ctx, store.API{Name: "svc", BasePath: "/svc", UpstreamURL: v1.URL, StripPath: true,
		AuthType: "none", TimeoutMS: 2000, Enabled: true}, "t", "v1")
	if err != nil {
		t.Fatal(err)
	}

	u, _ := url.Parse(dsn)
	proxy := newPartitionProxy(t, u.Host)
	pu := *u
	pu.Host = proxy.addr
	q := pu.Query()
	q.Set("connect_timeout", "1")
	pu.RawQuery = q.Encode()

	a := startNode(t, pu.String(), "node-a") // reaches Postgres through the proxy
	b := startNode(t, dsn, "node-b")
	for _, n := range []*node{a, b} {
		if code, body := n.get("/svc"); code != 200 || body != "v1" {
			t.Fatalf("initial: %d %q", code, body)
		}
	}

	// Partition node A from the database, then publish v2.
	proxy.Cut()
	api.UpstreamURL = v2.URL
	rev2 := int64(0)
	if _, rev2, err = direct.UpdateAPIAtomic(ctx, api, "t", "v2"); err != nil {
		t.Fatal(err)
	}

	eventually(t, 5*time.Second, "node B applies v2", func() bool { _, body := b.get("/svc"); return body == "v2" })

	// Node A keeps serving its last configuration through the outage, including
	// across reload attempts (resync every second) that cannot reach Postgres.
	for i := 0; i < 15; i++ {
		if code, body := a.get("/svc"); code != 200 || body != "v1" {
			t.Fatalf("partitioned node during outage: %d %q (must keep serving last-known-good)", code, body)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// A node that restarts during the outage starts from its on-disk cache.
	hub := realtime.NewHub()
	cold := New(analytics.NewCollector(nil, hub, "node-a"), nil, "node-a") // node A restarting
	unreachable, _ := store.Open(ctx, pu.String())                         // fails: database unreachable
	if unreachable != nil {
		t.Fatal("expected the partitioned DSN to be unreachable")
	}
	// As in production, the restarted node keeps a lazily connecting store.
	lazy, err := store.OpenLazy(pu.String())
	if err != nil {
		t.Fatal(err)
	}
	defer lazy.Close()
	cw := NewWatcher(cold, lazy, hub, time.Second)
	if err := cw.Reload(ctx, "startup"); err != nil {
		t.Fatalf("cold start during outage: %v", err)
	}
	rec := httptest.NewRecorder()
	cold.ServeHTTP(rec, httptest.NewRequest("GET", "/svc", nil))
	if rec.Code != 200 || rec.Body.String() != "v1" {
		t.Fatalf("cold start from cache: %d %q", rec.Code, rec.Body)
	}
	coldCtx, coldCancel := context.WithCancel(ctx)
	defer coldCancel()
	go cw.Run(coldCtx)

	// Heal the partition: both the long-running node and the node that started
	// during the outage converge on v2 without a restart.
	proxy.Restore(t)
	eventually(t, 15*time.Second, "node A catches up to v2 after the partition heals", func() bool {
		_, body := a.get("/svc")
		return body == "v2"
	})
	eventually(t, 15*time.Second, "the node started during the outage catches up to v2", func() bool {
		rec := httptest.NewRecorder()
		cold.ServeHTTP(rec, httptest.NewRequest("GET", "/svc", nil))
		return rec.Body.String() == "v2"
	})
	eventually(t, 5*time.Second, "both nodes acknowledge the new revision", func() bool {
		f, err := direct.GetFleetStatus(ctx)
		if err != nil {
			return false
		}
		acked := 0
		for _, n := range f.Nodes {
			if (n.NodeID == "node-a" || n.NodeID == "node-b") && n.Revision == rev2 {
				acked++
			}
		}
		return acked == 2
	})
}

func TestIntegrationCanarySplitAcrossNodes(t *testing.T) {
	dsn := testDatabaseURL(t)
	ctx := context.Background()
	stable, canary := backend(t, "stable"), backend(t, "canary")
	wd, _ := os.Getwd()
	_ = os.Chdir(t.TempDir())
	t.Cleanup(func() { _ = os.Chdir(wd) })

	direct, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	if err := direct.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	api, _, err := direct.CreateAPIAtomic(ctx, store.API{Name: "svc", BasePath: "/svc", UpstreamURL: stable.URL, StripPath: true,
		AuthType: "none", TimeoutMS: 2000, Enabled: true}, "t", "stable")
	if err != nil {
		t.Fatal(err)
	}
	n1, n2 := startNode(t, dsn, "n1"), startNode(t, dsn, "n2")

	api.UpstreamURL = canary.URL
	crev, err := direct.AtomicPublishCanary(ctx, "t", "canary", store.CanarySplit{Header: "X-Canary"}, func(tx pgxTx) error {
		_, err := tx.Exec(ctx, `UPDATE apis SET upstream_url=$1 WHERE id=$2`, canary.URL, api.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*node{n1, n2} {
		n := n
		eventually(t, 5*time.Second, "node serves the header canary", func() bool {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/svc", nil)
			req.Header.Set("X-Canary", "1")
			n.gw.ServeHTTP(rec, req)
			return rec.Body.String() == "canary"
		})
		if _, body := n.get("/svc"); body != "stable" {
			t.Fatalf("requests without the header must stay stable, got %q", body)
		}
	}

	// Abort: every node drops the canary.
	if _, err := direct.AbortCanary(ctx, crev, "t"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*node{n1, n2} {
		n := n
		eventually(t, 5*time.Second, "node drops the aborted canary", func() bool {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/svc", nil)
			req.Header.Set("X-Canary", "1")
			n.gw.ServeHTTP(rec, req)
			return rec.Body.String() == "stable"
		})
	}
}

type pgxTx = pgx.Tx
