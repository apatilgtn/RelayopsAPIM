package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// TrafficPolicy controls how the gateway reaches an API's upstream: which targets
// receive traffic, how they are balanced, and how failures are retried, isolated
// (circuit breaking) and detected (active health checks). The zero value keeps the
// historical behaviour: a single upstream_url, no retries, no breaker, no probes.
type TrafficPolicy struct {
	Targets        []UpstreamTarget      `json:"targets,omitempty"`
	LoadBalancing  string                `json:"load_balancing,omitempty"` // round_robin (default) | random | least_requests
	Retries        *RetryPolicy          `json:"retries,omitempty"`
	CircuitBreaker *CircuitBreakerPolicy `json:"circuit_breaker,omitempty"`
	HealthCheck    *HealthCheckPolicy    `json:"health_check,omitempty"`
	// WasmPlugins run in order on each request after authentication and rate
	// limits, before the upstream call. Names refer to *.wasm files each
	// gateway loads from RELAYOPS_WASM_PLUGINS_DIR; a gateway without a named
	// plugin refuses the API's requests rather than skipping the plugin.
	WasmPlugins []string `json:"wasm_plugins,omitempty"`
	// WasmConfig gives a plugin its settings for this API (plugin name ->
	// JSON object), passed to the plugin as "config" on every request.
	WasmConfig map[string]json.RawMessage `json:"wasm_config,omitempty"`
	// WasmBodyLimitBytes > 0 hands plugins the request body as text, up to
	// this size; a larger body is refused with 413. 0 keeps the body
	// streaming and plugins see none.
	WasmBodyLimitBytes int `json:"wasm_body_limit_bytes,omitempty"`
	// WasmResponseBodyLimitBytes > 0 hands response-phase plugins the
	// response body (up to this size; larger or streamed bodies are passed
	// through and plugins are told the body is unavailable).
	WasmResponseBodyLimitBytes int `json:"wasm_response_body_limit_bytes,omitempty"`
}

// MaxWasmPlugins bounds the plugin chain per API.
const MaxWasmPlugins = 8

// MaxWasmBodyBytes bounds the request body buffered for plugins, and
// MaxWasmConfigBytes one plugin's settings.
const (
	MaxWasmBodyBytes   = 1 << 20
	MaxWasmConfigBytes = 16 << 10
)

var wasmPluginName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// UpstreamTarget is one backend instance. An omitted weight defaults to 1;
// an explicit "weight": 0 drains the target (it receives no new traffic).
type UpstreamTarget struct {
	URL    string `json:"url"`
	Weight int    `json:"weight"`
}

// UnmarshalJSON applies the default weight only when the field is absent.
func (t *UpstreamTarget) UnmarshalJSON(b []byte) error {
	var raw struct {
		URL    string `json:"url"`
		Weight *int   `json:"weight"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	t.URL, t.Weight = raw.URL, 1
	if raw.Weight != nil {
		t.Weight = *raw.Weight
	}
	return nil
}

// RetryPolicy retries failed upstream attempts on another healthy target.
// Attempts counts the first try, so Attempts=3 means up to two retries.
type RetryPolicy struct {
	Attempts           int   `json:"attempts"`
	BackoffMS          int   `json:"backoff_ms,omitempty"`
	RetryOnStatus      []int `json:"retry_on_status,omitempty"` // default 502, 503, 504
	RetryNonIdempotent bool  `json:"retry_non_idempotent,omitempty"`
}

// CircuitBreakerPolicy opens a target after FailureThreshold consecutive failures.
// While open the target receives no traffic; after OpenSeconds a single probe
// request is let through (half-open) and its outcome closes or re-opens it.
type CircuitBreakerPolicy struct {
	FailureThreshold int `json:"failure_threshold"`
	OpenSeconds      int `json:"open_seconds,omitempty"`
}

// HealthCheckPolicy actively probes every target with GET Path.
type HealthCheckPolicy struct {
	Path               string `json:"path"`
	IntervalSeconds    int    `json:"interval_seconds,omitempty"`
	TimeoutMS          int    `json:"timeout_ms,omitempty"`
	UnhealthyThreshold int    `json:"unhealthy_threshold,omitempty"`
	HealthyThreshold   int    `json:"healthy_threshold,omitempty"`
}

const (
	MaxRetryAttempts = 5
	MaxTargets       = 32
)

// Normalize fills defaults in place. It is idempotent.
func (p *TrafficPolicy) Normalize() {
	p.LoadBalancing = strings.ToLower(strings.TrimSpace(p.LoadBalancing))
	if p.LoadBalancing == "" {
		p.LoadBalancing = "round_robin"
	}
	for i := range p.Targets {
		p.Targets[i].URL = strings.TrimSpace(p.Targets[i].URL)
	}
	if r := p.Retries; r != nil {
		if r.BackoffMS == 0 {
			r.BackoffMS = 25
		}
		if len(r.RetryOnStatus) == 0 {
			r.RetryOnStatus = []int{502, 503, 504}
		}
	}
	if cb := p.CircuitBreaker; cb != nil && cb.OpenSeconds == 0 {
		cb.OpenSeconds = 30
	}
	if hc := p.HealthCheck; hc != nil {
		if hc.IntervalSeconds == 0 {
			hc.IntervalSeconds = 10
		}
		if hc.TimeoutMS == 0 {
			hc.TimeoutMS = 2000
		}
		if hc.UnhealthyThreshold == 0 {
			hc.UnhealthyThreshold = 3
		}
		if hc.HealthyThreshold == 0 {
			hc.HealthyThreshold = 2
		}
	}
}

// Validate reports the first invalid setting. Call Normalize first.
func (p TrafficPolicy) Validate() error {
	switch p.LoadBalancing {
	case "", "round_robin", "random", "least_requests":
	default:
		return errors.New("traffic_policy.load_balancing must be one of: round_robin, random, least_requests")
	}
	if len(p.Targets) > MaxTargets {
		return fmt.Errorf("traffic_policy.targets supports at most %d targets", MaxTargets)
	}
	seen := map[string]bool{}
	total := 0
	for i, t := range p.Targets {
		u, err := url.Parse(t.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("traffic_policy.targets[%d].url must be an absolute http(s) URL", i)
		}
		if t.Weight < 0 || t.Weight > 1000 {
			return fmt.Errorf("traffic_policy.targets[%d].weight must be between 0 and 1000", i)
		}
		if seen[t.URL] {
			return fmt.Errorf("traffic_policy.targets[%d].url %q is duplicated", i, t.URL)
		}
		seen[t.URL] = true
		total += t.Weight
	}
	if len(p.Targets) > 0 && total == 0 {
		return errors.New("traffic_policy.targets must have at least one target with weight > 0")
	}
	if len(p.WasmPlugins) > MaxWasmPlugins {
		return fmt.Errorf("traffic_policy.wasm_plugins supports at most %d plugins", MaxWasmPlugins)
	}
	for i, name := range p.WasmPlugins {
		if !wasmPluginName.MatchString(name) {
			return fmt.Errorf("traffic_policy.wasm_plugins[%d] %q must be lowercase letters, digits, '-' or '_' (the .wasm file name without extension)", i, name)
		}
	}
	for name, raw := range p.WasmConfig {
		if !slices.Contains(p.WasmPlugins, name) {
			return fmt.Errorf("traffic_policy.wasm_config has settings for %q, which is not in wasm_plugins", name)
		}
		if len(raw) > MaxWasmConfigBytes {
			return fmt.Errorf("traffic_policy.wasm_config[%q] is larger than %d bytes", name, MaxWasmConfigBytes)
		}
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			return fmt.Errorf("traffic_policy.wasm_config[%q] must be a JSON object", name)
		}
	}
	if p.WasmBodyLimitBytes < 0 || p.WasmBodyLimitBytes > MaxWasmBodyBytes {
		return fmt.Errorf("traffic_policy.wasm_body_limit_bytes must be between 0 and %d", MaxWasmBodyBytes)
	}
	if p.WasmResponseBodyLimitBytes < 0 || p.WasmResponseBodyLimitBytes > 4*MaxWasmBodyBytes {
		return fmt.Errorf("traffic_policy.wasm_response_body_limit_bytes must be between 0 and %d", 4*MaxWasmBodyBytes)
	}
	if r := p.Retries; r != nil {
		if r.Attempts < 1 || r.Attempts > MaxRetryAttempts {
			return fmt.Errorf("traffic_policy.retries.attempts must be between 1 and %d", MaxRetryAttempts)
		}
		if r.BackoffMS < 0 || r.BackoffMS > 10_000 {
			return errors.New("traffic_policy.retries.backoff_ms must be between 0 and 10000")
		}
		for _, s := range r.RetryOnStatus {
			if s < 500 || s > 599 {
				return errors.New("traffic_policy.retries.retry_on_status only accepts 5xx status codes")
			}
		}
	}
	if cb := p.CircuitBreaker; cb != nil {
		if cb.FailureThreshold < 1 || cb.FailureThreshold > 1000 {
			return errors.New("traffic_policy.circuit_breaker.failure_threshold must be between 1 and 1000")
		}
		if cb.OpenSeconds < 1 || cb.OpenSeconds > 3600 {
			return errors.New("traffic_policy.circuit_breaker.open_seconds must be between 1 and 3600")
		}
	}
	if hc := p.HealthCheck; hc != nil {
		if !strings.HasPrefix(hc.Path, "/") {
			return errors.New("traffic_policy.health_check.path must start with '/'")
		}
		if hc.IntervalSeconds < 1 || hc.IntervalSeconds > 300 {
			return errors.New("traffic_policy.health_check.interval_seconds must be between 1 and 300")
		}
		if hc.TimeoutMS < 50 || hc.TimeoutMS > 60_000 {
			return errors.New("traffic_policy.health_check.timeout_ms must be between 50 and 60000")
		}
		if hc.UnhealthyThreshold < 1 || hc.HealthyThreshold < 1 {
			return errors.New("traffic_policy.health_check thresholds must be >= 1")
		}
	}
	return nil
}

// EffectiveTargets returns the configured targets, or the single upstream URL.
func (a API) EffectiveTargets() []UpstreamTarget {
	if len(a.TrafficPolicy.Targets) > 0 {
		return a.TrafficPolicy.Targets
	}
	return []UpstreamTarget{{URL: a.UpstreamURL, Weight: 1}}
}

func nonNilHeaders(h map[string]string) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	return h
}

func nonNilSpec(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func visibilityOrDefault(v string) string {
	if v == "" {
		return "public"
	}
	return v
}

func quotaPolicyOrDefault(v string) string {
	if v == "" {
		return "fail_open"
	}
	return v
}
