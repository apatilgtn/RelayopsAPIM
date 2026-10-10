package admin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/testdb"
)

// countingProxy forwards TCP to Postgres and counts the bytes the database
// sends back: what hosted providers such as Supabase bill as egress.
type countingProxy struct {
	addr string
	down atomic.Int64
}

func newCountingProxy(t *testing.T, target string) *countingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &countingProxy{addr: ln.Addr().String()}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				s, err := net.Dial("tcp", target)
				if err != nil {
					c.Close()
					return
				}
				go func() { _, _ = io.Copy(s, c); s.Close() }()
				buf := make([]byte, 32<<10)
				for {
					n, err := s.Read(buf)
					if n > 0 {
						p.down.Add(int64(n))
						if _, werr := c.Write(buf[:n]); werr != nil {
							break
						}
					}
					if err != nil {
						break
					}
				}
				c.Close()
			}(c)
		}
	}()
	return p
}

// The auto-rollback supervisor runs every few seconds on every control-plane
// node. It once read the full configuration snapshot each time, which made an
// idle node pull ~2 GB/day from a hosted database. Keep each evaluation small.
func TestIntegrationAutoRollbackEvaluationEgressBudget(t *testing.T) {
	dsn := testdb.New(t)
	ctx := context.Background()
	direct, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	if err := direct.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// A configuration whose snapshot is large enough that reading it would blow the budget.
	for i := 0; i < 30; i++ {
		if _, _, err := direct.CreateAPIAtomic(ctx, store.API{Name: fmt.Sprintf("svc-%d", i), BasePath: fmt.Sprintf("/svc-%d", i),
			UpstreamURL: "http://upstream", AuthType: "none", TimeoutMS: 1000, Enabled: true,
			Description: strings.Repeat("d", 2000)}, "t", "seed"); err != nil {
			t.Fatal(err)
		}
	}
	var snapshotBytes int
	if err := direct.Pool.QueryRow(ctx, `SELECT octet_length(snapshot_data::text) FROM config_revisions ORDER BY revision DESC LIMIT 1`).Scan(&snapshotBytes); err != nil {
		t.Fatal(err)
	}
	if snapshotBytes < 50_000 {
		t.Fatalf("seed snapshot is only %d bytes; the budget check would not catch a snapshot read", snapshotBytes)
	}

	u, _ := url.Parse(dsn)
	proxy := newCountingProxy(t, u.Host)
	u.Host = proxy.addr
	proxied, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer proxied.Close()
	cp := newControlPlane(t, proxied, "cp-egress")

	_ = cp.evaluateAutoRollback(ctx) // warm up connections and prepared statements
	const evaluations = 20
	before := proxy.down.Load()
	for i := 0; i < evaluations; i++ {
		_ = cp.evaluateAutoRollback(ctx)
	}
	perEval := (proxy.down.Load() - before) / evaluations
	t.Logf("auto-rollback evaluation: %d bytes from the database per evaluation (snapshot is %d bytes)", perEval, snapshotBytes)
	const budget = 5_000
	if perEval > budget {
		t.Fatalf("auto-rollback evaluation pulls %d bytes per run (budget %d); at one run every 3s that is %.1f MB/day per node",
			perEval, budget, float64(perEval)*28800/1e6)
	}
}
