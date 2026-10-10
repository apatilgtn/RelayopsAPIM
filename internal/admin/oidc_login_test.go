package admin

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIdP is a minimal OpenID Connect provider: discovery, JWKS, and a token
// endpoint that enforces PKCE and issues RS256 ID tokens.
type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]issued // code -> what to issue
	issuer string
}

type issued struct {
	challenge string
	claims    map[string]any
}

func newFakeIdP(t *testing.T) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, codes: map[string]issued{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer": f.issuer, "authorization_endpoint": f.issuer + "/authorize",
			"token_endpoint": f.issuer + "/token", "jwks_uri": f.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		is, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != is.challenge || r.PostForm.Get("client_secret") != "s3cret" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id_token": f.sign(is.claims), "token_type": "Bearer"})
	})
	f.srv = httptest.NewServer(mux)
	f.issuer = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) sign(claims map[string]any) string {
	enc := base64.RawURLEncoding.EncodeToString
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	p, _ := json.Marshal(claims)
	unsigned := enc(h) + "." + enc(p)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return unsigned + "." + enc(sig)
}

// signIn runs the browser side of the flow: start, "authenticate" at the IdP
// with the given claims (nonce and audience filled in from the request), and
// follow the callback. It returns the callback response.
func (f *fakeIdP) signIn(t *testing.T, h http.Handler, claims map[string]any, tamper func(map[string]any)) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://cp.example/api/auth/oidc/corp/start?redirect=/%23/apis", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "relayops" || q.Get("redirect_uri") != "http://cp.example/api/auth/oidc/callback" {
		t.Fatalf("authorization request: %s", loc)
	}
	c := map[string]any{"iss": f.issuer, "aud": "relayops", "exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(), "nonce": q.Get("nonce"), "email_verified": true}
	for k, v := range claims {
		c[k] = v
	}
	if tamper != nil {
		tamper(c)
	}
	code := randomToken(16)
	f.mu.Lock()
	f.codes[code] = issued{challenge: q.Get("code_challenge"), claims: c}
	f.mu.Unlock()
	cb := httptest.NewRecorder()
	cbReq := httptest.NewRequest("GET", "http://cp.example/api/auth/oidc/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), nil)
	for _, ck := range rec.Result().Cookies() {
		cbReq.AddCookie(ck)
		if ck.Name == oidcBindCookieName {
			cb.Header().Set("X-Test-Bind-Cookie", ck.Value)
		}
	}
	h.ServeHTTP(cb, cbReq)
	cb.Header().Set("X-Test-State", q.Get("state"))
	return cb
}

var tokenInPage = regexp.MustCompile(`setItem\('relayops_token', "([^"]+)"\)`)

func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	m := tokenInPage.FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || m == nil {
		t.Fatalf("callback did not hand over a session: %d %s", rec.Code, rec.Body)
	}
	return m[1]
}

func TestIntegrationOIDCSignInProvisionsAccessFromGroups(t *testing.T) {
	s := openAdminTestStore(t)
	h := newControlPlane(t, s, "cp").Handler()
	idp := newFakeIdP(t)
	platform := client{t: t, h: h, token: "test-admin-token"}
	platform.must("POST", "/api/tenants", `{"slug":"acme","name":"Acme"}`, 201)
	platform.must("POST", "/api/tenants", `{"slug":"globex","name":"Globex"}`, 201)

	// Only a superadmin configures identity providers; secrets are never returned.
	cfg := `{"issuer":"` + idp.issuer + `","client_id":"relayops","client_secret":"s3cret","allowed_domains":["corp.example"],
		"claim_mappings":[{"claim":"groups","value":"acme-admins","tenant":"acme","role":"admin"},
		                  {"claim":"groups","value":"globex-readers","tenant":"globex","role":"auditor"},
		                  {"claim":"groups","value":"platform-ops","role":"operator"}]}`
	saved := platform.must("PUT", "/api/admin/oidc-providers/corp", cfg, 200)
	if _, leaked := saved["client_secret"]; leaked {
		t.Fatal("client secret returned by the API")
	}
	if rec := platform.do("PUT", "/api/admin/oidc-providers/bad", `{"issuer":"http://idp.example","client_id":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-https issuer accepted: %d", rec.Code)
	}
	if rec := platform.do("PUT", "/api/admin/oidc-providers/bad", `{"issuer":"`+idp.issuer+`","client_id":"x","claim_mappings":[{"claim":"g","value":"v","tenant":"nope","role":"admin"}]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("mapping to an unknown tenant accepted: %d", rec.Code)
	}
	pub := (client{t: t, h: h}).do("GET", "/api/auth/oidc/providers", "")
	if !strings.Contains(pub.Body.String(), `"/api/auth/oidc/corp/start"`) || strings.Contains(pub.Body.String(), "s3cret") {
		t.Fatalf("public provider list: %s", pub.Body)
	}

	// First sign-in provisions the account and its tenant memberships from groups.
	claims := map[string]any{"sub": "u-123", "email": "Erin@corp.example", "email_verified": true, "name": "Erin",
		"groups": []any{"acme-admins", "globex-readers"}}
	rec := idp.signIn(t, h, claims, nil)
	erin := client{t: t, h: h, token: sessionFrom(t, rec)}
	if !strings.Contains(rec.Body.String(), `location.replace("/#/apis")`) {
		t.Fatalf("callback must return to the requested page: %s", rec.Body)
	}
	me := erin.must("GET", "/api/auth/me", "", 200)
	ms := me["memberships"].([]any)
	if len(ms) != 2 || me["platform"] != false || me["user"].(map[string]any)["email"] != "erin@corp.example" {
		t.Fatalf("provisioned access: %v", me)
	}
	erin.in("acme").must("POST", "/api/plans", `{"name":"p","rate_limit_per_minute":1}`, 201)
	if rec := erin.in("globex").do("POST", "/api/plans", `{"name":"p","rate_limit_per_minute":1}`); rec.Code != http.StatusForbidden {
		t.Fatalf("auditor in globex created a plan: %d", rec.Code)
	}

	// The state is single use and protected by initiating-browser binding.
	stateVal := rec.Header().Get("X-Test-State")
	bindCookieVal := rec.Header().Get("X-Test-Bind-Cookie")

	// 1. Replay from an external browser without binding cookie fails closed (403 Forbidden).
	replayNoCookie := httptest.NewRecorder()
	h.ServeHTTP(replayNoCookie, httptest.NewRequest("GET", "http://cp.example/api/auth/oidc/callback?code=x&state="+url.QueryEscape(stateVal), nil))
	if replayNoCookie.Code != http.StatusForbidden {
		t.Fatalf("replayed state without browser binding cookie: %d (want 403)", replayNoCookie.Code)
	}

	// 2. Tampered binding cookie value fails closed (403 Forbidden).
	replayTamperedCookie := httptest.NewRecorder()
	reqTampered := httptest.NewRequest("GET", "http://cp.example/api/auth/oidc/callback?code=x&state="+url.QueryEscape(stateVal), nil)
	reqTampered.AddCookie(&http.Cookie{Name: oidcBindCookieName, Value: stateVal + ".tampered-signature"})
	h.ServeHTTP(replayTamperedCookie, reqTampered)
	if replayTamperedCookie.Code != http.StatusForbidden {
		t.Fatalf("tampered binding cookie: %d (want 403)", replayTamperedCookie.Code)
	}

	// 3. Replay from initiating browser with binding cookie fails because state was already consumed (400 Bad Request).
	replayWithCookie := httptest.NewRecorder()
	reqWithCookie := httptest.NewRequest("GET", "http://cp.example/api/auth/oidc/callback?code=x&state="+url.QueryEscape(stateVal), nil)
	reqWithCookie.AddCookie(&http.Cookie{
		Name:  oidcBindCookieName,
		Value: bindCookieVal,
	})
	h.ServeHTTP(replayWithCookie, reqWithCookie)
	if replayWithCookie.Code != http.StatusBadRequest {
		t.Fatalf("replayed state from initiating browser: %d (want 400)", replayWithCookie.Code)
	}

	// Leaving an IdP group removes the tenant access it granted at next sign-in.
	claims["groups"] = []any{"globex-readers"}
	erin = client{t: t, h: h, token: sessionFrom(t, idp.signIn(t, h, claims, nil))}
	if ms := erin.must("GET", "/api/auth/me", "", 200)["memberships"].([]any); len(ms) != 1 {
		t.Fatalf("memberships after group removal: %v", ms)
	}
	if rec := erin.in("acme").do("GET", "/api/apis", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("access to acme after leaving acme-admins: %d", rec.Code)
	}

	// Platform roles come from platform mappings.
	ops := client{t: t, h: h, token: sessionFrom(t, idp.signIn(t, h, map[string]any{"sub": "u-ops", "email": "ops@corp.example",
		"email_verified": true, "groups": []any{"platform-ops"}}, nil))}
	if me := ops.must("GET", "/api/auth/me", "", 200); me["platform"] != true || me["user"].(map[string]any)["role"] != "operator" {
		t.Fatalf("platform operator: %v", me)
	}

	// Denials.
	for name, tc := range map[string]struct {
		claims map[string]any
		tamper func(map[string]any)
		want   int
	}{
		"no matching group":    {map[string]any{"sub": "u-x", "email": "x@corp.example", "email_verified": true, "groups": []any{"other"}}, nil, 403},
		"foreign domain":       {map[string]any{"sub": "u-y", "email": "y@evil.example", "email_verified": true, "groups": []any{"platform-ops"}}, nil, 403},
		"unverified email":     {map[string]any{"sub": "u-z", "email": "z@corp.example", "email_verified": false, "groups": []any{"platform-ops"}}, nil, 403},
		"nonce mismatch":       {map[string]any{"sub": "u-n", "email": "n@corp.example", "groups": []any{"platform-ops"}}, func(c map[string]any) { c["nonce"] = "forged" }, 401},
		"wrong audience":       {map[string]any{"sub": "u-a", "email": "a@corp.example", "groups": []any{"platform-ops"}}, func(c map[string]any) { c["aud"] = "other-app" }, 401},
		"expired token":        {map[string]any{"sub": "u-e", "email": "e@corp.example", "groups": []any{"platform-ops"}}, func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, 401},
		"email takeover (sub)": {map[string]any{"sub": "someone-else", "email": "erin@corp.example", "email_verified": true, "groups": []any{"globex-readers"}}, nil, 403},
	} {
		if rec := idp.signIn(t, h, tc.claims, tc.tamper); rec.Code != tc.want || tokenInPage.MatchString(rec.Body.String()) {
			t.Errorf("%s: %d (want %d), session issued=%v", name, rec.Code, tc.want, tokenInPage.MatchString(rec.Body.String()))
		}
	}

	// Open redirects are neutralised.
	for _, bad := range []string{"https://evil.example", "//evil.example", "/\\evil.example"} {
		if got := safeRedirect(bad); got != "/" {
			t.Errorf("safeRedirect(%q) = %q", bad, got)
		}
	}
}

func TestOIDCCookieBindingAndPKCEValidation(t *testing.T) {
	s := &Server{token: "cluster-secret"}
	state := "random-state-123456789012345678901234"
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk456789"

	sig := s.oidcBindSignature(state, verifier)
	if sig == "" {
		t.Fatal("expected non-empty signature")
	}

	// Different verifier must produce different signature
	diffVerifierSig := s.oidcBindSignature(state, verifier+"-alt")
	if sig == diffVerifierSig {
		t.Fatal("expected different signature for different verifier")
	}

	// Different state must produce different signature
	diffStateSig := s.oidcBindSignature("other-state", verifier)
	if sig == diffStateSig {
		t.Fatal("expected different signature for different state")
	}

	// PKCE code verifier length validation
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("test verifier length must be RFC 7636 compliant: %d", len(verifier))
	}
}
