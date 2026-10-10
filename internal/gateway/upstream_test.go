package gateway

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/relayops/apim/internal/store"
)

func testPool(t *testing.T, policy store.TrafficPolicy, urls ...string) *upstreamPool {
	t.Helper()
	a := store.API{ID: "api", UpstreamURL: urls[0], TrafficPolicy: policy}
	if len(urls) > 1 || len(policy.Targets) == 0 {
		a.TrafficPolicy.Targets = nil
		for _, u := range urls {
			a.TrafficPolicy.Targets = append(a.TrafficPolicy.Targets, store.UpstreamTarget{URL: u, Weight: 1})
		}
	}
	a.TrafficPolicy.Normalize()
	if err := a.TrafficPolicy.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := newUpstreamPool(a, newUpstreamRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBreakerHalfOpenAllowsSingleProbe(t *testing.T) {
	p := testPool(t, store.TrafficPolicy{CircuitBreaker: &store.CircuitBreakerPolicy{FailureThreshold: 2, OpenSeconds: 1}}, "http://a")
	tgt := p.targets[0]
	p.report(tgt, false, "boom")
	p.report(tgt, false, "boom")
	if _, err := p.pick(nil); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("pick while open = %v, want ErrCircuitOpen", err)
	}

	// Expire the open window: exactly one probe is admitted.
	tgt.mu.Lock()
	tgt.openUntil = time.Now().Add(-time.Millisecond)
	tgt.mu.Unlock()
	probe, err := p.pick(nil)
	if err != nil || probe != tgt {
		t.Fatalf("first pick after expiry = %v, %v", probe, err)
	}
	if _, err := p.pick(nil); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("a second concurrent request must not be admitted while the probe is in flight")
	}

	// A failed probe re-opens immediately; a successful one closes the circuit.
	p.report(tgt, false, "still down")
	if tgt.state != breakerOpen {
		t.Fatalf("state after failed probe = %d, want open", tgt.state)
	}
	tgt.mu.Lock()
	tgt.openUntil = time.Now().Add(-time.Millisecond)
	tgt.mu.Unlock()
	if _, err := p.pick(nil); err != nil {
		t.Fatal(err)
	}
	p.report(tgt, true, "")
	if tgt.state != breakerClosed || tgt.consecutiveFailures != 0 {
		t.Fatalf("state after good probe = %d failures=%d", tgt.state, tgt.consecutiveFailures)
	}
}

func TestReleaseFreesProbeSlot(t *testing.T) {
	p := testPool(t, store.TrafficPolicy{CircuitBreaker: &store.CircuitBreakerPolicy{FailureThreshold: 1, OpenSeconds: 1}}, "http://a")
	tgt := p.targets[0]
	p.report(tgt, false, "x")
	tgt.openUntil = time.Now().Add(-time.Millisecond)
	if _, err := p.pick(nil); err != nil {
		t.Fatal(err)
	}
	p.release(tgt) // client went away before the probe completed
	if _, err := p.pick(nil); err != nil {
		t.Fatalf("probe slot not released: %v", err)
	}
}

func TestPickPrefersUntriedTargets(t *testing.T) {
	p := testPool(t, store.TrafficPolicy{}, "http://a", "http://b")
	first, _ := p.pick(nil)
	second, _ := p.pick(map[*targetState]bool{first: true})
	if second == first {
		t.Fatal("retry should prefer a target that has not been tried")
	}
	third, err := p.pick(map[*targetState]bool{p.targets[0]: true, p.targets[1]: true})
	if err != nil || third == nil {
		t.Fatalf("when all were tried, a tried target is reused: %v", err)
	}
}

func TestPanicModeUsesUnhealthyTargetsWhenNoneHealthy(t *testing.T) {
	p := testPool(t, store.TrafficPolicy{HealthCheck: &store.HealthCheckPolicy{Path: "/h"}}, "http://a", "http://b")
	for _, tg := range p.targets {
		tg.healthy.Store(false)
	}
	if _, err := p.pick(nil); err != nil {
		t.Fatalf("all-unhealthy pool must still serve (panic mode): %v", err)
	}
	p.targets[1].healthy.Store(true)
	for i := 0; i < 5; i++ {
		if got, _ := p.pick(nil); got != p.targets[1] {
			t.Fatal("healthy target must be preferred over unhealthy ones")
		}
	}
}

func TestLeastRequestsPrefersIdleTarget(t *testing.T) {
	p := testPool(t, store.TrafficPolicy{LoadBalancing: "least_requests"}, "http://a", "http://b")
	p.targets[0].inflight.Store(5)
	if got, _ := p.pick(nil); got != p.targets[1] {
		t.Fatal("least_requests should choose the idle target")
	}
}

func TestZeroWeightTargetIsDrained(t *testing.T) {
	var policy store.TrafficPolicy
	if err := json.Unmarshal([]byte(`{"targets":[{"url":"http://a"},{"url":"http://drain","weight":0}]}`), &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Targets[0].Weight != 1 || policy.Targets[1].Weight != 0 {
		t.Fatalf("weights = %d,%d; omitted must default to 1 and explicit 0 must drain", policy.Targets[0].Weight, policy.Targets[1].Weight)
	}
	policy.Normalize()
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	p, _ := newUpstreamPool(store.API{ID: "api", UpstreamURL: "http://a", TrafficPolicy: policy}, newUpstreamRegistry())
	for i := 0; i < 10; i++ {
		if got, _ := p.pick(nil); got.rawURL != "http://a" {
			t.Fatal("zero-weight target must not receive traffic")
		}
	}
}

func TestTargetURLJoinsBasePathAndKeepsQuery(t *testing.T) {
	target, _ := url.Parse("http://b:9000/api/v2")
	in, _ := url.Parse("/orders/7")
	got := targetURL(target, in, "a=1")
	if got.String() != "http://b:9000/api/v2/orders/7?a=1" {
		t.Fatalf("got %s", got)
	}
	root, _ := url.Parse("http://b:9000")
	if got := targetURL(root, in, ""); got.String() != "http://b:9000/orders/7" {
		t.Fatalf("got %s", got)
	}
}

func TestForRequestHeaderValueMustMatch(t *testing.T) {
	s := &Snapshot{Version: 1, Canary: &Snapshot{Version: 2, IsCanary: true}, CanaryRule: CanaryRule{Header: "X-Ring", HeaderValue: "beta"}}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Ring", "alpha")
	if s.ForRequest(r, "1.2.3.4").IsCanary {
		t.Fatal("non-matching header value must stay stable")
	}
	r.Header.Set("X-Ring", "beta")
	if !s.ForRequest(r, "1.2.3.4").IsCanary {
		t.Fatal("matching header value must route to canary")
	}
	full := &Snapshot{Version: 1, Canary: &Snapshot{Version: 2, IsCanary: true}, CanaryRule: CanaryRule{TrafficPercent: 100}}
	if !full.ForRequest(httptest.NewRequest("GET", "/", nil), "9.9.9.9").IsCanary {
		t.Fatal("100% canary must take every request")
	}
}

func TestInvalidTrafficPolicyRejectedAtBuild(t *testing.T) {
	api := testAPI("x", "/x", "http://a")
	api.TrafficPolicy = store.TrafficPolicy{Retries: &store.RetryPolicy{Attempts: 99}}
	_, errs := buildSnapshot(store.SnapshotData{APIs: []store.API{api}}, 1, newUpstreamRegistry())
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want the invalid API reported and skipped", errs)
	}
}

func TestExactBasePathMapsToUpstreamURL(t *testing.T) {
	up := newUpstream(t, 200)
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{testAPI("mcp", "/tools", up.srv.URL+"/mcp/message")}})
	for path, want := range map[string]string{"/tools": "/mcp/message", "/tools/": "/mcp/message/", "/tools/x": "/mcp/message/x"} {
		if rec := do(g, "POST", path, nil, "{}"); rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if got := up.last.URL.Path; got != want {
			t.Errorf("%s reached upstream as %s, want %s", path, got, want)
		}
	}
	root := newTestGateway(t)
	root.load(t, store.SnapshotData{APIs: []store.API{testAPI("root", "/r", up.srv.URL)}})
	if do(root, "GET", "/r", nil, ""); up.last.URL.Path != "/" {
		t.Errorf("upstream without a path got %q", up.last.URL.Path)
	}
}
