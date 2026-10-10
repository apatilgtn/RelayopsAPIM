package tracing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseTraceparent(t *testing.T) {
	valid := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sc, ok := ParseTraceparent(valid)
	if !ok || !sc.Sampled || sc.TraceIDHex() != "4bf92f3577b34da6a3ce929d0e0e4736" || sc.SpanIDHex() != "00f067aa0ba902b7" {
		t.Fatalf("parse %q = %+v %v", valid, sc, ok)
	}
	if sc.Traceparent() != valid {
		t.Fatalf("round trip = %q", sc.Traceparent())
	}
	if sc, ok := ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"); !ok || sc.Sampled {
		t.Fatal("unsampled flag not honoured")
	}
	if _, ok := ParseTraceparent("01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-future"); !ok {
		t.Fatal("future versions with extra fields must be accepted")
	}
	for _, bad := range []string{
		"", "garbage",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",   // zero trace id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",   // zero span id
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",   // forbidden version
		"00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01",    // short trace id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-x", // v00 with extra field
		"00-zzf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	} {
		if _, ok := ParseTraceparent(bad); ok {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestDisabledTracerIsNoOp(t *testing.T) {
	tr := &Tracer{}
	if tr.Enabled() || tr.StartServer(http.Header{}, "x") != nil || tr.StartChild(nil, "y", KindClient) != nil {
		t.Fatal("a tracer without an endpoint must not create spans")
	}
	var s *Span
	s.SetAttr("k", "v")
	s.SetStatus(StatusError, "x")
	s.End() // nil-safe
}

func TestSamplerRespectsParentDecision(t *testing.T) {
	tr := &Tracer{sampleRatio: 0, exporter: &exporter{queue: make(chan *Span, 4)}}
	h := http.Header{}
	h.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !tr.StartServer(h, "x").Context.Sampled {
		t.Fatal("a sampled parent must be continued even when the root ratio is 0")
	}
	if tr.StartServer(http.Header{}, "x").Context.Sampled {
		t.Fatal("root ratio 0 must not sample new traces")
	}
	tr.sampleRatio = 1
	root := tr.StartServer(http.Header{}, "x")
	child := tr.StartChild(root, "c", KindClient)
	if !root.Context.Sampled || child.Context.TraceID != root.Context.TraceID || child.Parent != root.Context.SpanID {
		t.Fatal("child must share the trace and point at its parent")
	}
}

func TestExporterPostsOTLPJSON(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		raw := new(strings.Builder)
		_ = json.NewDecoder(r.Body).Decode(&m)
		b, _ := json.Marshal(m)
		raw.Write(b)
		mu.Lock()
		bodies = append(bodies, raw.String())
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=abc, x-tenant = t1")
	t.Setenv("OTEL_SERVICE_NAME", "gw-test")
	tr := NewFromEnv("relayops-gateway", "node-1")
	if !tr.Enabled() {
		t.Fatal("endpoint configured but tracer disabled")
	}
	s := tr.StartServer(http.Header{}, "GET /x")
	s.SetAttr("http.response.status_code", 502)
	s.SetAttr("relayops.ok", true)
	s.SetStatus(StatusError, "bad gateway")
	s.End()
	s.End() // double End must not export twice

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tr.Shutdown(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("exports = %d, want 1", len(bodies))
	}
	b := bodies[0]
	for _, want := range []string{`"service.name"`, `"gw-test"`, `"service.instance.id"`, `"node-1"`, `"kind":2`,
		`"intValue":"502"`, `"boolValue":true`, `"code":2`, `"message":"bad gateway"`, `"startTimeUnixNano":"`} {
		if !strings.Contains(b, want) {
			t.Errorf("payload missing %s: %s", want, b)
		}
	}
	if strings.Count(b, `"spanId"`) != 1 {
		t.Fatalf("span exported more than once: %s", b)
	}
	if headers[0].Get("x-api-key") != "abc" || headers[0].Get("x-tenant") != "t1" || headers[0].Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", headers[0])
	}
	if exported, dropped, failed := tr.Stats(); exported != 1 || dropped != 0 || failed != 0 {
		t.Fatalf("stats = %d/%d/%d", exported, dropped, failed)
	}
}
