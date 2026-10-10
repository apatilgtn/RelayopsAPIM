package gateway

import "github.com/relayops/apim/internal/policy"

// Decision-trail values recorded on every request log.
//
//	auth_status:         ok | <failure reason> (missing_api_key, invalid_api_key, invalid_jwt, ...) |
//	                     consumer_<status> (suspended consumer) | none (not evaluated)
//	subscription_status: active | not_required (public, JWT, OIDC) | pending_approval | rejected |
//	                     disabled | not_subscribed | none (not evaluated)
//	rate_limit_status:   ok | rate_limited | quota_exceeded | quota_backend_unavailable
func decisionStatuses(dec policy.Decision) (auth, sub, rate string) {
	auth, sub, rate = "none", "none", "ok"

	if ev, ok := dec.Evaluations["auth"].(map[string]any); ok {
		switch {
		case ev["result"] == "allow":
			auth = "ok"
		case dec.ConsumerStatus != "" && dec.ConsumerStatus != "active":
			auth = "consumer_" + dec.ConsumerStatus
		default:
			auth = dec.Reason
		}
	}

	if ev, ok := dec.Evaluations["subscription"].(map[string]any); ok {
		switch ev["result"] {
		case "allow":
			sub = "active"
		case "bypassed_public":
			sub = "not_required"
		case "pending":
			sub = "pending_approval"
		case "rejected", "disabled", "not_subscribed":
			sub = ev["result"].(string)
		}
	} else if auth == "ok" {
		sub = "not_required" // JWT / OIDC APIs authorize by token, not subscription
	}

	switch dec.Reason {
	case "rate_limit_exceeded":
		rate = "rate_limited"
	case "daily_quota_exceeded", "monthly_quota_exceeded":
		rate = "quota_exceeded"
	case "quota_backend_unavailable":
		rate = "quota_backend_unavailable"
	}
	return auth, sub, rate
}
