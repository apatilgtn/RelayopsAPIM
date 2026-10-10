package policy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/relayops/apim/internal/store"
)

type fakeSnap struct {
	keys map[string]store.KeyRecord
	subs map[string]store.SubRecord
}

func (f fakeSnap) LookupKey(h string) (store.KeyRecord, bool) { k, ok := f.keys[h]; return k, ok }
func (f fakeSnap) LookupSub(c, a string) (store.SubRecord, bool) {
	s, ok := f.subs[c+"|"+a]
	return s, ok
}

type fakeLimiter struct {
	rateAllowed  bool
	quotaAllowed bool
	degraded     bool
	lastRateKey  string
	lastLimit    int
	quotaDayHits int
}

func (f *fakeLimiter) CheckRateLimit(_ context.Context, key string, limit int) (bool, int, time.Duration, bool) {
	f.lastRateKey, f.lastLimit = key, limit
	return f.rateAllowed, 3, 0, false
}
func (f *fakeLimiter) CheckDailyQuota(_ context.Context, _ string, _ int) (bool, int, bool) {
	f.quotaDayHits++
	return f.quotaAllowed, 9, f.degraded
}
func (f *fakeLimiter) CheckMonthlyQuota(_ context.Context, _ string, _ int) (bool, int, bool) {
	return f.quotaAllowed, 99, f.degraded
}

type fakeOIDC struct {
	claims map[string]any
	err    error
}

func (f fakeOIDC) VerifyOIDC(_, _, _, _ string, _ time.Time) (map[string]any, error) {
	return f.claims, f.err
}

func baseAPI() *store.API {
	return &store.API{ID: "api-1", Name: "orders", BasePath: "/orders", UpstreamURL: "http://up", AuthType: "api_key", QuotaFailurePolicy: "fail_open"}
}

func req(path string, headers map[string]string) Request {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return Request{Method: http.MethodGet, Path: path, Header: h, QueryParams: url.Values{}, ClientIP: "10.0.0.1"}
}

func snapWith(sub *store.SubRecord, consumerStatus string) fakeSnap {
	f := fakeSnap{
		keys: map[string]store.KeyRecord{store.HashKey("rk_live"): {KeyHash: store.HashKey("rk_live"), KeyID: "k1", ConsumerID: "c1", ConsumerName: "acme", ConsumerStatus: consumerStatus}},
		subs: map[string]store.SubRecord{},
	}
	if sub != nil {
		f.subs["c1|api-1"] = *sub
	}
	return f
}

func intp(i int) *int { return &i }

func TestEvaluateAPIKeyDecisions(t *testing.T) {
	active := &store.SubRecord{ConsumerID: "c1", APIID: "api-1", Status: "approved", Active: true, PlanName: "gold", RateLimitPerMinute: intp(50)}
	cases := []struct {
		name       string
		snap       fakeSnap
		headers    map[string]string
		wantStatus int
		wantReason string
	}{
		{"missing key", snapWith(active, "active"), nil, 401, "missing_api_key"},
		{"unknown key", snapWith(active, "active"), map[string]string{"X-API-Key": "rk_nope"}, 401, "invalid_api_key"},
		{"suspended consumer", snapWith(active, "suspended"), map[string]string{"X-API-Key": "rk_live"}, 403, "consumer_account_suspended"},
		{"not subscribed", snapWith(nil, "active"), map[string]string{"X-API-Key": "rk_live"}, 403, "consumer_not_subscribed"},
		{"pending", snapWith(&store.SubRecord{Status: "pending", Active: true}, "active"), map[string]string{"X-API-Key": "rk_live"}, 403, "subscription_pending_approval"},
		{"rejected", snapWith(&store.SubRecord{Status: "rejected", Active: true}, "active"), map[string]string{"X-API-Key": "rk_live"}, 403, "subscription_rejected"},
		{"disabled", snapWith(&store.SubRecord{Status: "approved", Active: false}, "active"), map[string]string{"X-API-Key": "rk_live"}, 403, "subscription_disabled"},
		{"bearer rk_ key", snapWith(active, "active"), map[string]string{"Authorization": "Bearer rk_live"}, 200, "proxied_successfully"},
		{"allowed", snapWith(active, "active"), map[string]string{"X-API-Key": "rk_live"}, 200, "proxied_successfully"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lim := &fakeLimiter{rateAllowed: true, quotaAllowed: true}
			d := NewEvaluator(nil).Evaluate(context.Background(), req("/orders/1", tc.headers), baseAPI(), tc.snap, lim, false)
			if d.Status != tc.wantStatus || d.Reason != tc.wantReason {
				t.Fatalf("got %d %s, want %d %s", d.Status, d.Reason, tc.wantStatus, tc.wantReason)
			}
			if tc.wantStatus == 200 {
				if !d.Allowed || d.PlanName != "gold" || lim.lastLimit != 50 || lim.lastRateKey != "consumer:c1:api-1" {
					t.Fatalf("plan limit not applied: allowed=%v plan=%q limit=%d key=%q", d.Allowed, d.PlanName, lim.lastLimit, lim.lastRateKey)
				}
			}
		})
	}
}

func TestEvaluateQueryParamKey(t *testing.T) {
	r := req("/orders", nil)
	r.QueryParams.Set("apikey", "rk_live")
	active := &store.SubRecord{Status: "approved", Active: true}
	d := NewEvaluator(nil).Evaluate(context.Background(), r, baseAPI(), snapWith(active, "active"), nil, false)
	if !d.Allowed {
		t.Fatalf("apikey query param should authenticate: %s", d.Reason)
	}
}

func TestEvaluateRateLimitAndQuota(t *testing.T) {
	api := baseAPI()
	api.AuthType = "none"
	api.RateLimitPerMinute = 10
	d := NewEvaluator(nil).Evaluate(context.Background(), req("/orders", nil), api, fakeSnap{}, &fakeLimiter{rateAllowed: false}, false)
	if d.Status != http.StatusTooManyRequests || d.Policy != "rate_limit" {
		t.Fatalf("rate limit: %d %s", d.Status, d.Policy)
	}

	api.RateLimitPerMinute = 0
	api.QuotaPerDay = 100
	d = NewEvaluator(nil).Evaluate(context.Background(), req("/orders", nil), api, fakeSnap{}, &fakeLimiter{rateAllowed: true, quotaAllowed: false}, false)
	if d.Status != http.StatusTooManyRequests || d.Reason != "daily_quota_exceeded" {
		t.Fatalf("quota exhausted must block regardless of failure policy: %d %s", d.Status, d.Reason)
	}

	// Quota backend outage: fail_open allows, fail_closed blocks with 503.
	d = NewEvaluator(nil).Evaluate(context.Background(), req("/orders", nil), api, fakeSnap{}, &fakeLimiter{rateAllowed: true, quotaAllowed: true, degraded: true}, false)
	if !d.Allowed || !d.Degraded {
		t.Fatalf("fail_open outage should allow and mark degraded: %+v", d)
	}
	api.QuotaFailurePolicy = "fail_closed"
	d = NewEvaluator(nil).Evaluate(context.Background(), req("/orders", nil), api, fakeSnap{}, &fakeLimiter{rateAllowed: true, quotaAllowed: false, degraded: true}, false)
	if d.Status != http.StatusServiceUnavailable || d.Reason != "quota_backend_unavailable" {
		t.Fatalf("fail_closed outage: %d %s", d.Status, d.Reason)
	}
}

func TestEvaluateRoutingAndCORS(t *testing.T) {
	if d := NewEvaluator(nil).Evaluate(context.Background(), req("/x", nil), nil, fakeSnap{}, nil, false); d.Status != 404 {
		t.Fatalf("nil api: %d", d.Status)
	}
	if d := NewEvaluator(nil).Evaluate(context.Background(), req("/ordersX", nil), baseAPI(), fakeSnap{}, nil, false); d.Reason != "route_prefix_mismatch" {
		t.Fatalf("prefix mismatch: %s", d.Reason)
	}
	api := baseAPI()
	api.CORSEnabled = true
	r := req("/orders", map[string]string{"Access-Control-Request-Method": "POST"})
	r.Method = http.MethodOptions
	if d := NewEvaluator(nil).Evaluate(context.Background(), r, api, fakeSnap{}, nil, false); !d.Allowed || d.Status != 204 {
		t.Fatalf("CORS preflight must bypass auth: %+v", d)
	}
}

func sign(secret string, claims map[string]any) string {
	enc := base64.RawURLEncoding
	h := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	p, _ := json.Marshal(claims)
	unsigned := h + "." + enc.EncodeToString(p)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(unsigned))
	return unsigned + "." + enc.EncodeToString(m.Sum(nil))
}

func TestEvaluateJWT(t *testing.T) {
	api := baseAPI()
	api.AuthType, api.JWTSecret = "jwt", "0123456789abcdef"
	ev := NewEvaluator(nil)
	good := sign(api.JWTSecret, map[string]any{"sub": "u1", "exp": time.Now().Add(time.Hour).Unix()})
	d := ev.Evaluate(context.Background(), req("/orders", map[string]string{"Authorization": "Bearer " + good}), api, fakeSnap{}, &fakeLimiter{rateAllowed: true}, false)
	if !d.Allowed || d.ConsumerName != "u1" {
		t.Fatalf("valid jwt: %+v", d)
	}
	for name, tok := range map[string]string{
		"wrong secret": sign("another-secret-value", map[string]any{"sub": "u1"}),
		"expired":      sign(api.JWTSecret, map[string]any{"sub": "u1", "exp": time.Now().Add(-time.Minute).Unix()}),
		"malformed":    "not.a.jwt.token",
	} {
		d := ev.Evaluate(context.Background(), req("/orders", map[string]string{"Authorization": "Bearer " + tok}), api, fakeSnap{}, nil, false)
		if d.Status != 401 || d.Reason != "invalid_jwt" {
			t.Fatalf("%s: %d %s", name, d.Status, d.Reason)
		}
	}
	if d := ev.Evaluate(context.Background(), req("/orders", nil), api, fakeSnap{}, nil, false); d.Reason != "missing_jwt_token" {
		t.Fatalf("missing: %s", d.Reason)
	}
}

func TestEvaluateOIDCFailsClosed(t *testing.T) {
	api := baseAPI()
	api.AuthType = "oidc"
	h := map[string]string{"Authorization": "Bearer abc.def.ghi"}

	d := NewEvaluator(fakeOIDC{}).Evaluate(context.Background(), req("/orders", h), api, fakeSnap{}, nil, false)
	if d.Allowed || d.Reason != "oidc_provider_unconfigured" {
		t.Fatalf("no JWKS URL must fail closed: %+v", d)
	}
	api.JWKSURL = "https://idp/jwks"
	d = NewEvaluator(fakeOIDC{err: errors.New("bad signature")}).Evaluate(context.Background(), req("/orders", h), api, fakeSnap{}, nil, false)
	if d.Status != 401 || d.Reason != "invalid_oidc_token" {
		t.Fatalf("bad token: %d %s", d.Status, d.Reason)
	}
	d = NewEvaluator(fakeOIDC{claims: map[string]any{"sub": "svc-a"}}).Evaluate(context.Background(), req("/orders", h), api, fakeSnap{}, &fakeLimiter{rateAllowed: true}, false)
	if !d.Allowed || d.ConsumerName != "svc-a" {
		t.Fatalf("good token: %+v", d)
	}
}

func TestParseTraceUnverifiedClaims(t *testing.T) {
	tok := sign("x-secret-xxxxxxxxx", map[string]any{"sub": "s"})
	c, err := ParseUnverifiedClaims(tok)
	if err != nil || c["sub"] != "s" {
		t.Fatalf("claims=%v err=%v", c, err)
	}
	if _, err := ParseUnverifiedClaims("bad"); err == nil {
		t.Fatal("malformed token must error")
	}
}

func TestVerifyHS256RequiresExpAndHonoursNbf(t *testing.T) {
	secret := "0123456789abcdef"
	now := time.Now()
	if _, err := VerifyHS256(sign(secret, map[string]any{"sub": "u"}), secret, now); err == nil || err.Error() != "invalid jwt: missing required exp claim" {
		t.Fatalf("token without exp: err = %v", err)
	}
	future := sign(secret, map[string]any{"sub": "u", "exp": now.Add(time.Hour).Unix(), "nbf": now.Add(time.Minute).Unix()})
	if _, err := VerifyHS256(future, secret, now); err == nil {
		t.Fatal("token used before nbf must be rejected")
	}
	if _, err := VerifyHS256(future, secret, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("token after nbf: %v", err)
	}
}

func TestPendingSubscriptionRecordsAuthenticationSuccess(t *testing.T) {
	d := NewEvaluator(nil).Evaluate(context.Background(), req("/orders", map[string]string{"X-API-Key": "rk_live"}), baseAPI(),
		snapWith(&store.SubRecord{Status: "pending", Active: true}, "active"), nil, false)
	auth, _ := d.Evaluations["auth"].(map[string]any)
	if d.Reason != "subscription_pending_approval" || auth["result"] != "allow" {
		t.Fatalf("reason=%s auth=%v: a valid key must be recorded as authenticated even when the subscription blocks", d.Reason, auth)
	}
}

func TestMTLSPolicyEvaluation(t *testing.T) {
	mtlsAPI := &store.API{
		ID:          "api-mtls",
		Name:        "mtls-secure",
		BasePath:    "/secure",
		UpstreamURL: "http://up",
		AuthType:    "mtls",
	}

	ev := NewEvaluator(nil)
	ctx := context.Background()

	// 1. Missing certificate -> 401
	rNoCert := Request{Method: "GET", Path: "/secure", Header: http.Header{}, ClientIP: "1.2.3.4"}
	d1 := ev.Evaluate(ctx, rNoCert, mtlsAPI, fakeSnap{}, nil, false)
	if d1.Status != http.StatusUnauthorized || d1.Reason != "missing_client_certificate" {
		t.Fatalf("expected 401 missing_client_certificate, got status=%d reason=%s", d1.Status, d1.Reason)
	}

	// 2. Untrusted fingerprint -> 403
	hdrUntrusted := http.Header{}
	hdrUntrusted.Set("X-Client-Cert-Fingerprint", "1122334455667788")
	rUntrusted := Request{Method: "GET", Path: "/secure", Header: hdrUntrusted, ClientIP: "1.2.3.4"}
	d2 := ev.Evaluate(ctx, rUntrusted, mtlsAPI, fakeSnap{}, nil, false)
	if d2.Status != http.StatusForbidden || d2.Reason != "untrusted_client_certificate" {
		t.Fatalf("expected 403 untrusted_client_certificate, got status=%d reason=%s", d2.Status, d2.Reason)
	}

	// 3. Registered consumer fingerprint -> 200
	fp := "AABBCCDDEEFF0011"
	snap := fakeSnap{
		keys: map[string]store.KeyRecord{
			store.HashKey(fp): {
				ConsumerID:     "c-partner",
				ConsumerName:   "Partner Org",
				ConsumerStatus: "active",
				KeyID:          "key-cert-1",
			},
		},
		subs: map[string]store.SubRecord{
			"c-partner|api-mtls": {
				Status:   "approved",
				Active:   true,
				PlanName: "enterprise",
			},
		},
	}
	hdrTrusted := http.Header{}
	hdrTrusted.Set("X-Client-Cert-Fingerprint", fp)
	rTrusted := Request{Method: "GET", Path: "/secure", Header: hdrTrusted, ClientIP: "1.2.3.4"}
	d3 := ev.Evaluate(ctx, rTrusted, mtlsAPI, snap, nil, false)
	if !d3.Allowed || d3.ConsumerID != "c-partner" {
		t.Fatalf("expected allowed mTLS for registered consumer, got: %+v", d3)
	}

	// 4. Allowed-Client-CN override on API -> 200
	mtlsWithCN := &store.API{
		ID:          "api-mtls-cn",
		Name:        "mtls-cn-secure",
		BasePath:    "/secure-cn",
		UpstreamURL: "http://up",
		AuthType:    "mtls",
		RequestHeaders: map[string]string{
			"Allowed-Client-CN": "trusted-client.partner.com",
		},
	}
	hdrCN := http.Header{}
	hdrCN.Set("X-Client-Cert-Fingerprint", "9988776655443322")
	hdrCN.Set("X-Client-Cert-CN", "trusted-client.partner.com")
	rCN := Request{Method: "GET", Path: "/secure-cn", Header: hdrCN, ClientIP: "1.2.3.4"}
	d4 := ev.Evaluate(ctx, rCN, mtlsWithCN, fakeSnap{}, nil, false)
	if !d4.Allowed || d4.ConsumerName != "trusted-client.partner.com" {
		t.Fatalf("expected allowed mTLS for authorized CN, got: %+v", d4)
	}
}

