// Package tracing implements W3C Trace Context propagation and an OpenTelemetry
// OTLP/HTTP (JSON encoding) span exporter with no third-party dependencies.
//
// Configuration follows the standard OpenTelemetry environment variables:
//
//	OTEL_EXPORTER_OTLP_TRACES_ENDPOINT  full URL, e.g. http://otel-collector:4318/v1/traces
//	OTEL_EXPORTER_OTLP_ENDPOINT         base URL; "/v1/traces" is appended
//	OTEL_EXPORTER_OTLP_HEADERS          comma-separated key=value pairs (e.g. auth headers)
//	OTEL_SERVICE_NAME                   service.name resource attribute (default relayops-gateway)
//	OTEL_TRACES_SAMPLER_ARG             root sampling ratio 0.0-1.0 (default 1.0); parent-based
//
// When no endpoint is configured the tracer is disabled: incoming traceparent
// headers are still forwarded unchanged and their trace IDs recorded in logs.
package tracing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SpanKind values from the OTLP protobuf enum.
const (
	KindInternal = 1
	KindServer   = 2
	KindClient   = 3
)

// Status codes from the OTLP protobuf enum.
const (
	StatusUnset = 0
	StatusOK    = 1
	StatusError = 2
)

// SpanContext identifies a span within a trace.
type SpanContext struct {
	TraceID [16]byte
	SpanID  [8]byte
	Sampled bool
}

func (sc SpanContext) IsValid() bool {
	return sc.TraceID != [16]byte{} && sc.SpanID != [8]byte{}
}

func (sc SpanContext) TraceIDHex() string { return hex.EncodeToString(sc.TraceID[:]) }
func (sc SpanContext) SpanIDHex() string  { return hex.EncodeToString(sc.SpanID[:]) }

// Traceparent renders the W3C traceparent header value.
func (sc SpanContext) Traceparent() string {
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return "00-" + sc.TraceIDHex() + "-" + sc.SpanIDHex() + "-" + flags
}

// ParseTraceparent parses a W3C traceparent header (version 00 semantics; future
// versions are accepted when their first four fields are well formed).
func ParseTraceparent(h string) (SpanContext, bool) {
	var sc SpanContext
	h = strings.TrimSpace(h)
	parts := strings.Split(h, "-")
	if len(parts) < 4 {
		return sc, false
	}
	ver, tid, sid, flags := parts[0], parts[1], parts[2], parts[3]
	if len(ver) != 2 || ver == "ff" || len(tid) != 32 || len(sid) != 16 || len(flags) != 2 {
		return sc, false
	}
	if ver == "00" && len(parts) != 4 {
		return sc, false
	}
	if _, err := hex.Decode(sc.TraceID[:], []byte(tid)); err != nil {
		return sc, false
	}
	if _, err := hex.Decode(sc.SpanID[:], []byte(sid)); err != nil {
		return sc, false
	}
	f, err := hex.DecodeString(flags)
	if err != nil {
		return sc, false
	}
	sc.Sampled = f[0]&0x01 == 1
	if !sc.IsValid() {
		return SpanContext{}, false
	}
	return sc, true
}

func newTraceID() (id [16]byte) {
	_, _ = rand.Read(id[:])
	return
}

func newSpanID() (id [8]byte) {
	_, _ = rand.Read(id[:])
	return
}

// Span is a single timed operation.
type Span struct {
	tracer    *Tracer
	Name      string
	Kind      int
	Context   SpanContext
	Parent    [8]byte
	HasParent bool
	Start     time.Time
	EndTime   time.Time
	Status    int
	StatusMsg string
	mu        sync.Mutex
	attrs     map[string]any
	ended     atomic.Bool
}

// SetAttr records an attribute (string, bool, int, int64 or float64).
func (s *Span) SetAttr(k string, v any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.attrs == nil {
		s.attrs = map[string]any{}
	}
	s.attrs[k] = v
	s.mu.Unlock()
}

// SetStatus sets the span status.
func (s *Span) SetStatus(code int, msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Status, s.StatusMsg = code, msg
	s.mu.Unlock()
}

// End finishes the span and queues it for export when sampled.
func (s *Span) End() {
	if s == nil || !s.ended.CompareAndSwap(false, true) {
		return
	}
	s.EndTime = time.Now()
	if s.tracer != nil && s.tracer.exporter != nil && s.Context.Sampled {
		s.tracer.exporter.enqueue(s)
	}
}

// Tracer creates spans.
type Tracer struct {
	exporter    *exporter
	sampleRatio float64
	service     string
}

// Enabled reports whether spans are exported.
func (t *Tracer) Enabled() bool { return t != nil && t.exporter != nil }

// NewFromEnv builds a tracer from OTEL_* environment variables.
func NewFromEnv(defaultService, nodeID string) *Tracer {
	t := &Tracer{sampleRatio: 1.0, service: defaultService}
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		t.service = v
	}
	if v := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			t.sampleRatio = f
		}
	}
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if endpoint == "" {
		if base := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); base != "" {
			endpoint = strings.TrimRight(base, "/") + "/v1/traces"
		}
	}
	if endpoint == "" {
		return t
	}
	t.exporter = newExporter(endpoint, parseHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")), t.service, nodeID)
	slog.Info("opentelemetry tracing enabled", "endpoint", endpoint, "service", t.service, "sample_ratio", t.sampleRatio)
	return t
}

// NewWithEndpoint builds an exporting tracer (used by tests and embedding).
func NewWithEndpoint(endpoint, service string, ratio float64) *Tracer {
	return &Tracer{sampleRatio: ratio, service: service, exporter: newExporter(endpoint, nil, service, "")}
}

func parseHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.TrimSpace(k) != "" {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// StartServer starts a SERVER span continuing the trace in the incoming
// traceparent header, or a new root trace. When the tracer is disabled it
// returns nil and the caller should forward headers untouched.
func (t *Tracer) StartServer(h http.Header, name string) *Span {
	if !t.Enabled() {
		return nil
	}
	s := &Span{tracer: t, Name: name, Kind: KindServer, Start: time.Now()}
	if parent, ok := ParseTraceparent(h.Get("traceparent")); ok {
		s.Context = SpanContext{TraceID: parent.TraceID, SpanID: newSpanID(), Sampled: parent.Sampled}
		s.Parent, s.HasParent = parent.SpanID, true
	} else {
		s.Context = SpanContext{TraceID: newTraceID(), SpanID: newSpanID(), Sampled: t.sampleRoot()}
	}
	return s
}

// StartChild starts a child span (e.g. a CLIENT span per upstream attempt).
func (t *Tracer) StartChild(parent *Span, name string, kind int) *Span {
	if parent == nil || !t.Enabled() {
		return nil
	}
	return &Span{
		tracer: t, Name: name, Kind: kind, Start: time.Now(),
		Context: SpanContext{TraceID: parent.Context.TraceID, SpanID: newSpanID(), Sampled: parent.Context.Sampled},
		Parent:  parent.Context.SpanID, HasParent: true,
	}
}

func (t *Tracer) sampleRoot() bool {
	if t.sampleRatio >= 1 {
		return true
	}
	if t.sampleRatio <= 0 {
		return false
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return float64(binary.BigEndian.Uint64(b[:])>>11)/float64(1<<53) < t.sampleRatio
}

// Shutdown flushes queued spans.
func (t *Tracer) Shutdown(ctx context.Context) {
	if t.Enabled() {
		t.exporter.shutdown(ctx)
	}
}

// TraceIDFromHeader returns the trace ID of an incoming traceparent, or "".
func TraceIDFromHeader(h http.Header) string {
	if sc, ok := ParseTraceparent(h.Get("traceparent")); ok {
		return sc.TraceIDHex()
	}
	return ""
}

// ---------------------------------------------------------------------------
// OTLP/HTTP JSON exporter
// ---------------------------------------------------------------------------

type exporter struct {
	endpoint string
	headers  map[string]string
	service  string
	nodeID   string
	client   *http.Client
	queue    chan *Span
	flushReq chan chan struct{}
	done     chan struct{}
	dropped  atomic.Int64
	exported atomic.Int64
	failed   atomic.Int64
}

const (
	queueSize    = 8192
	maxBatch     = 512
	flushEvery   = 2 * time.Second
	exportTimout = 10 * time.Second
)

func newExporter(endpoint string, headers map[string]string, service, nodeID string) *exporter {
	e := &exporter{
		endpoint: endpoint, headers: headers, service: service, nodeID: nodeID,
		client:   &http.Client{Timeout: exportTimout},
		queue:    make(chan *Span, queueSize),
		flushReq: make(chan chan struct{}),
		done:     make(chan struct{}),
	}
	go e.loop()
	return e
}

func (e *exporter) enqueue(s *Span) {
	select {
	case e.queue <- s:
	default:
		e.dropped.Add(1) // never block the request path
	}
}

func (e *exporter) loop() {
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	var batch []*Span
	flush := func() {
		if len(batch) > 0 {
			e.export(batch)
			batch = nil
		}
	}
	for {
		select {
		case s := <-e.queue:
			batch = append(batch, s)
			if len(batch) >= maxBatch {
				flush()
			}
		case <-tick.C:
			flush()
		case ack := <-e.flushReq:
			for drained := false; !drained; {
				select {
				case s := <-e.queue:
					batch = append(batch, s)
				default:
					drained = true
				}
			}
			flush()
			close(ack)
		case <-e.done:
			return
		}
	}
}

func (e *exporter) shutdown(ctx context.Context) {
	ack := make(chan struct{})
	select {
	case e.flushReq <- ack:
		select {
		case <-ack:
		case <-ctx.Done():
		}
	case <-ctx.Done():
	}
	close(e.done)
}

type otlpKV struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func kv(k string, v any) otlpKV {
	switch x := v.(type) {
	case string:
		return otlpKV{k, map[string]any{"stringValue": x}}
	case bool:
		return otlpKV{k, map[string]any{"boolValue": x}}
	case int:
		return otlpKV{k, map[string]any{"intValue": strconv.Itoa(x)}}
	case int64:
		return otlpKV{k, map[string]any{"intValue": strconv.FormatInt(x, 10)}}
	case float64:
		return otlpKV{k, map[string]any{"doubleValue": x}}
	default:
		b, _ := json.Marshal(x)
		return otlpKV{k, map[string]any{"stringValue": string(b)}}
	}
}

// EncodeOTLP renders spans as an OTLP ExportTraceServiceRequest in JSON.
func EncodeOTLP(spans []*Span, service, nodeID string) ([]byte, error) {
	resAttrs := []otlpKV{kv("service.name", service), kv("telemetry.sdk.name", "relayops"), kv("telemetry.sdk.language", "go")}
	if nodeID != "" {
		resAttrs = append(resAttrs, kv("service.instance.id", nodeID))
	}
	out := make([]map[string]any, 0, len(spans))
	for _, s := range spans {
		s.mu.Lock()
		attrs := make([]otlpKV, 0, len(s.attrs))
		for k, v := range s.attrs {
			attrs = append(attrs, kv(k, v))
		}
		status := map[string]any{"code": s.Status}
		if s.StatusMsg != "" {
			status["message"] = s.StatusMsg
		}
		s.mu.Unlock()
		span := map[string]any{
			"traceId":           s.Context.TraceIDHex(),
			"spanId":            s.Context.SpanIDHex(),
			"name":              s.Name,
			"kind":              s.Kind,
			"startTimeUnixNano": strconv.FormatInt(s.Start.UnixNano(), 10),
			"endTimeUnixNano":   strconv.FormatInt(s.EndTime.UnixNano(), 10),
			"attributes":        attrs,
			"status":            status,
		}
		if s.HasParent {
			span["parentSpanId"] = hex.EncodeToString(s.Parent[:])
		}
		out = append(out, span)
	}
	return json.Marshal(map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": resAttrs},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "github.com/relayops/apim/gateway", "version": "1.0"},
				"spans": out,
			}},
		}},
	})
}

func (e *exporter) export(batch []*Span) {
	body, err := EncodeOTLP(batch, e.service, e.nodeID)
	if err != nil {
		e.failed.Add(int64(len(batch)))
		return
	}
	req, err := http.NewRequest(http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		e.failed.Add(int64(len(batch)))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.failed.Add(int64(len(batch)))
		slog.Warn("otlp export failed", "err", err, "spans", len(batch))
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		e.failed.Add(int64(len(batch)))
		slog.Warn("otlp export rejected", "status", resp.StatusCode, "spans", len(batch))
		return
	}
	e.exported.Add(int64(len(batch)))
}

// Stats reports exporter counters.
func (t *Tracer) Stats() (exported, dropped, failed int64) {
	if !t.Enabled() {
		return 0, 0, 0
	}
	return t.exporter.exported.Load(), t.exporter.dropped.Load(), t.exporter.failed.Load()
}
