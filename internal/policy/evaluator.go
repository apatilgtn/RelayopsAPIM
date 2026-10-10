package policy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/relayops/apim/internal/store"
)

// Request contains the normalized request properties needed for policy evaluation.
type Request struct {
	Method      string
	Path        string
	Header      http.Header
	QueryParams url.Values
	ClientIP    string
	Body        []byte
	// PluginConfig is the API's settings for the WASM plugin being run.
	PluginConfig json.RawMessage
}

// Decision represents the final outcome of the gateway policy evaluation pipeline.
type Decision struct {
	Allowed             bool           `json:"allowed"`
	Status              int            `json:"status"`
	Policy              string         `json:"policy"` // "routing", "cors", "authentication", "subscription", "rate_limit", "quota", "proxy"
	Reason              string         `json:"reason"`
	Detail              string         `json:"detail,omitempty"`
	ConsumerID          string         `json:"consumer_id,omitempty"`
	ConsumerName        string         `json:"consumer_name,omitempty"`
	ConsumerStatus      string         `json:"consumer_status,omitempty"`
	KeyID               string         `json:"key_id,omitempty"`
	PlanName            string         `json:"plan_name,omitempty"`
	RateLimitLimit      int            `json:"rate_limit_limit,omitempty"`
	RateLimitRemaining  int            `json:"rate_limit_remaining,omitempty"`
	QuotaDayLimit       int            `json:"quota_day_limit,omitempty"`
	QuotaDayRemaining   int            `json:"quota_day_remaining,omitempty"`
	QuotaMonthLimit     int            `json:"quota_month_limit,omitempty"`
	QuotaMonthRemaining int            `json:"quota_month_remaining,omitempty"`
	Degraded            bool           `json:"degraded,omitempty"`
	Evaluations         map[string]any `json:"evaluations,omitempty"`
}

// SnapshotLookup abstracts key and subscription state lookup.
type SnapshotLookup interface {
	LookupKey(hash string) (store.KeyRecord, bool)
	LookupSub(consumerID, apiID string) (store.SubRecord, bool)
}

// LimiterLookup abstracts rate limiting and quota enforcement.
type LimiterLookup interface {
	CheckRateLimit(ctx context.Context, key string, limitPerMinute int) (allowed bool, remaining int, resetAfter time.Duration, degraded bool)
	CheckDailyQuota(ctx context.Context, key string, quotaDay int) (allowed bool, remaining int, degraded bool)
	CheckMonthlyQuota(ctx context.Context, key string, quotaMonth int) (allowed bool, remaining int, degraded bool)
}

// OIDCVerifier abstracts cryptographic OIDC token validation.
type OIDCVerifier interface {
	VerifyOIDC(rawToken, jwksURL, expectedIssuer, expectedAudience string, now time.Time) (map[string]any, error)
}

// Evaluator executes the policy pipeline identically for live traffic and replay preview.
type Evaluator struct {
	OIDC OIDCVerifier
}

func NewEvaluator(oidc OIDCVerifier) *Evaluator {
	return &Evaluator{OIDC: oidc}
}

// Evaluate runs the unified policy pipeline against an API configuration.
func (e *Evaluator) Evaluate(ctx context.Context, req Request, api *store.API, snap SnapshotLookup, limiter LimiterLookup, dryRun bool) Decision {
	dec := Decision{
		Allowed:     false,
		Status:      http.StatusOK,
		Evaluations: make(map[string]any),
	}

	if api == nil {
		dec.Status = http.StatusNotFound
		dec.Policy = "routing"
		dec.Reason = "no_route_matched"
		dec.Detail = "no API configured for path " + req.Path
		return dec
	}

	// 1. ROUTING POLICY
	basePath := api.BasePath
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	if req.Path != basePath && !strings.HasPrefix(req.Path, strings.TrimRight(basePath, "/")+"/") {
		dec.Status = http.StatusNotFound
		dec.Policy = "routing"
		dec.Reason = "route_prefix_mismatch"
		dec.Detail = fmt.Sprintf("Path '%s' does not match API base path '%s'", req.Path, api.BasePath)
		dec.Evaluations["routing"] = map[string]any{"matched": false, "base_path": api.BasePath}
		return dec
	}
	dec.Evaluations["routing"] = map[string]any{"matched": true, "base_path": api.BasePath, "upstream": api.UpstreamURL}

	// 2. CORS PREFLIGHT POLICY
	if api.CORSEnabled && req.Method == http.MethodOptions && req.Header.Get("Access-Control-Request-Method") != "" {
		dec.Allowed = true
		dec.Status = http.StatusNoContent
		dec.Policy = "cors"
		dec.Reason = "cors_preflight_allowed"
		dec.Evaluations["cors"] = map[string]any{"preflight": true, "result": "allow"}
		return dec
	}

	// 3. AUTHENTICATION & SUBSCRIPTION POLICIES
	identity := "ip:" + req.ClientIP
	limit := api.RateLimitPerMinute
	quotaDay := api.QuotaPerDay
	quotaMonth := api.QuotaPerMonth

	switch api.AuthType {
	case "none", "":
		dec.Policy = "authentication"
		dec.Reason = "bypassed_public"
		dec.Evaluations["auth"] = publicAuthEval
		dec.Evaluations["subscription"] = publicSubscriptionEval

	case "api_key":
		rawKey := req.Header.Get("X-API-Key")
		if rawKey == "" && req.QueryParams != nil {
			rawKey = req.QueryParams.Get("apikey")
		}
		if rawKey == "" {
			authz := req.Header.Get("Authorization")
			if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
				tok := strings.TrimSpace(authz[7:])
				if strings.HasPrefix(tok, "rk_") {
					rawKey = tok
				}
			}
		}

		if rawKey == "" {
			dec.Status = http.StatusUnauthorized
			dec.Policy = "authentication"
			dec.Reason = "missing_api_key"
			dec.Detail = "provide an API key via the X-API-Key header or Bearer token"
			dec.Evaluations["auth"] = map[string]any{"type": "api_key", "result": "missing"}
			return dec
		}

		keyHash := store.HashKey(rawKey)
		keyRec, ok := snap.LookupKey(keyHash)
		if !ok {
			dec.Status = http.StatusUnauthorized
			dec.Policy = "authentication"
			dec.Reason = "invalid_api_key"
			dec.Detail = "API key is invalid or revoked"
			dec.Evaluations["auth"] = map[string]any{"type": "api_key", "result": "invalid"}
			return dec
		}

		dec.ConsumerID = keyRec.ConsumerID
		dec.ConsumerName = keyRec.ConsumerName
		dec.ConsumerStatus = keyRec.ConsumerStatus
		dec.KeyID = keyRec.KeyID

		if keyRec.ConsumerStatus != "" && keyRec.ConsumerStatus != "active" {
			dec.Status = http.StatusForbidden
			dec.Policy = "authentication"
			dec.Reason = "consumer_account_" + keyRec.ConsumerStatus
			dec.Detail = "consumer account is " + keyRec.ConsumerStatus
			dec.Evaluations["auth"] = map[string]any{"type": "api_key", "result": "consumer_" + keyRec.ConsumerStatus}
			return dec
		}

		// The key is valid and the consumer active: authentication has passed,
		// whatever the subscription check decides next.
		dec.Evaluations["auth"] = map[string]any{"type": "api_key", "result": "allow", "consumer_id": keyRec.ConsumerID}

		// Subscription check
		sub, hasSub := snap.LookupSub(keyRec.ConsumerID, api.ID)
		if !hasSub {
			dec.Status = http.StatusForbidden
			dec.Policy = "subscription"
			dec.Reason = "consumer_not_subscribed"
			dec.Detail = "consumer is not subscribed to this API"
			dec.Evaluations["subscription"] = map[string]any{"result": "not_subscribed"}
			return dec
		}
		if sub.Status == "pending" {
			dec.Status = http.StatusForbidden
			dec.Policy = "subscription"
			dec.Reason = "subscription_pending_approval"
			dec.Detail = "subscription is pending administrator review"
			dec.Evaluations["subscription"] = map[string]any{"result": "pending"}
			return dec
		}
		if sub.Status == "rejected" {
			dec.Status = http.StatusForbidden
			dec.Policy = "subscription"
			dec.Reason = "subscription_rejected"
			dec.Detail = "subscription was rejected by administrator"
			dec.Evaluations["subscription"] = map[string]any{"result": "rejected"}
			return dec
		}
		if !sub.Active {
			dec.Status = http.StatusForbidden
			dec.Policy = "subscription"
			dec.Reason = "subscription_disabled"
			dec.Detail = "subscription is currently disabled"
			dec.Evaluations["subscription"] = map[string]any{"result": "disabled"}
			return dec
		}

		dec.PlanName = sub.PlanName
		if sub.RateLimitPerMinute != nil {
			limit = *sub.RateLimitPerMinute
		}
		if sub.QuotaPerDay != nil {
			quotaDay = *sub.QuotaPerDay
		}
		if sub.QuotaPerMonth != nil {
			quotaMonth = *sub.QuotaPerMonth
		}
		identity = "consumer:" + keyRec.ConsumerID
		dec.Evaluations["subscription"] = map[string]any{"result": "allow", "plan": sub.PlanName}

	case "jwt":
		authz := req.Header.Get("Authorization")
		token := ""
		if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
			token = strings.TrimSpace(authz[7:])
		}
		if token == "" {
			dec.Status = http.StatusUnauthorized
			dec.Policy = "authentication"
			dec.Reason = "missing_jwt_token"
			dec.Detail = "provide a bearer token in Authorization header"
			dec.Evaluations["auth"] = map[string]any{"type": "jwt", "result": "missing"}
			return dec
		}

		claims, err := VerifyHS256(token, api.JWTSecret, time.Now())
		if err != nil {
			dec.Status = http.StatusUnauthorized
			dec.Policy = "authentication"
			dec.Reason = "invalid_jwt"
			dec.Detail = err.Error()
			dec.Evaluations["auth"] = map[string]any{"type": "jwt", "result": "invalid", "error": err.Error()}
			return dec
		}
		dec.Evaluations["auth"] = map[string]any{"type": "jwt", "result": "allow"}
		if sub, ok := claims["sub"].(string); ok && sub != "" {
			dec.ConsumerName = sub
			identity = "sub:" + sub
		}

	case "oidc", "oauth2_jwks":
		authz := req.Header.Get("Authorization")
		token := ""
		if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
			token = strings.TrimSpace(authz[7:])
		}
		if token == "" {
			dec.Status = http.StatusUnauthorized
			dec.Policy = "authentication"
			dec.Reason = "missing_oidc_token"
			dec.Detail = "provide an OIDC bearer token in Authorization header"
			dec.Evaluations["auth"] = map[string]any{"type": "oidc", "result": "missing"}
			return dec
		}

		if e.OIDC == nil || api.JWKSURL == "" {
			// Fail-closed: Never fall back to unverified claims when JWKS endpoint is unconfigured
			dec.Status = http.StatusBadGateway
			dec.Policy = "authentication"
			dec.Reason = "oidc_provider_unconfigured"
			dec.Detail = "OIDC JWKS provider endpoint is unconfigured; failing closed"
			dec.Evaluations["auth"] = map[string]any{"type": "oidc", "result": "fail_closed_unconfigured"}
			return dec
		}

		claims, err := e.OIDC.VerifyOIDC(token, api.JWKSURL, api.OIDCIssuer, api.OIDCAudience, time.Now())
		if err != nil {
			dec.Status = http.StatusUnauthorized
			dec.Policy = "authentication"
			dec.Reason = "invalid_oidc_token"
			dec.Detail = err.Error()
			dec.Evaluations["auth"] = map[string]any{"type": "oidc", "result": "invalid", "error": err.Error()}
			return dec
		}
		dec.Evaluations["auth"] = map[string]any{"type": "oidc", "result": "allow"}
		if sub, ok := claims["sub"].(string); ok && sub != "" {
			dec.ConsumerName = sub
			identity = "sub:" + sub
		}

	case "mtls":
		fp := req.Header.Get("X-Client-Cert-Fingerprint")
		if fp == "" {
			fp = req.Header.Get("X-SSL-Client-SHA256")
		}
		if fp == "" {
			dec.Status = http.StatusUnauthorized
			dec.Policy = "authentication"
			dec.Reason = "missing_client_certificate"
			dec.Detail = "mutual TLS required: no client certificate presented"
			dec.Evaluations["auth"] = map[string]any{"type": "mtls", "result": "missing"}
			return dec
		}

		cleanFP := strings.ToUpper(strings.ReplaceAll(fp, ":", ""))
		keyHash := store.HashKey("mtls:" + cleanFP)
		keyRec, ok := snap.LookupKey(keyHash)
		if !ok {
			keyRec, ok = snap.LookupKey(store.HashKey(cleanFP))
		}

		cn := req.Header.Get("X-Client-Cert-CN")
		if cn == "" {
			cn = req.Header.Get("X-SSL-Client-CN")
		}

		if !ok {
			allowed := false
			if api.RequestHeaders != nil {
				if allowedCN, exists := api.RequestHeaders["Allowed-Client-CN"]; exists && allowedCN != "" {
					if allowedCN == "*" || strings.EqualFold(allowedCN, cn) {
						allowed = true
					}
				}
				if allowedFP, exists := api.RequestHeaders["Allowed-Client-Fingerprint"]; exists && allowedFP != "" {
					if strings.EqualFold(strings.ReplaceAll(allowedFP, ":", ""), cleanFP) {
						allowed = true
					}
				}
			}

			if !allowed {
				dec.Status = http.StatusForbidden
				dec.Policy = "authentication"
				dec.Reason = "untrusted_client_certificate"
				dec.Detail = "client certificate fingerprint is not registered or authorized"
				dec.Evaluations["auth"] = map[string]any{"type": "mtls", "result": "untrusted", "fingerprint": cleanFP}
				return dec
			}

			dec.ConsumerID = "cert:" + cleanFP
			dec.ConsumerName = cn
			if dec.ConsumerName == "" {
				dec.ConsumerName = "ClientCert-" + cleanFP[:8]
			}
			identity = "cert:" + cleanFP
			dec.Evaluations["auth"] = map[string]any{"type": "mtls", "result": "allow", "fingerprint": cleanFP, "cn": cn}
			dec.Evaluations["subscription"] = map[string]any{"result": "bypassed_mtls_policy"}
			break
		}

		dec.ConsumerID = keyRec.ConsumerID
		dec.ConsumerName = keyRec.ConsumerName
		dec.ConsumerStatus = keyRec.ConsumerStatus
		dec.KeyID = keyRec.KeyID

		if keyRec.ConsumerStatus != "" && keyRec.ConsumerStatus != "active" {
			dec.Status = http.StatusForbidden
			dec.Policy = "authentication"
			dec.Reason = "consumer_account_" + keyRec.ConsumerStatus
			dec.Detail = "consumer account is " + keyRec.ConsumerStatus
			dec.Evaluations["auth"] = map[string]any{"type": "mtls", "result": "consumer_" + keyRec.ConsumerStatus}
			return dec
		}

		dec.Evaluations["auth"] = map[string]any{"type": "mtls", "result": "allow", "consumer_id": keyRec.ConsumerID, "fingerprint": cleanFP}

		sub, hasSub := snap.LookupSub(keyRec.ConsumerID, api.ID)
		if !hasSub {
			dec.Status = http.StatusForbidden
			dec.Policy = "subscription"
			dec.Reason = "consumer_not_subscribed"
			dec.Detail = "consumer is not subscribed to this API"
			dec.Evaluations["subscription"] = map[string]any{"result": "not_subscribed"}
			return dec
		}
		if sub.Status == "pending" {
			dec.Status = http.StatusForbidden
			dec.Policy = "subscription"
			dec.Reason = "subscription_pending_approval"
			dec.Detail = "subscription is pending administrator review"
			dec.Evaluations["subscription"] = map[string]any{"result": "pending"}
			return dec
		}
		if sub.Status == "rejected" {
			dec.Status = http.StatusForbidden
			dec.Policy = "subscription"
			dec.Reason = "subscription_rejected"
			dec.Detail = "subscription was rejected by administrator"
			dec.Evaluations["subscription"] = map[string]any{"result": "rejected"}
			return dec
		}
		if !sub.Active {
			dec.Status = http.StatusForbidden
			dec.Policy = "subscription"
			dec.Reason = "subscription_disabled"
			dec.Detail = "subscription is currently disabled"
			dec.Evaluations["subscription"] = map[string]any{"result": "disabled"}
			return dec
		}

		dec.PlanName = sub.PlanName
		if sub.RateLimitPerMinute != nil {
			limit = *sub.RateLimitPerMinute
		}
		if sub.QuotaPerDay != nil {
			quotaDay = *sub.QuotaPerDay
		}
		if sub.QuotaPerMonth != nil {
			quotaMonth = *sub.QuotaPerMonth
		}
		identity = "consumer:" + keyRec.ConsumerID
		dec.Evaluations["subscription"] = map[string]any{"result": "allow", "plan": sub.PlanName}
	}

	// 4. RATE LIMITING POLICY
	if limiter != nil && limit > 0 {
		rateKey := identity + ":" + api.ID
		allowed, rem, _, degraded := limiter.CheckRateLimit(ctx, rateKey, limit)
		dec.RateLimitLimit = limit
		dec.RateLimitRemaining = rem
		dec.Degraded = dec.Degraded || degraded

		if !allowed {
			dec.Status = http.StatusTooManyRequests
			dec.Policy = "rate_limit"
			dec.Reason = "rate_limit_exceeded"
			dec.Detail = fmt.Sprintf("rate limit of %d requests/minute exceeded", limit)
			dec.Evaluations["rate_limit"] = map[string]any{"allowed": false, "limit": limit, "remaining": rem}
			return dec
		}
		dec.Evaluations["rate_limit"] = map[string]any{"allowed": true, "limit": limit, "remaining": rem}
	}

	// 5. DAILY QUOTA POLICY
	if limiter != nil && quotaDay > 0 {
		quotaKey := identity + ":" + api.ID
		allowed, rem, degraded := limiter.CheckDailyQuota(ctx, quotaKey, quotaDay)
		dec.QuotaDayLimit = quotaDay
		dec.QuotaDayRemaining = rem
		dec.Degraded = dec.Degraded || degraded

		if degraded {
			// Backend failure / outage: obey configured QuotaFailurePolicy
			if api.QuotaFailurePolicy == "fail_open" {
				dec.Evaluations["quota_day"] = map[string]any{"allowed": true, "degraded": true, "policy": "fail_open"}
			} else {
				dec.Status = http.StatusServiceUnavailable
				dec.Policy = "quota"
				dec.Reason = "quota_backend_unavailable"
				dec.Detail = "quota backend is unreachable and policy is fail_closed"
				dec.Evaluations["quota_day"] = map[string]any{"allowed": false, "degraded": true, "policy": "fail_closed"}
				return dec
			}
		} else if !allowed {
			// Legitimate quota exhaustion: ALWAYS block regardless of QuotaFailurePolicy
			dec.Status = http.StatusTooManyRequests
			dec.Policy = "quota"
			dec.Reason = "daily_quota_exceeded"
			dec.Detail = fmt.Sprintf("daily quota of %d requests reached", quotaDay)
			dec.Evaluations["quota_day"] = map[string]any{"allowed": false, "quota": quotaDay, "remaining": rem}
			return dec
		} else {
			dec.Evaluations["quota_day"] = map[string]any{"allowed": true, "remaining": rem}
		}
	}

	// 6. MONTHLY QUOTA POLICY
	if limiter != nil && quotaMonth > 0 {
		quotaKey := identity + ":" + api.ID
		allowed, rem, degraded := limiter.CheckMonthlyQuota(ctx, quotaKey, quotaMonth)
		dec.QuotaMonthLimit = quotaMonth
		dec.QuotaMonthRemaining = rem
		dec.Degraded = dec.Degraded || degraded

		if degraded {
			// Backend failure / outage: obey configured QuotaFailurePolicy
			if api.QuotaFailurePolicy == "fail_open" {
				dec.Evaluations["quota_month"] = map[string]any{"allowed": true, "degraded": true, "policy": "fail_open"}
			} else {
				dec.Status = http.StatusServiceUnavailable
				dec.Policy = "quota"
				dec.Reason = "quota_backend_unavailable"
				dec.Detail = "quota backend is unreachable and policy is fail_closed"
				dec.Evaluations["quota_month"] = map[string]any{"allowed": false, "degraded": true, "policy": "fail_closed"}
				return dec
			}
		} else if !allowed {
			// Legitimate quota exhaustion: ALWAYS block regardless of QuotaFailurePolicy
			dec.Status = http.StatusTooManyRequests
			dec.Policy = "quota"
			dec.Reason = "monthly_quota_exceeded"
			dec.Detail = fmt.Sprintf("monthly quota of %d requests reached", quotaMonth)
			dec.Evaluations["quota_month"] = map[string]any{"allowed": false, "quota": quotaMonth, "remaining": rem}
			return dec
		} else {
			dec.Evaluations["quota_month"] = map[string]any{"allowed": true, "remaining": rem}
		}
	}

	// Request successfully cleared all gateway policies
	dec.Allowed = true
	dec.Status = http.StatusOK
	dec.Policy = "proxy"
	dec.Reason = "proxied_successfully"
	return dec
}

// ---------------------------------------------------------------------------
// Helpers for cryptographic JWT validation
// ---------------------------------------------------------------------------

func VerifyHS256(rawToken, secret string, now time.Time) (map[string]any, error) {
	if secret == "" {
		return nil, errors.New("jwt_secret is not configured on this API")
	}
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid jwt: expected 3 dot-separated parts")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	expected := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		got, err = base64.URLEncoding.DecodeString(parts[2])
	}
	if err != nil || !hmac.Equal(got, expected) {
		return nil, errors.New("invalid jwt: signature mismatch")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payloadBytes, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return nil, errors.New("invalid jwt: payload decode failed")
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, errors.New("invalid jwt: payload is not valid JSON")
	}
	exp, ok := claims["exp"].(float64)
	if !ok || exp <= 0 {
		return nil, errors.New("invalid jwt: missing required exp claim")
	}
	if now.Unix() > int64(exp) {
		return nil, errors.New("invalid jwt: token has expired")
	}
	if nbf, ok := claims["nbf"].(float64); ok && now.Unix() < int64(nbf) {
		return nil, errors.New("invalid jwt: token is not valid yet (nbf)")
	}
	return claims, nil
}

func ParseUnverifiedClaims(rawToken string) (map[string]any, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed jwt")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payloadBytes, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// Shared decision-trail entries for public APIs. They are read-only: callers
// only add top-level keys to Evaluations, never modify these maps.
var (
	publicAuthEval         = map[string]any{"type": "none", "result": "allow"}
	publicSubscriptionEval = map[string]any{"result": "bypassed_public"}
)
