package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/store"
)

var (
	// ErrCircuitOpen is returned when every target of an API has an open circuit.
	ErrCircuitOpen = errors.New("all upstream targets have open circuits")
)

const (
	breakerClosed = iota
	breakerOpen
	breakerHalfOpen
)

// targetState is the runtime state of one upstream target. It lives in the
// registry, keyed by API ID and URL, so breaker and health state survive config
// reloads: a reload must not instantly re-admit traffic to a failing backend.
type targetState struct {
	key      string
	apiID    string
	rawURL   string
	url      *url.URL
	weight   atomic.Int64
	inflight atomic.Int64

	healthy        atomic.Bool
	probeRunning   atomic.Bool
	retriesTotal   atomic.Int64
	failuresTotal  atomic.Int64
	requestsTotal  atomic.Int64
	breakerOpens   atomic.Int64
	lastProbeNanos atomic.Int64

	mu                  sync.Mutex
	state               int
	consecutiveFailures int
	openUntil           time.Time
	probeInFlight       bool
	hcSuccesses         int
	hcFailures          int
	lastError           string
	currentWeight       int // smooth weighted round robin, guarded by the pool lock
}

type upstreamRegistry struct {
	mu     sync.Mutex
	states map[string]*targetState
}

func newUpstreamRegistry() *upstreamRegistry {
	return &upstreamRegistry{states: map[string]*targetState{}}
}

func (r *upstreamRegistry) get(apiID, raw string, u *url.URL, weight int) *targetState {
	key := apiID + "|" + raw
	r.mu.Lock()
	defer r.mu.Unlock()
	ts, ok := r.states[key]
	if !ok {
		ts = &targetState{key: key, apiID: apiID, rawURL: raw, url: u}
		ts.healthy.Store(true)
		r.states[key] = ts
	}
	ts.weight.Store(int64(weight))
	return ts
}

// retain drops state for targets no longer referenced by any loaded snapshot.
func (r *upstreamRegistry) retain(keep map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.states {
		if !keep[k] {
			delete(r.states, k)
		}
	}
}

func (r *upstreamRegistry) snapshot() []*targetState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*targetState, 0, len(r.states))
	for _, ts := range r.states {
		out = append(out, ts)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// upstreamPool selects targets for one API according to its traffic policy.
type upstreamPool struct {
	apiID   string
	targets []*targetState
	policy  store.TrafficPolicy
	mu      sync.Mutex
}

func newUpstreamPool(a store.API, reg *upstreamRegistry) (*upstreamPool, error) {
	p := &upstreamPool{apiID: a.ID, policy: a.TrafficPolicy}
	for _, t := range a.EffectiveTargets() {
		u, err := url.Parse(t.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("invalid upstream target %q", t.URL)
		}
		w := t.Weight
		if w == 0 && len(a.TrafficPolicy.Targets) == 0 {
			w = 1
		}
		p.targets = append(p.targets, reg.get(a.ID, t.URL, u, w))
	}
	return p, nil
}

// available reports whether the target may receive a request now, without
// reserving a half-open probe slot.
func (p *upstreamPool) available(t *targetState, now time.Time) bool {
	if t.weight.Load() <= 0 {
		return false
	}
	if p.policy.CircuitBreaker == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.state {
	case breakerOpen:
		return !now.Before(t.openUntil) && !t.probeInFlight
	case breakerHalfOpen:
		return !t.probeInFlight
	}
	return true
}

// acquire reserves the target. For an expired open breaker it moves to
// half-open and claims the single probe slot.
func (p *upstreamPool) acquire(t *targetState, now time.Time) bool {
	if p.policy.CircuitBreaker == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.state {
	case breakerOpen:
		if now.Before(t.openUntil) || t.probeInFlight {
			return false
		}
		t.state = breakerHalfOpen
		t.probeInFlight = true
	case breakerHalfOpen:
		if t.probeInFlight {
			return false
		}
		t.probeInFlight = true
	}
	return true
}

// pick chooses a target. Targets in avoid (already tried for this request) are
// used only when nothing else is available. Health-check failures are ignored
// when every target is unhealthy (panic mode), but open circuits are respected.
func (p *upstreamPool) pick(avoid map[*targetState]bool) (*targetState, error) {
	now := time.Now()
	for attempt := 0; attempt < 3; attempt++ {
		var fresh, retried, unhealthy []*targetState
		for _, t := range p.targets {
			if !p.available(t, now) {
				continue
			}
			switch {
			case p.policy.HealthCheck != nil && !t.healthy.Load():
				unhealthy = append(unhealthy, t)
			case avoid[t]:
				retried = append(retried, t)
			default:
				fresh = append(fresh, t)
			}
		}
		candidates := fresh
		if len(candidates) == 0 {
			candidates = retried
		}
		if len(candidates) == 0 {
			candidates = unhealthy
		}
		if len(candidates) == 0 {
			return nil, ErrCircuitOpen
		}
		t := p.choose(candidates)
		if p.acquire(t, now) {
			return t, nil
		}
	}
	return nil, ErrCircuitOpen
}

func (p *upstreamPool) choose(c []*targetState) *targetState {
	if len(c) == 1 {
		return c[0]
	}
	switch p.policy.LoadBalancing {
	case "random":
		total := int64(0)
		for _, t := range c {
			total += t.weight.Load()
		}
		n := rand.Int63n(total)
		for _, t := range c {
			n -= t.weight.Load()
			if n < 0 {
				return t
			}
		}
		return c[len(c)-1]
	case "least_requests":
		best := c[0]
		bestScore := float64(best.inflight.Load()+1) / float64(best.weight.Load())
		for _, t := range c[1:] {
			score := float64(t.inflight.Load()+1) / float64(t.weight.Load())
			if score < bestScore {
				best, bestScore = t, score
			}
		}
		return best
	default: // smooth weighted round robin (nginx algorithm)
		p.mu.Lock()
		defer p.mu.Unlock()
		total := 0
		var best *targetState
		for _, t := range c {
			w := int(t.weight.Load())
			t.currentWeight += w
			total += w
			if best == nil || t.currentWeight > best.currentWeight {
				best = t
			}
		}
		best.currentWeight -= total
		return best
	}
}

// report records the outcome of an attempt for circuit breaking.
func (p *upstreamPool) report(t *targetState, ok bool, errMsg string) {
	t.requestsTotal.Add(1)
	if !ok {
		t.failuresTotal.Add(1)
	}
	cb := p.policy.CircuitBreaker
	t.mu.Lock()
	defer t.mu.Unlock()
	if !ok {
		t.lastError = errMsg
	}
	if cb == nil {
		return
	}
	wasProbe := t.state == breakerHalfOpen
	t.probeInFlight = false
	if ok {
		t.consecutiveFailures = 0
		t.state = breakerClosed
		return
	}
	t.consecutiveFailures++
	if wasProbe || t.consecutiveFailures >= cb.FailureThreshold {
		if t.state != breakerOpen {
			t.breakerOpens.Add(1)
		}
		t.state = breakerOpen
		t.openUntil = time.Now().Add(time.Duration(cb.OpenSeconds) * time.Second)
	}
}

// release frees a reserved half-open probe slot when the attempt never reached
// the target (for example the client went away first).
func (p *upstreamPool) release(t *targetState) {
	t.mu.Lock()
	t.probeInFlight = false
	t.mu.Unlock()
}

func (p *upstreamPool) retryPolicy() *store.RetryPolicy { return p.policy.Retries }

// isBreakerFailure classifies an upstream response status as a target failure.
func isBreakerFailure(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func statusIn(status int, list []int) bool {
	for _, s := range list {
		if s == status {
			return true
		}
	}
	return false
}

func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodTrace:
		return true
	}
	return false
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func joinURLPath(a, b *url.URL) (path, rawpath string) {
	if b.Path == "" {
		if a.Path == "" {
			return "/", ""
		}
		return a.Path, a.RawPath
	}
	if a.RawPath == "" && b.RawPath == "" {
		return singleJoiningSlash(a.Path, b.Path), ""
	}
	apath := a.EscapedPath()
	bpath := b.EscapedPath()
	aslash := strings.HasSuffix(apath, "/")
	bslash := strings.HasPrefix(bpath, "/")
	switch {
	case aslash && bslash:
		return a.Path + b.Path[1:], apath + bpath[1:]
	case !aslash && !bslash:
		return a.Path + "/" + b.Path, apath + "/" + bpath
	}
	return a.Path + b.Path, apath + bpath
}

// ---------------------------------------------------------------------------
// Active health checks
// ---------------------------------------------------------------------------

// RunHealthChecks probes targets of every API that configures a health check.
// It blocks until ctx is cancelled.
func (g *Gateway) RunHealthChecks(ctx context.Context) {
	client := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			g.probeDue(ctx, client, time.Now())
		}
	}
}

func (g *Gateway) probeDue(ctx context.Context, client *http.Client, now time.Time) {
	snap := g.snap.Load()
	if snap == nil {
		return
	}
	seen := map[*targetState]bool{}
	for _, s := range []*Snapshot{snap, snap.Canary} {
		if s == nil {
			continue
		}
		for _, r := range s.Routes {
			hc := r.API.TrafficPolicy.HealthCheck
			if hc == nil {
				continue
			}
			for _, t := range r.Pool.targets {
				if seen[t] {
					continue
				}
				seen[t] = true
				interval := time.Duration(hc.IntervalSeconds) * time.Second
				if now.UnixNano()-t.lastProbeNanos.Load() < int64(interval) {
					continue
				}
				if !t.probeRunning.CompareAndSwap(false, true) {
					continue
				}
				t.lastProbeNanos.Store(now.UnixNano())
				go probeTarget(ctx, client, t, *hc)
			}
		}
	}
}

func probeTarget(ctx context.Context, client *http.Client, t *targetState, hc store.HealthCheckPolicy) {
	defer t.probeRunning.Store(false)
	pctx, cancel := context.WithTimeout(ctx, time.Duration(hc.TimeoutMS)*time.Millisecond)
	defer cancel()
	probeURL := *t.url
	probeURL.Path = singleJoiningSlash(strings.TrimSuffix(t.url.Path, "/"), hc.Path)
	if t.url.Path == "" {
		probeURL.Path = hc.Path
	}
	probeURL.RawPath, probeURL.RawQuery = "", ""
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, probeURL.String(), nil)
	ok := false
	errMsg := ""
	if err == nil {
		req.Header.Set("User-Agent", "RelayOps-HealthCheck/1.0")
		resp, rerr := client.Do(req)
		if rerr != nil {
			errMsg = rerr.Error()
		} else {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			ok = resp.StatusCode >= 200 && resp.StatusCode < 400
			if !ok {
				errMsg = fmt.Sprintf("health check returned %d", resp.StatusCode)
			}
		}
	} else {
		errMsg = err.Error()
	}
	recordProbe(t, ok, errMsg, hc)
}

func recordProbe(t *targetState, ok bool, errMsg string, hc store.HealthCheckPolicy) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ok {
		t.hcSuccesses++
		t.hcFailures = 0
		if !t.healthy.Load() && t.hcSuccesses >= hc.HealthyThreshold {
			t.healthy.Store(true)
		}
		return
	}
	t.hcFailures++
	t.hcSuccesses = 0
	t.lastError = errMsg
	if t.healthy.Load() && t.hcFailures >= hc.UnhealthyThreshold {
		t.healthy.Store(false)
	}
}

// ---------------------------------------------------------------------------
// Introspection
// ---------------------------------------------------------------------------

// TargetStatus is a point-in-time view of one upstream target on this node.
type TargetStatus struct {
	APIID               string     `json:"api_id"`
	URL                 string     `json:"url"`
	Weight              int64      `json:"weight"`
	Healthy             bool       `json:"healthy"`
	Circuit             string     `json:"circuit"` // closed | open | half_open
	OpenUntil           *time.Time `json:"open_until,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	InFlight            int64      `json:"in_flight"`
	Requests            int64      `json:"requests_total"`
	Failures            int64      `json:"failures_total"`
	Retries             int64      `json:"retries_total"`
	CircuitOpens        int64      `json:"circuit_opens_total"`
	LastError           string     `json:"last_error,omitempty"`
}

// UpstreamStatus lists the state of every upstream target known to this node.
func (g *Gateway) UpstreamStatus() []TargetStatus {
	var out []TargetStatus
	for _, t := range g.upstreams.snapshot() {
		t.mu.Lock()
		st := TargetStatus{
			APIID: t.apiID, URL: t.rawURL, Weight: t.weight.Load(), Healthy: t.healthy.Load(),
			ConsecutiveFailures: t.consecutiveFailures, InFlight: t.inflight.Load(),
			Requests: t.requestsTotal.Load(), Failures: t.failuresTotal.Load(), Retries: t.retriesTotal.Load(),
			CircuitOpens: t.breakerOpens.Load(), LastError: t.lastError,
		}
		switch t.state {
		case breakerOpen:
			st.Circuit = "open"
			ou := t.openUntil
			st.OpenUntil = &ou
		case breakerHalfOpen:
			st.Circuit = "half_open"
		default:
			st.Circuit = "closed"
		}
		t.mu.Unlock()
		out = append(out, st)
	}
	return out
}

// WriteUpstreamMetrics renders Prometheus metrics for upstream targets.
func (g *Gateway) WriteUpstreamMetrics(w io.Writer) {
	status := g.UpstreamStatus()
	if len(status) == 0 {
		return
	}
	series := []struct {
		name, help, typ string
		val             func(TargetStatus) float64
	}{
		{"relayops_upstream_target_healthy", "Active health-check state of an upstream target (1=healthy)", "gauge", func(s TargetStatus) float64 { return b2f(s.Healthy) }},
		{"relayops_upstream_circuit_open", "Circuit breaker state of an upstream target (1=open or half-open)", "gauge", func(s TargetStatus) float64 { return b2f(s.Circuit != "closed") }},
		{"relayops_upstream_requests_total", "Upstream attempts sent to a target", "counter", func(s TargetStatus) float64 { return float64(s.Requests) }},
		{"relayops_upstream_failures_total", "Upstream attempts that failed (transport error or 502/503/504)", "counter", func(s TargetStatus) float64 { return float64(s.Failures) }},
		{"relayops_upstream_retries_total", "Retries sent to a target after a failed attempt", "counter", func(s TargetStatus) float64 { return float64(s.Retries) }},
		{"relayops_upstream_circuit_opens_total", "Times the target circuit transitioned to open", "counter", func(s TargetStatus) float64 { return float64(s.CircuitOpens) }},
	}
	for _, m := range series {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.typ)
		for _, s := range status {
			fmt.Fprintf(w, "%s{api_id=%q,target=%q} %g\n", m.name, s.APIID, s.URL, m.val(s))
		}
		fmt.Fprintln(w)
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
