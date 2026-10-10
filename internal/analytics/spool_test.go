package analytics

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

// flakyDB fails while down and records what it accepted, in order.
type flakyDB struct {
	mu       sync.Mutex
	down     bool
	failNext int // fail this many calls even when up
	got      []string
}

func (f *flakyDB) InsertLogs(_ context.Context, logs []store.RequestLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down || f.failNext > 0 {
		if f.failNext > 0 {
			f.failNext--
		}
		return errors.New("connection refused")
	}
	for _, l := range logs {
		f.got = append(f.got, l.RequestID)
	}
	return nil
}

func reqs(prefix string, n int) []store.RequestLog {
	out := make([]store.RequestLog, n)
	for i := range out {
		out[i] = store.RequestLog{RequestID: prefix + strconv.Itoa(i), Status: 200}
	}
	return out
}

func newSpooledCollector(t *testing.T, db *flakyDB, maxBytes int64) (*Collector, string) {
	t.Helper()
	c := NewCollector(nil, nil, "n1")
	c.writer = db
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	if err := c.EnableSpool(path, maxBytes); err != nil {
		t.Fatal(err)
	}
	return c, path
}

func TestOutageLogsAreSpooledAndReplayedInOrder(t *testing.T) {
	db := &flakyDB{down: true}
	c, _ := newSpooledCollector(t, db, 1<<20)
	ctx := context.Background()

	c.flush(ctx, reqs("a", 3)) // fails: spooled, backoff starts
	c.flush(ctx, reqs("b", 2)) // inside backoff: spooled without touching the DB
	if got := c.spool.records.Load(); got != 5 {
		t.Fatalf("spooled = %d, want 5", got)
	}
	if c.persistFailures.Load() != 1 {
		t.Fatalf("persist failures = %d, want 1 (backoff must avoid hammering a dead database)", c.persistFailures.Load())
	}

	db.mu.Lock()
	db.down = false
	db.mu.Unlock()
	c.retryAt.Store(0) // backoff elapsed
	c.flush(ctx, reqs("c", 2))

	want := "a0 a1 a2 b0 b1 c0 c1"
	if got := strings.Join(db.got, " "); got != want {
		t.Fatalf("delivered %q, want %q (backlog first, in order)", got, want)
	}
	if c.spool.records.Load() != 0 || c.spool.replayed.Load() != 5 {
		t.Fatalf("spool records=%d replayed=%d", c.spool.records.Load(), c.spool.replayed.Load())
	}
	var buf bytes.Buffer
	c.WritePrometheusMetrics(&buf)
	for _, want := range []string{"relayops_request_logs_spooled_total 5", "relayops_request_logs_replayed_total 5",
		"relayops_request_logs_spool_records 0", "relayops_request_logs_persist_failures_total 1",
		`relayops_request_logs_dropped_total{reason="spool_full"} 0`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestSpoolSurvivesRestart(t *testing.T) {
	db := &flakyDB{down: true}
	c, path := newSpooledCollector(t, db, 1<<20)
	c.flush(context.Background(), reqs("x", 4))

	// A new process opens the same spool file and replays it once the DB is up.
	db2 := &flakyDB{}
	c2 := NewCollector(nil, nil, "n1")
	c2.writer = db2
	if err := c2.EnableSpool(path, 1<<20); err != nil {
		t.Fatal(err)
	}
	if c2.spool.records.Load() != 4 {
		t.Fatalf("records after restart = %d, want 4", c2.spool.records.Load())
	}
	c2.flush(context.Background(), nil)
	if strings.Join(db2.got, " ") != "x0 x1 x2 x3" {
		t.Fatalf("replayed after restart: %v", db2.got)
	}
}

func TestNoDatabaseSpoolsForNextRun(t *testing.T) {
	c := NewCollector(nil, nil, "n1") // cached-recovery start: no store
	if err := c.EnableSpool(filepath.Join(t.TempDir(), "s.jsonl"), 1<<20); err != nil {
		t.Fatal(err)
	}
	c.flush(context.Background(), reqs("r", 3))
	if c.spool.records.Load() != 3 {
		t.Fatalf("records = %d, want 3", c.spool.records.Load())
	}
}

func TestFullSpoolCountsDrops(t *testing.T) {
	db := &flakyDB{down: true}
	c, _ := newSpooledCollector(t, db, 400) // room for only a few records
	c.flush(context.Background(), reqs("z", 50))
	kept, dropped := c.spool.records.Load(), c.spool.droppedFull.Load()
	if kept == 0 || dropped == 0 || kept+dropped != 50 || c.spool.size.Load() > 400 {
		t.Fatalf("kept=%d dropped=%d size=%d", kept, dropped, c.spool.size.Load())
	}
}

func TestPartialReplayKeepsRemainder(t *testing.T) {
	db := &flakyDB{down: true}
	c, _ := newSpooledCollector(t, db, 1<<20)
	c.flush(context.Background(), reqs("p", 7))

	db.mu.Lock()
	db.down = false
	db.mu.Unlock()
	// Replay in batches of 3: first batch lands, second fails.
	db.failNext = 0
	empty, err := c.spool.replay(context.Background(), &failAfter{inner: db, ok: 1}, 3)
	if empty || err == nil || c.spool.records.Load() != 4 {
		t.Fatalf("partial replay: empty=%v err=%v remaining=%d", empty, err, c.spool.records.Load())
	}
	if empty, err := c.spool.replay(context.Background(), db, 3); !empty || err != nil {
		t.Fatalf("final replay: empty=%v err=%v", empty, err)
	}
	if strings.Join(db.got, " ") != "p0 p1 p2 p3 p4 p5 p6" {
		t.Fatalf("delivered %v", db.got)
	}
}

type failAfter struct {
	inner *flakyDB
	ok    int
}

func (f *failAfter) InsertLogs(ctx context.Context, logs []store.RequestLog) error {
	if f.ok == 0 {
		return errors.New("lost connection mid-replay")
	}
	f.ok--
	return f.inner.InsertLogs(ctx, logs)
}

func TestWithoutSpoolLossIsCounted(t *testing.T) {
	c := NewCollector(nil, nil, "n1")
	c.writer = &flakyDB{down: true}
	c.flush(context.Background(), reqs("q", 2))
	if c.droppedNoSpool.Load() != 2 {
		t.Fatalf("dropped without spool = %d, want 2", c.droppedNoSpool.Load())
	}
}

func TestShutdownDrainsSpoolDespiteBackoff(t *testing.T) {
	db := &flakyDB{down: true}
	c, _ := newSpooledCollector(t, db, 1<<20)
	c.hub = realtime.NewHub()
	c.flush(context.Background(), reqs("s", 3)) // outage: spooled, backoff armed
	db.mu.Lock()
	db.down = false
	db.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	c.Record(store.RequestLog{RequestID: "last", Status: 200})
	cancel() // shutdown while still inside the backoff window
	<-done
	if got := strings.Join(db.got, " "); got != "s0 s1 s2 last" {
		t.Fatalf("delivered at shutdown: %q", got)
	}
	if c.spool.records.Load() != 0 {
		t.Fatalf("spool not drained: %d", c.spool.records.Load())
	}
}

func TestRecordAssignsUniqueLogIDs(t *testing.T) {
	c := NewCollector(nil, nil, "n")
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		c.Record(store.RequestLog{Status: 200})
		l := <-c.ch
		if len(l.LogID) != 36 || seen[l.LogID] {
			t.Fatalf("bad or duplicate log ID %q", l.LogID)
		}
		seen[l.LogID] = true
	}
}
