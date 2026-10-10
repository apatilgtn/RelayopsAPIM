// Package analytics collects per-request events from the gateway, streams
// one-second rollups to the live dashboard and batch-writes logs to Postgres.
package analytics

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

const (
	bufferSize      = 50_000
	maxTailPerTick  = 40
	maxBatchPerCopy = 5_000
)

var LatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0}

type Collector struct {
	store  *store.Store
	writer logWriter // nil when no database is configured
	spool  *spool    // nil unless EnableSpool was called
	hub    *realtime.Hub
	nodeID string
	ch     chan store.RequestLog

	dropped           atomic.Int64 // in-memory buffer full
	droppedNoSpool    atomic.Int64 // persist failed and no spool is configured
	persistFailures   atomic.Int64
	retryAt           atomic.Int64 // unix nanos; skip Postgres until then after a failure
	totalReqs         atomic.Int64
	totalErrs         atomic.Int64
	rateLimitExceeded atomic.Int64
	startedAt         time.Time

	statusCounts             [600]atomic.Int64
	bucketCounts             [11]atomic.Int64
	durationSumMicro         atomic.Int64
	durationCount            atomic.Int64
	upstreamDurationSumMicro atomic.Int64
	upstreamDurationCount    atomic.Int64

	onTick     func(Tick)
	exporter   *LogExporter // nil: no OTLP log export
	sampleRate atomic.Uint64 // float64 bits; 0 means 1 (keep everything)
	sampledOut atomic.Int64  // successful requests not persisted because of sampling
}

func NewCollector(s *store.Store, hub *realtime.Hub, nodeID string) *Collector {
	c := &Collector{store: s, hub: hub, nodeID: nodeID, ch: make(chan store.RequestLog, bufferSize), startedAt: time.Now()}
	if s != nil {
		c.writer = s
	}
	return c
}

// NewCollectorWithWriter persists through w instead of a local database, as
// gateway-only nodes do through the control plane. Retention is the control
// plane's job, so RunRetention is a no-op on such a collector.
func NewCollectorWithWriter(w LogWriter, hub *realtime.Hub, nodeID string) *Collector {
	return &Collector{writer: w, hub: hub, nodeID: nodeID, ch: make(chan store.RequestLog, bufferSize), startedAt: time.Now()}
}

// EnableSpool keeps request logs that cannot be written to Postgres in a
// bounded file at path, replaying them when the database is reachable again.
func (c *Collector) EnableSpool(path string, maxBytes int64) error {
	sp, err := openSpool(path, maxBytes)
	if err != nil {
		return err
	}
	c.spool = sp
	if n := sp.records.Load(); n > 0 {
		slog.Info("request log spool has records awaiting replay", "path", path, "records", n)
	}
	return nil
}

const persistRetryDelay = 5 * time.Second

// Record is called on the hot path; it never blocks.
func (c *Collector) Record(l store.RequestLog) {
	l.NodeID = c.nodeID
	if l.LogID == "" {
		l.LogID = newLogID()
	}
	c.totalReqs.Add(1)
	if l.Status >= 100 && l.Status < 600 {
		c.statusCounts[l.Status].Add(1)
	}
	if l.Status >= 500 {
		c.totalErrs.Add(1)
	}
	if l.Status == 429 || l.RateLimitStatus == "rate_limited" {
		c.rateLimitExceeded.Add(1)
	}

	// Latency histogram in seconds
	durSec := l.LatencyMS / 1000.0
	c.durationCount.Add(1)
	c.durationSumMicro.Add(int64(l.LatencyMS * 1000.0))
	for i, le := range LatencyBuckets {
		if durSec <= le {
			c.bucketCounts[i].Add(1)
		}
	}

	if l.UpstreamDurationMS > 0 {
		c.upstreamDurationCount.Add(1)
		c.upstreamDurationSumMicro.Add(int64(l.UpstreamDurationMS * 1000.0))
	}

	select {
	case c.ch <- l:
	default:
		c.dropped.Add(1)
	}
}

// Tick is the payload pushed to dashboards every second.
type Tick struct {
	TS          time.Time          `json:"ts"`
	Node        string             `json:"node"`
	RPS         int                `json:"rps"`
	Errors      int                `json:"errors"`
	Status      map[string]int     `json:"status"`
	AvgLatency  float64            `json:"avg_latency_ms"`
	P95Latency  float64            `json:"p95_latency_ms"`
	BytesOut    int64              `json:"bytes_out"`
	ByAPI       map[string]int     `json:"by_api"`
	Recent      []store.RequestLog `json:"recent"`
	TotalReqs   int64              `json:"total_requests"`
	TotalErrors int64              `json:"total_errors"`
	Dropped     int64              `json:"dropped"`
	UptimeSec   int64              `json:"uptime_sec"`
}

func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var window []store.RequestLog
	for {
		select {
		case <-ctx.Done():
			// Shutting down (for example the pod is being replaced): ignore the retry
			// backoff and make one last attempt to deliver the spool and the buffer.
			c.retryAt.Store(0)
			fctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			c.flush(fctx, drain(c.ch, window))
			cancel()
			if c.spool != nil {
				if n := c.spool.records.Load(); n > 0 {
					slog.Warn("request logs still spooled at shutdown; they are delivered on the next start of this node", "records", n, "path", c.spool.path)
				}
			}
			return
		case l := <-c.ch:
			window = append(window, l)
		case now := <-ticker.C:
			fullTick := c.rollup(now, window)
			PublishTick(c.hub, fullTick)
			if c.onTick != nil {
				c.onTick(fullTick)
			}
			c.flush(ctx, window)
			window = window[:0:0]
		}
	}
}

// SetLogExporter sends every request log (before sampling) to an OTLP collector.
func (c *Collector) SetLogExporter(e *LogExporter) { c.exporter = e }

// LogExporter returns the configured exporter, or nil.
func (c *Collector) LogExporter() *LogExporter { return c.exporter }

// OnTick registers a callback that receives every one-second tick, for
// example to forward it from a gateway-only node to the control plane. It
// must not block.
func (c *Collector) OnTick(fn func(Tick)) { c.onTick = fn }

// PublishTick sends a tick to live-stream subscribers, each seeing only its
// tenant's traffic (or everything, for platform subscribers).
func PublishTick(hub *realtime.Hub, t Tick) {
	hub.PublishWithFilter("tick", func(clientTenant string) any {
		if clientTenant == "" {
			return t
		}
		return filterTickForTenant(t, clientTenant)
	})
}

func filterTickForTenant(t Tick, tenantID string) Tick {
	out := t
	out.Recent = make([]store.RequestLog, 0, len(t.Recent))
	out.ByAPI = make(map[string]int)
	out.Status = map[string]int{"2xx": 0, "3xx": 0, "4xx": 0, "5xx": 0}
	rps := 0
	errs := 0
	for _, l := range t.Recent {
		if store.TenantOrDefault(l.TenantID) == tenantID {
			out.Recent = append(out.Recent, l)
			rps++
			if l.Status >= 400 {
				errs++
			}
			cat := statusClass(l.Status)
			out.Status[cat]++
			if l.APIName != "" {
				out.ByAPI[l.APIName]++
			}
		}
	}
	out.RPS = rps
	out.Errors = errs
	return out
}

func drain(ch chan store.RequestLog, buf []store.RequestLog) []store.RequestLog {
	for {
		select {
		case l := <-ch:
			buf = append(buf, l)
		default:
			return buf
		}
	}
}

func (c *Collector) rollup(now time.Time, w []store.RequestLog) Tick {
	t := Tick{
		TS: now, Node: c.nodeID, RPS: len(w),
		Status: map[string]int{"2xx": 0, "3xx": 0, "4xx": 0, "5xx": 0},
		ByAPI:  map[string]int{}, Recent: []store.RequestLog{},
		TotalReqs: c.totalReqs.Load(), TotalErrors: c.totalErrs.Load(),
		Dropped: c.dropped.Load(), UptimeSec: int64(now.Sub(c.startedAt).Seconds()),
	}
	if len(w) == 0 {
		return t
	}
	lat := make([]float64, len(w))
	var sum float64
	for i, l := range w {
		lat[i] = l.LatencyMS
		sum += l.LatencyMS
		t.BytesOut += l.BytesOut
		switch {
		case l.Status >= 500:
			t.Status["5xx"]++
			t.Errors++
		case l.Status >= 400:
			t.Status["4xx"]++
		case l.Status >= 300:
			t.Status["3xx"]++
		default:
			t.Status["2xx"]++
		}
		name := l.APIName
		if name == "" {
			name = "(unmatched)"
		}
		t.ByAPI[name]++
	}
	sort.Float64s(lat)
	t.AvgLatency = sum / float64(len(w))
	t.P95Latency = lat[int(float64(len(lat)-1)*0.95)]
	start := len(w) - maxTailPerTick
	if start < 0 {
		start = 0
	}
	t.Recent = append(t.Recent, w[start:]...)
	return t
}

// SetSampleRate persists only a fraction (0 < rate <= 1) of successful
// requests. Errors (status >= 400, or an error recorded) are always kept.
// Each kept success carries SampleWeight 1/rate so aggregates stay unbiased.
// Live dashboards and Prometheus metrics always see every request.
func (c *Collector) SetSampleRate(rate float64) {
	if rate <= 0 || rate > 1 {
		rate = 1
	}
	c.sampleRate.Store(math.Float64bits(rate))
}

func (c *Collector) rate() float64 {
	if bits := c.sampleRate.Load(); bits != 0 {
		return math.Float64frombits(bits)
	}
	return 1
}

// sample drops successful requests not selected for persistence. Selection
// hashes the log ID, so a request is kept or dropped the same way everywhere.
func (c *Collector) sample(batch []store.RequestLog) []store.RequestLog {
	rate := c.rate()
	if rate >= 1 {
		return batch
	}
	threshold := uint64(rate * float64(math.MaxUint32))
	kept := batch[:0]
	for _, l := range batch {
		if l.Status >= 400 || l.Status == 0 || l.Error != "" {
			kept = append(kept, l)
			continue
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(l.LogID))
		if uint64(h.Sum32()) <= threshold {
			l.SampleWeight = 1 / rate
			kept = append(kept, l)
			continue
		}
		c.sampledOut.Add(1)
	}
	return kept
}

// flush persists a batch. Spooled backlog is replayed first so logs reach
// Postgres in order; while the database is failing, batches go to the spool.
func (c *Collector) flush(ctx context.Context, batch []store.RequestLog) {
	if c.exporter != nil {
		c.exporter.Enqueue(batch) // every request, before sampling
	}
	batch = c.sample(batch)
	if c.writer == nil {
		c.keep(batch) // no database at all (cached-recovery start): spool for the next run
		return
	}
	waiting := time.Now().UnixNano() < c.retryAt.Load()
	if c.spool != nil && c.spool.records.Load() > 0 {
		if waiting {
			c.keep(batch)
			return
		}
		empty, err := c.spool.replay(ctx, c.writer, maxBatchPerCopy)
		if !empty {
			if err != nil {
				c.persistFailed(err, 0)
			}
			c.keep(batch)
			return
		}
		slog.Info("request log spool replayed into Postgres", "replayed_total", c.spool.replayed.Load())
	} else if waiting && c.spool != nil {
		c.keep(batch)
		return
	}
	for len(batch) > 0 {
		n := len(batch)
		if n > maxBatchPerCopy {
			n = maxBatchPerCopy
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.writer.InsertLogs(cctx, batch[:n])
		cancel()
		if err != nil {
			c.persistFailed(err, n)
			c.keep(batch)
			return
		}
		batch = batch[n:]
	}
}

func (c *Collector) persistFailed(err error, n int) {
	c.persistFailures.Add(1)
	c.retryAt.Store(time.Now().Add(persistRetryDelay).UnixNano())
	slog.Error("persist request logs", "err", err, "count", n, "spool", c.spool != nil)
}

// keep spools logs that could not be persisted, or counts them as lost.
func (c *Collector) keep(batch []store.RequestLog) {
	if len(batch) == 0 {
		return
	}
	if c.spool == nil {
		c.droppedNoSpool.Add(int64(len(batch)))
		return
	}
	c.spool.append(batch)
}

// RunRetention periodically deletes logs older than the retention window.
func (c *Collector) RunRetention(ctx context.Context, retention time.Duration) {
	if c.store == nil || retention <= 0 {
		return
	}
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if n, err := c.store.PurgeLogs(ctx, retention); err != nil {
			slog.Warn("log retention", "err", err)
		} else if n > 0 {
			slog.Info("purged old request logs", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Collector) WritePrometheusMetrics(w io.Writer) {
	fmt.Fprintf(w, "# HELP relayops_requests_total Total cumulative HTTP requests handled by gateway\n")
	fmt.Fprintf(w, "# TYPE relayops_requests_total counter\n")
	hasStatus := false
	for status := 100; status < 600; status++ {
		count := c.statusCounts[status].Load()
		if count > 0 {
			hasStatus = true
			fmt.Fprintf(w, "relayops_requests_total{status=\"%d\"} %d\n", status, count)
		}
	}
	if !hasStatus {
		fmt.Fprintf(w, "relayops_requests_total %d\n", c.totalReqs.Load())
	}

	fmt.Fprintf(w, "\n# HELP relayops_request_duration_seconds Latency histogram in seconds\n")
	fmt.Fprintf(w, "# TYPE relayops_request_duration_seconds histogram\n")
	for i, le := range LatencyBuckets {
		fmt.Fprintf(w, "relayops_request_duration_seconds_bucket{le=\"%.3f\"} %d\n", le, c.bucketCounts[i].Load())
	}
	totalCount := c.durationCount.Load()
	fmt.Fprintf(w, "relayops_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", totalCount)
	fmt.Fprintf(w, "relayops_request_duration_seconds_sum %.6f\n", float64(c.durationSumMicro.Load())/1000000.0)
	fmt.Fprintf(w, "relayops_request_duration_seconds_count %d\n", totalCount)

	fmt.Fprintf(w, "\n# HELP relayops_rate_limit_exceeded_total Total HTTP requests rejected due to rate limits\n")
	fmt.Fprintf(w, "# TYPE relayops_rate_limit_exceeded_total counter\n")
	fmt.Fprintf(w, "relayops_rate_limit_exceeded_total %d\n", c.rateLimitExceeded.Load())

	fmt.Fprintf(w, "\n# HELP relayops_request_logs_dropped_total Request logs lost before reaching Postgres\n")
	fmt.Fprintf(w, "# TYPE relayops_request_logs_dropped_total counter\n")
	var spoolFull, spoolRecords, spooled, replayed int64
	if c.spool != nil {
		spoolFull, spoolRecords = c.spool.droppedFull.Load(), c.spool.records.Load()
		spooled, replayed = c.spool.spooled.Load(), c.spool.replayed.Load()
	}
	fmt.Fprintf(w, "relayops_request_logs_dropped_total{reason=\"buffer_full\"} %d\n", c.dropped.Load())
	fmt.Fprintf(w, "relayops_request_logs_dropped_total{reason=\"spool_full\"} %d\n", spoolFull)
	fmt.Fprintf(w, "relayops_request_logs_dropped_total{reason=\"no_spool\"} %d\n", c.droppedNoSpool.Load())
	fmt.Fprintf(w, "\n# HELP relayops_request_logs_persist_failures_total Failed attempts to write request logs to Postgres\n")
	fmt.Fprintf(w, "# TYPE relayops_request_logs_persist_failures_total counter\n")
	fmt.Fprintf(w, "relayops_request_logs_persist_failures_total %d\n", c.persistFailures.Load())
	fmt.Fprintf(w, "\n# HELP relayops_request_logs_spool_records Request logs waiting in the on-disk spool\n")
	fmt.Fprintf(w, "# TYPE relayops_request_logs_spool_records gauge\n")
	fmt.Fprintf(w, "relayops_request_logs_spool_records %d\n", spoolRecords)
	fmt.Fprintf(w, "\n# HELP relayops_request_logs_spooled_total Request logs written to the spool during database outages\n")
	fmt.Fprintf(w, "# TYPE relayops_request_logs_spooled_total counter\n")
	fmt.Fprintf(w, "relayops_request_logs_spooled_total %d\n", spooled)
	fmt.Fprintf(w, "\n# HELP relayops_request_logs_replayed_total Spooled request logs replayed into Postgres\n")
	fmt.Fprintf(w, "# TYPE relayops_request_logs_replayed_total counter\n")
	fmt.Fprintf(w, "relayops_request_logs_replayed_total %d\n\n", replayed)

	fmt.Fprintf(w, "# HELP relayops_request_logs_sampled_out_total Successful requests not persisted because of RELAYOPS_LOG_SAMPLE_RATE\n")
	fmt.Fprintf(w, "# TYPE relayops_request_logs_sampled_out_total counter\n")
	fmt.Fprintf(w, "relayops_request_logs_sampled_out_total %d\n\n", c.sampledOut.Load())
	if c.exporter != nil {
		c.exporter.WriteMetrics(w)
	}

	if c.upstreamDurationCount.Load() > 0 {
		fmt.Fprintf(w, "\n# HELP relayops_upstream_duration_seconds Latency of calls to upstream servers in seconds\n")
		fmt.Fprintf(w, "# TYPE relayops_upstream_duration_seconds summary\n")
		fmt.Fprintf(w, "relayops_upstream_duration_seconds_sum %.6f\n", float64(c.upstreamDurationSumMicro.Load())/1000000.0)
		fmt.Fprintf(w, "relayops_upstream_duration_seconds_count %d\n", c.upstreamDurationCount.Load())
	}
}

// newLogID returns a random RFC 4122 version 4 UUID identifying one log event.
// It runs on every request, so it uses the runtime's per-thread ChaCha8
// generator rather than crypto/rand (a syscall per call); log IDs only need
// to be unique, not secret.
func newLogID() string {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[:8], rand.Uint64())
	binary.LittleEndian.PutUint64(b[8:], rand.Uint64())
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:])
	return string(out[:])
}

// statusClass returns "2xx", "4xx" and so on without formatting.
func statusClass(status int) string {
	switch status / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	case 5:
		return "5xx"
	}
	return strconv.Itoa(status/100) + "xx"
}
