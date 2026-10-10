package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/tracing"
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

func newTestGateway(t *testing.T) *Gateway {
	t.Helper()
	c := analytics.NewCollector(nil, realtime.NewHub(), "test-node")
	return New(c, nil, "test-node")
}

func (g *Gateway) load(t *testing.T, d store.SnapshotData) {
	t.Helper()
	if d.Revision == 0 {
		d.Revision = 10
	}
	snap, errs := buildSnapshot(d, d.Revision, g.upstreams)
	if len(errs) > 0 {
		t.Fatalf("buildSnapshot errors: %v", errs)
	}
	g.swap(snap)
}

func testAPI(name, base, upstream string) store.API {
	return store.API{
		ID: "id-" + name, Name: name, BasePath: base, UpstreamURL: upstream, StripPath: true,
		AuthType: "none", TimeoutMS: 2000, Enabled: true, QuotaFailurePolicy: "fail_open",
	}
}

func do(g *Gateway, method, target string, headers map[string]string, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

type upstreamStub struct {
	srv   *httptest.Server
	hits  atomic.Int64
	mu    sync.Mutex
	last  *http.Request
	body  string
	codes []int // status per hit; the last entry repeats
}

func newUpstream(t *testing.T, codes ...int) *upstreamStub {
	t.Helper()
	u := &upstreamStub{codes: codes}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(u.hits.Add(1)) - 1
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.last = r.Clone(r.Context())
		u.body = string(b)
		u.mu.Unlock()
		code := http.StatusOK
		if len(u.codes) > 0 {
			if n >= len(u.codes) {
				n = len(u.codes) - 1
			}
			code = u.codes[n]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path, "query": r.URL.RawQuery, "server": u.srv.URL})
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstreamStub) lastRequest() *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

// ---------------------------------------------------------------------------
// Routing and request pipeline
// ---------------------------------------------------------------------------

func TestProxyRoutesStripsPathAndSetsHeaders(t *testing.T) {
	up := newUpstream(t)
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{
		testAPI("orders", "/orders", up.srv.URL+"/v1"),
		testAPI("root", "/", up.srv.URL),
	}})

	rec := do(g, http.MethodGet, "/orders/list?x=1", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	got := up.lastRequest()
	if got.URL.Path != "/v1/list" || got.URL.RawQuery != "x=1" {
		t.Fatalf("upstream got %s?%s, want /v1/list?x=1", got.URL.Path, got.URL.RawQuery)
	}
	if rec.Header().Get("X-Request-ID") == "" || got.Header.Get("X-Request-ID") != rec.Header().Get("X-Request-ID") {
		t.Fatalf("request ID not propagated")
	}
	if rec.Header().Get("X-RelayOps-Revision") != "rev_0000000010" {
		t.Fatalf("revision header = %q", rec.Header().Get("X-RelayOps-Revision"))
	}
}

func TestUnknownRouteIs404WhenNoCatchAll(t *testing.T) {
	up := newUpstream(t)
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{testAPI("orders", "/orders", up.srv.URL)}})
	rec := do(g, http.MethodGet, "/ordersX", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if up.hits.Load() != 0 {
		t.Fatal("upstream must not be called for unmatched routes")
	}
}

func TestGatewayNotReadyWithoutSnapshot(t *testing.T) {
	g := newTestGateway(t)
	if rec := do(g, http.MethodGet, "/x", nil, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestAPIKeyAuthSubscriptionAndCredentialStripping(t *testing.T) {
	up := newUpstream(t)
	g := newTestGateway(t)
	api := testAPI("secure", "/secure", up.srv.URL)
	api.AuthType = "api_key"
	g.load(t, store.SnapshotData{
		APIs: []store.API{api},
		Keys: []store.KeyRecord{
			{KeyHash: store.HashKey("rk_good"), KeyID: "k1", ConsumerID: "c1", ConsumerName: "acme", ConsumerStatus: "active"},
			{KeyHash: store.HashKey("rk_nosub"), KeyID: "k2", ConsumerID: "c2", ConsumerName: "beta", ConsumerStatus: "active"},
		},
		Subs: []store.SubRecord{{ConsumerID: "c1", APIID: api.ID, Status: "approved", Active: true}},
	})

	if rec := do(g, "GET", "/secure", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: %d", rec.Code)
	}
	if rec := do(g, "GET", "/secure", map[string]string{"X-API-Key": "rk_bad"}, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", rec.Code)
	}
	if rec := do(g, "GET", "/secure", map[string]string{"X-API-Key": "rk_nosub"}, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("unsubscribed: %d", rec.Code)
	}
	rec := do(g, "GET", "/secure/a?apikey=rk_good&keep=1", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("subscribed: %d %s", rec.Code, rec.Body)
	}
	got := up.lastRequest()
	if got.URL.Query().Has("apikey") || got.URL.Query().Get("keep") != "1" {
		t.Fatalf("apikey query must be stripped, others kept: %q", got.URL.RawQuery)
	}
	if got.Header.Get("X-Consumer-ID") != "c1" || got.Header.Get("X-Consumer-Name") != "acme" {
		t.Fatalf("consumer headers = %q/%q", got.Header.Get("X-Consumer-ID"), got.Header.Get("X-Consumer-Name"))
	}
	do(g, "GET", "/secure", map[string]string{"X-API-Key": "rk_good"}, "")
	if up.lastRequest().Header.Get("X-API-Key") != "" {
		t.Fatal("X-API-Key must never be forwarded upstream")
	}
}

func TestJWTSubjectForwardedAsConsumerName(t *testing.T) {
	up := newUpstream(t)
	g := newTestGateway(t)
	api := testAPI("billing", "/billing", up.srv.URL)
	api.AuthType, api.JWTSecret = "jwt", "0123456789abcdef0123"
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	tok := signHS256(t, api.JWTSecret, map[string]any{"sub": "user-42", "exp": time.Now().Add(time.Hour).Unix()})
	rec := do(g, "GET", "/billing", map[string]string{"Authorization": "Bearer " + tok}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if got := up.lastRequest().Header.Get("X-Consumer-Name"); got != "user-42" {
		t.Fatalf("X-Consumer-Name = %q, want user-42", got)
	}
}

func TestRevisionPlanLimitsAreEnforced(t *testing.T) {
	up := newUpstream(t)
	g := newTestGateway(t)
	api := testAPI("limited", "/limited", up.srv.URL)
	api.AuthType = "api_key"
	api.RateLimitPerMinute = 1000
	two := 2
	g.load(t, store.SnapshotData{
		APIs: []store.API{api},
		Keys: []store.KeyRecord{{KeyHash: store.HashKey("rk_a"), KeyID: "k", ConsumerID: "c", ConsumerName: "c", ConsumerStatus: "active"}},
		Subs: []store.SubRecord{{ConsumerID: "c", APIID: api.ID, Status: "approved", Active: true, PlanName: "tiny", RateLimitPerMinute: &two}},
	})
	h := map[string]string{"X-API-Key": "rk_a"}
	codes := []int{do(g, "GET", "/limited", h, "").Code, do(g, "GET", "/limited", h, "").Code, do(g, "GET", "/limited", h, "").Code}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("codes = %v, want [200 200 429] from the plan limit (not the API default)", codes)
	}
}

func TestCORSPreflightShortCircuits(t *testing.T) {
	up := newUpstream(t)
	g := newTestGateway(t)
	api := testAPI("web", "/web", up.srv.URL)
	api.CORSEnabled = true
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	rec := do(g, http.MethodOptions, "/web/x", map[string]string{"Origin": "https://app.example", "Access-Control-Request-Method": "POST"}, "")
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("preflight = %d, ACAO %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if up.hits.Load() != 0 {
		t.Fatal("preflight must not reach the upstream")
	}
}

func TestUpstreamTimeoutIs504AndDownIs502(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()
	g := newTestGateway(t)
	a := testAPI("slow", "/slow", slow.URL)
	a.TimeoutMS = 50
	b := testAPI("down", "/down", "http://127.0.0.1:1")
	g.load(t, store.SnapshotData{APIs: []store.API{a, b}})
	if rec := do(g, "GET", "/slow", nil, ""); rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("slow = %d, want 504", rec.Code)
	}
	if rec := do(g, "GET", "/down", nil, ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("down = %d, want 502", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Load balancing, retries and circuit breaking
// ---------------------------------------------------------------------------

func TestLoadBalancingAcrossTargets(t *testing.T) {
	a, b := newUpstream(t), newUpstream(t)
	g := newTestGateway(t)
	api := testAPI("lb", "/lb", a.srv.URL)
	api.TrafficPolicy = store.TrafficPolicy{Targets: []store.UpstreamTarget{{URL: a.srv.URL, Weight: 3}, {URL: b.srv.URL, Weight: 1}}}
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	for i := 0; i < 40; i++ {
		if rec := do(g, "GET", "/lb", nil, ""); rec.Code != 200 {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if a.hits.Load() != 30 || b.hits.Load() != 10 {
		t.Fatalf("weighted round robin split = %d/%d, want 30/10", a.hits.Load(), b.hits.Load())
	}
}

func TestRetryMovesToHealthyTarget(t *testing.T) {
	bad, good := newUpstream(t, 503), newUpstream(t, 200)
	g := newTestGateway(t)
	api := testAPI("retry", "/retry", bad.srv.URL)
	api.TrafficPolicy = store.TrafficPolicy{
		Targets: []store.UpstreamTarget{{URL: bad.srv.URL, Weight: 1}, {URL: good.srv.URL, Weight: 1}},
		Retries: &store.RetryPolicy{Attempts: 2, BackoffMS: 1},
	}
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	for i := 0; i < 6; i++ {
		if rec := do(g, "GET", "/retry/x", nil, ""); rec.Code != 200 {
			t.Fatalf("request %d: status %d, want 200 after retrying the other target", i, rec.Code)
		}
	}
	if bad.hits.Load() == 0 {
		t.Fatal("round robin should have tried the failing target")
	}
	if got := good.lastRequest().URL.Path; got != "/x" {
		t.Fatalf("retried request path = %q, want /x", got)
	}
}

func TestRetryReturnsLastFailureWhenExhausted(t *testing.T) {
	bad := newUpstream(t, 503)
	g := newTestGateway(t)
	api := testAPI("r", "/r", bad.srv.URL)
	api.TrafficPolicy = store.TrafficPolicy{Retries: &store.RetryPolicy{Attempts: 3, BackoffMS: 1}}
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	rec := do(g, "GET", "/r", nil, "")
	if rec.Code != 503 || bad.hits.Load() != 3 {
		t.Fatalf("status=%d hits=%d, want 503 after 3 attempts", rec.Code, bad.hits.Load())
	}
}

func TestNonIdempotentRequestsAreNotRetriedByDefault(t *testing.T) {
	bad := newUpstream(t, 502)
	g := newTestGateway(t)
	api := testAPI("p", "/p", bad.srv.URL)
	api.TrafficPolicy = store.TrafficPolicy{Retries: &store.RetryPolicy{Attempts: 3, BackoffMS: 1}}
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	do(g, "POST", "/p", nil, `{"charge":1}`)
	if bad.hits.Load() != 1 {
		t.Fatalf("POST hits = %d, want 1 (no retry)", bad.hits.Load())
	}
}

func TestRetryReplaysRequestBodyWhenAllowed(t *testing.T) {
	up := newUpstream(t, 503, 200)
	g := newTestGateway(t)
	api := testAPI("p", "/p", up.srv.URL)
	api.TrafficPolicy = store.TrafficPolicy{Retries: &store.RetryPolicy{Attempts: 2, BackoffMS: 1, RetryNonIdempotent: true}}
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	rec := do(g, "POST", "/p", map[string]string{"Content-Type": "application/json"}, `{"order":7}`)
	if rec.Code != 200 || up.hits.Load() != 2 {
		t.Fatalf("status=%d hits=%d", rec.Code, up.hits.Load())
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.body != `{"order":7}` {
		t.Fatalf("replayed body = %q", up.body)
	}
}

func TestCircuitBreakerOpensAndShortCircuits(t *testing.T) {
	bad := newUpstream(t, 503)
	g := newTestGateway(t)
	api := testAPI("cb", "/cb", bad.srv.URL)
	api.TrafficPolicy = store.TrafficPolicy{CircuitBreaker: &store.CircuitBreakerPolicy{FailureThreshold: 3, OpenSeconds: 60}}
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	for i := 0; i < 3; i++ {
		do(g, "GET", "/cb", nil, "")
	}
	rec := do(g, "GET", "/cb", nil, "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "upstream_circuit_open") {
		t.Fatalf("after threshold: %d %s, want 503 upstream_circuit_open", rec.Code, rec.Body)
	}
	if bad.hits.Load() != 3 {
		t.Fatalf("upstream hits = %d, want 3 (open circuit must not call upstream)", bad.hits.Load())
	}
	st := g.UpstreamStatus()
	if len(st) != 1 || st[0].Circuit != "open" || st[0].CircuitOpens != 1 {
		t.Fatalf("status = %+v", st)
	}
}

func TestCircuitStateSurvivesReload(t *testing.T) {
	bad := newUpstream(t, 503)
	g := newTestGateway(t)
	api := testAPI("cb", "/cb", bad.srv.URL)
	api.TrafficPolicy = store.TrafficPolicy{CircuitBreaker: &store.CircuitBreakerPolicy{FailureThreshold: 1, OpenSeconds: 60}}
	g.load(t, store.SnapshotData{APIs: []store.API{api}, Revision: 1})
	do(g, "GET", "/cb", nil, "")
	g.load(t, store.SnapshotData{APIs: []store.API{api}, Revision: 2}) // config reload
	do(g, "GET", "/cb", nil, "")
	if bad.hits.Load() != 1 {
		t.Fatalf("hits = %d: a reload must not reset an open circuit", bad.hits.Load())
	}
}

func TestHealthCheckMarksTargetsAndRoutesAround(t *testing.T) {
	var healthy atomic.Bool
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && !healthy.Load() {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte("a"))
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("b")) }))
	defer b.Close()

	g := newTestGateway(t)
	api := testAPI("hc", "/hc", a.URL)
	api.TrafficPolicy = store.TrafficPolicy{
		Targets:     []store.UpstreamTarget{{URL: a.URL, Weight: 1}, {URL: b.URL, Weight: 1}},
		HealthCheck: &store.HealthCheckPolicy{Path: "/healthz", IntervalSeconds: 1, TimeoutMS: 500, UnhealthyThreshold: 1, HealthyThreshold: 1},
	}
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	client := &http.Client{}
	g.probeDue(context.Background(), client, time.Now())
	waitFor(t, func() bool {
		for _, s := range g.UpstreamStatus() {
			if s.URL == a.URL && !s.Healthy {
				return true
			}
		}
		return false
	})
	for i := 0; i < 4; i++ {
		if body := do(g, "GET", "/hc", nil, "").Body.String(); body != "b" {
			t.Fatalf("request %d served by %q, want only healthy target b", i, body)
		}
	}
	healthy.Store(true)
	g.probeDue(context.Background(), client, time.Now().Add(2*time.Second))
	waitFor(t, func() bool {
		for _, s := range g.UpstreamStatus() {
			if s.URL == a.URL && s.Healthy {
				return true
			}
		}
		return false
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// ---------------------------------------------------------------------------
// Canary cohorts
// ---------------------------------------------------------------------------

func canaryData(stableURL, canaryURL string, percent int, header string) store.SnapshotData {
	return store.SnapshotData{
		Revision: 5,
		APIs:     []store.API{testAPI("svc", "/svc", stableURL)},
		Canary: &store.CanaryData{
			Revision: 6, TrafficPercent: percent, Header: header,
			APIs: []store.API{testAPI("svc", "/svc", canaryURL)},
		},
	}
}

func TestCanaryHeaderRoutesToCanaryRevision(t *testing.T) {
	stable, canary := newUpstream(t), newUpstream(t)
	g := newTestGateway(t)
	g.load(t, canaryData(stable.srv.URL, canary.srv.URL, 0, "X-Canary"))

	rec := do(g, "GET", "/svc", map[string]string{"X-Canary": "1"}, "")
	if rec.Header().Get("X-RelayOps-Cohort") != "canary" || rec.Header().Get("X-RelayOps-Revision") != "rev_0000000006" {
		t.Fatalf("cohort=%q revision=%q", rec.Header().Get("X-RelayOps-Cohort"), rec.Header().Get("X-RelayOps-Revision"))
	}
	rec = do(g, "GET", "/svc", nil, "")
	if rec.Header().Get("X-RelayOps-Cohort") != "stable" || rec.Header().Get("X-RelayOps-Revision") != "rev_0000000005" {
		t.Fatalf("no header should stay stable, got %q", rec.Header().Get("X-RelayOps-Cohort"))
	}
	if canary.hits.Load() != 1 || stable.hits.Load() != 1 {
		t.Fatalf("hits stable=%d canary=%d", stable.hits.Load(), canary.hits.Load())
	}
}

func TestCanaryPercentageIsStickyAndApproximate(t *testing.T) {
	stable, canary := newUpstream(t), newUpstream(t)
	g := newTestGateway(t)
	g.load(t, canaryData(stable.srv.URL, canary.srv.URL, 20, ""))
	canaryCount := 0
	const callers = 2000
	for i := 0; i < callers; i++ {
		key := "rk_caller_" + itoa(i)
		first := do(g, "GET", "/svc", map[string]string{"X-API-Key": key}, "").Header().Get("X-RelayOps-Cohort")
		again := do(g, "GET", "/svc", map[string]string{"X-API-Key": key}, "").Header().Get("X-RelayOps-Cohort")
		if first != again {
			t.Fatalf("caller %d switched cohorts (%s -> %s)", i, first, again)
		}
		if first == "canary" {
			canaryCount++
		}
	}
	share := float64(canaryCount) / callers
	if share < 0.15 || share > 0.25 {
		t.Fatalf("canary share = %.3f, want about 0.20", share)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func signHS256(t *testing.T, secret string, claims map[string]any) string {
	t.Helper()
	enc := base64.RawURLEncoding
	h := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	p, _ := json.Marshal(claims)
	unsigned := h + "." + enc.EncodeToString(p)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	return unsigned + "." + enc.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// Tracing
// ---------------------------------------------------------------------------

func TestTraceContextPropagatedAndExported(t *testing.T) {
	var mu sync.Mutex
	var payloads []map[string]any
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		payloads = append(payloads, m)
		mu.Unlock()
	}))
	defer collector.Close()
	tr := tracing.NewWithEndpoint(collector.URL+"/v1/traces", "relayops-test", 1)

	up := newUpstream(t)
	g := newTestGateway(t)
	g.SetTracer(tr)
	g.load(t, store.SnapshotData{APIs: []store.API{testAPI("t", "/t", up.srv.URL)}})

	parent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	rec := do(g, "GET", "/t", map[string]string{"traceparent": parent}, "")
	if rec.Header().Get("X-RelayOps-Trace-Id") != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace id header = %q", rec.Header().Get("X-RelayOps-Trace-Id"))
	}
	got, ok := tracing.ParseTraceparent(up.lastRequest().Header.Get("traceparent"))
	if !ok || got.TraceIDHex() != "4bf92f3577b34da6a3ce929d0e0e4736" || got.SpanIDHex() == "00f067aa0ba902b7" || !got.Sampled {
		t.Fatalf("upstream traceparent = %q", up.lastRequest().Header.Get("traceparent"))
	}

	ctx, cancel := contextWithTimeout(2 * time.Second)
	defer cancel()
	tr.Shutdown(ctx)
	mu.Lock()
	defer mu.Unlock()
	raw, _ := json.Marshal(payloads)
	body := string(raw)
	for _, want := range []string{`"traceId":"4bf92f3577b34da6a3ce929d0e0e4736"`, `"parentSpanId":"00f067aa0ba902b7"`, `"kind":2`, `"kind":3`, `"relayops-test"`, `"http.route"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("exported spans missing %s: %s", want, body)
		}
	}
}

func TestTracingDisabledForwardsIncomingTraceparent(t *testing.T) {
	up := newUpstream(t)
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{testAPI("t", "/t", up.srv.URL)}})
	parent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	do(g, "GET", "/t", map[string]string{"traceparent": parent}, "")
	if got := up.lastRequest().Header.Get("traceparent"); got != parent {
		t.Fatalf("traceparent = %q, want unchanged passthrough", got)
	}
}

func TestAILosslessBodySampling(t *testing.T) {
	var receivedBytes int
	var receivedModel string
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		receivedBytes = len(body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		receivedModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`))
	}))
	defer upSrv.Close()

	g := newTestGateway(t)
	api := testAPI("ai-svc", "/ai", upSrv.URL)
	api.IsAI = true
	g.load(t, store.SnapshotData{APIs: []store.API{api}})

	// Construct a JSON payload > 5 MiB (6 MiB total)
	prefix := []byte(`{"model":"gpt-4o","content":"`)
	suffix := []byte(`"}`)
	paddingSize := (6 << 20) - len(prefix) - len(suffix)
	padding := bytes.Repeat([]byte("A"), paddingSize)
	largeBody := append(prefix, append(padding, suffix...)...)

	rec := do(g, "POST", "/ai", nil, string(largeBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	if receivedBytes != len(largeBody) {
		t.Fatalf("expected upstream to receive all %d bytes, but received %d bytes (truncated!)", len(largeBody), receivedBytes)
	}
	if receivedModel != "gpt-4o" {
		t.Fatalf("expected upstream to receive model 'gpt-4o', got %q", receivedModel)
	}
}

type mockBudgetLedger struct {
	account *store.AIBudgetAccount
	err     error
	settled bool
}

func (m *mockBudgetLedger) GetAIBudgetAccountByConsumer(ctx context.Context, tenantID, consumerID string) (*store.AIBudgetAccount, error) {
	return m.account, nil
}

func (m *mockBudgetLedger) ReserveAIBudget(ctx context.Context, accountID, requestID string, reserveCents int64) (*store.AIBudgetReservation, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &store.AIBudgetReservation{ID: "res-1", AccountID: accountID, RequestID: requestID, ReservedCents: reserveCents}, nil
}

func (m *mockBudgetLedger) SettleAIBudgetUsage(ctx context.Context, requestID string, usage store.AIUsage) (int64, error) {
	m.settled = true
	return usage.EstimateCents, nil
}

func (m *mockBudgetLedger) ReleaseAIBudget(ctx context.Context, requestID string) error {
	return nil
}

func TestAIBudgetEnforcement(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upSrv.Close()

	g := newTestGateway(t)
	api := testAPI("ai-svc", "/ai", upSrv.URL)
	api.IsAI = true
	api.AuthType = "api_key"
	g.load(t, store.SnapshotData{
		APIs: []store.API{api},
		Keys: []store.KeyRecord{
			{KeyHash: store.HashKey("rk_good"), KeyID: "k1", ConsumerID: "c1", ConsumerName: "Acme", ConsumerStatus: "active"},
		},
		Subs: []store.SubRecord{
			{ConsumerID: "c1", APIID: api.ID, Status: "approved", Active: true},
		},
	})

	cid := "c1"
	// 1. Budget Exceeded case: must be blocked before reaching upstream
	mockExceeded := &mockBudgetLedger{
		account: &store.AIBudgetAccount{ID: "acc-1", ConsumerID: &cid, StrictEnforcement: true},
		err:     store.ErrBudgetExceeded,
	}
	g.SetAIBudgetLedger(mockExceeded)

	recExceeded := do(g, "POST", "/ai", map[string]string{"X-API-Key": "rk_good"}, `{"model":"gpt-4o"}`)
	if recExceeded.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests, got %d: %s", recExceeded.Code, recExceeded.Body.String())
	}
	if !strings.Contains(recExceeded.Body.String(), "ai_budget_exceeded") {
		t.Fatalf("expected error body to contain 'ai_budget_exceeded', got %s", recExceeded.Body.String())
	}

	// 2. Budget Available case: allowed, proxied, and settled
	mockAllowed := &mockBudgetLedger{
		account: &store.AIBudgetAccount{ID: "acc-1", ConsumerID: &cid, StrictEnforcement: true},
		err:     nil,
	}
	g.SetAIBudgetLedger(mockAllowed)

	recAllowed := do(g, "POST", "/ai", map[string]string{"X-API-Key": "rk_good"}, `{"model":"gpt-4o"}`)
	if recAllowed.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", recAllowed.Code, recAllowed.Body.String())
	}
	if !mockAllowed.settled {
		t.Fatalf("expected budget reservation to be settled after successful call")
	}
}

