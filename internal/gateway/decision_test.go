package gateway

import (
	"testing"

	"github.com/relayops/apim/internal/policy"
)

func TestDecisionStatuses(t *testing.T) {
	ev := func(auth, sub string) map[string]any {
		m := map[string]any{}
		if auth != "" {
			m["auth"] = map[string]any{"result": auth}
		}
		if sub != "" {
			m["subscription"] = map[string]any{"result": sub}
		}
		return m
	}
	cases := []struct {
		name          string
		dec           policy.Decision
		auth, sub, rl string
	}{
		{"proxied with key", policy.Decision{Allowed: true, Reason: "proxied_successfully", ConsumerStatus: "active", Evaluations: ev("allow", "allow")}, "ok", "active", "ok"},
		{"public api", policy.Decision{Allowed: true, Reason: "proxied_successfully", Evaluations: ev("allow", "bypassed_public")}, "ok", "not_required", "ok"},
		{"jwt api", policy.Decision{Allowed: true, Reason: "proxied_successfully", Evaluations: ev("allow", "")}, "ok", "not_required", "ok"},
		{"pending", policy.Decision{Reason: "subscription_pending_approval", ConsumerStatus: "active", Evaluations: ev("allow", "pending")}, "ok", "pending_approval", "ok"},
		{"missing key", policy.Decision{Reason: "missing_api_key", Evaluations: ev("missing", "")}, "missing_api_key", "none", "ok"},
		{"suspended", policy.Decision{Reason: "consumer_account_suspended", ConsumerStatus: "suspended", Evaluations: ev("consumer_suspended", "")}, "consumer_suspended", "none", "ok"},
		{"not subscribed", policy.Decision{Reason: "consumer_not_subscribed", Evaluations: ev("", "not_subscribed")}, "none", "not_subscribed", "ok"},
		{"rate limited", policy.Decision{Reason: "rate_limit_exceeded", Evaluations: ev("allow", "allow")}, "ok", "active", "rate_limited"},
		{"quota", policy.Decision{Reason: "daily_quota_exceeded", Evaluations: ev("allow", "allow")}, "ok", "active", "quota_exceeded"},
	}
	for _, c := range cases {
		a, s, r := decisionStatuses(c.dec)
		if a != c.auth || s != c.sub || r != c.rl {
			t.Errorf("%s: got %s/%s/%s, want %s/%s/%s", c.name, a, s, r, c.auth, c.sub, c.rl)
		}
	}
}
