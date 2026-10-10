package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveSubLimitsUsesRevisionPlans(t *testing.T) {
	live60 := 60
	live := []SubRecord{
		{ConsumerID: "c1", APIID: "a", PlanID: "p-gold", PlanName: "gold", RateLimitPerMinute: &live60},
		{ConsumerID: "c2", APIID: "a", PlanID: "p-new", PlanName: "new", RateLimitPerMinute: &live60},
		{ConsumerID: "c3", APIID: "a"},
	}
	plans := map[string]Plan{"p-gold": {ID: "p-gold", Name: "gold", RateLimitPerMinute: 600, QuotaPerDay: 5}}
	got := ResolveSubLimits(live, plans, true)

	if *got[0].RateLimitPerMinute != 600 || *got[0].QuotaPerDay != 5 || got[0].LimitsSource != "revision" {
		t.Fatalf("gold should use the revision's plan limits, got %+v", got[0])
	}
	if *got[1].RateLimitPerMinute != 60 || got[1].LimitsSource != "live" {
		t.Fatalf("a plan unknown to the revision keeps live limits, got %+v", got[1])
	}
	if got[2].RateLimitPerMinute != nil {
		t.Fatal("a subscription without a plan has no plan limits")
	}
	if *live[0].RateLimitPerMinute != 60 {
		t.Fatal("ResolveSubLimits must not mutate its input")
	}
	legacy := ResolveSubLimits(live, nil, false)
	if *legacy[0].RateLimitPerMinute != 60 || legacy[0].LimitsSource != "live" {
		t.Fatal("legacy revisions without plan snapshots fall back to live limits")
	}
}

func TestParseRevisionContent(t *testing.T) {
	raw := []byte(`{"apis":[{"id":"1","name":"on","enabled":true},{"id":"2","name":"off","enabled":false},{"id":"3","name":"draft","enabled":true,"is_draft":true}],
		"plans":[{"id":"p1","name":"gold","rate_limit_per_minute":10}]}`)
	c := parseRevisionContent(raw)
	if !c.HasAPIs || len(c.APIs) != 1 || c.APIs[0].Name != "on" {
		t.Fatalf("only enabled, published APIs are served: %+v", c.APIs)
	}
	if !c.HasPlans || c.Plans["p1"].RateLimitPerMinute != 10 {
		t.Fatalf("plans = %+v", c.Plans)
	}
	if c := parseRevisionContent([]byte(`{"apis":[]}`)); c.HasPlans || c.HasAPIs {
		t.Fatal("a revision without plans/apis must report them missing")
	}
	if c := parseRevisionContent([]byte(`not json`)); c.HasAPIs {
		t.Fatal("corrupt snapshot must not be served")
	}
}

func TestTrafficPolicyValidation(t *testing.T) {
	ok := []string{
		`{}`,
		`{"targets":[{"url":"http://a:1"},{"url":"https://b","weight":3}],"load_balancing":"least_requests"}`,
		`{"retries":{"attempts":3},"circuit_breaker":{"failure_threshold":5},"health_check":{"path":"/healthz"}}`,
	}
	for _, s := range ok {
		var p TrafficPolicy
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			t.Fatal(err)
		}
		p.Normalize()
		if err := p.Validate(); err != nil {
			t.Fatalf("%s: unexpected error %v", s, err)
		}
	}
	bad := map[string]string{
		`{"load_balancing":"sticky"}`:                            "load_balancing",
		`{"targets":[{"url":"ftp://a"}]}`:                        "absolute http",
		`{"targets":[{"url":"http://a"},{"url":"http://a"}]}`:    "duplicated",
		`{"targets":[{"url":"http://a","weight":0}]}`:            "weight > 0",
		`{"retries":{"attempts":9}}`:                             "attempts",
		`{"retries":{"attempts":2,"retry_on_status":[404]}}`:     "5xx",
		`{"circuit_breaker":{"failure_threshold":0}}`:            "failure_threshold",
		`{"health_check":{"path":"healthz"}}`:                    "must start with",
		`{"health_check":{"path":"/h","interval_seconds":1000}}`: "interval_seconds",
	}
	for s, want := range bad {
		var p TrafficPolicy
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			t.Fatal(err)
		}
		p.Normalize()
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want mention of %q", s, err, want)
		}
	}
}

func TestTrafficPolicyDefaults(t *testing.T) {
	p := TrafficPolicy{Retries: &RetryPolicy{Attempts: 2}, CircuitBreaker: &CircuitBreakerPolicy{FailureThreshold: 3}, HealthCheck: &HealthCheckPolicy{Path: "/h"}}
	p.Normalize()
	if p.LoadBalancing != "round_robin" || p.Retries.BackoffMS != 25 || len(p.Retries.RetryOnStatus) != 3 ||
		p.CircuitBreaker.OpenSeconds != 30 || p.HealthCheck.IntervalSeconds != 10 || p.HealthCheck.UnhealthyThreshold != 3 {
		t.Fatalf("defaults not applied: %+v %+v %+v %+v", p, *p.Retries, *p.CircuitBreaker, *p.HealthCheck)
	}
	a := API{UpstreamURL: "http://only"}
	if ts := a.EffectiveTargets(); len(ts) != 1 || ts[0].URL != "http://only" || ts[0].Weight != 1 {
		t.Fatalf("legacy single upstream = %+v", ts)
	}
}
