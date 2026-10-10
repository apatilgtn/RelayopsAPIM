package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/relayops/apim/internal/store"
)

// Enterprise sign-in: OpenID Connect authorization-code flow with PKCE.
//
//	GET /api/auth/oidc/providers          active providers, for sign-in buttons
//	GET /api/auth/oidc/{provider}/start   redirects to the IdP
//	GET /api/auth/oidc/callback           completes sign-in and opens the dashboard
//
// The callback verifies the ID token's signature (JWKS), issuer, audience,
// expiry and nonce, requires a verified email in an allowed domain, provisions
// the account on first sign-in, and derives the platform role and tenant
// memberships from the provider's claim mappings on every sign-in.

const (
	oidcStateTTL       = 10 * time.Minute
	oidcBindCookieName = "relayops_oidc_bind"
)

func (s *Server) oidcBindSignature(state, codeVerifier string) string {
	key := s.token
	if key == "" {
		key = "relayops-oidc-bind-cluster-key"
	}
	mac := hmac.New(sha256.New, []byte(key+"-oidc-bind"))
	mac.Write([]byte(state))
	if codeVerifier != "" {
		mac.Write([]byte(":"))
		vSum := sha256.Sum256([]byte(codeVerifier))
		mac.Write(vSum[:])
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

var (
	discoveryMu    sync.Mutex
	discoveryCache = map[string]struct {
		doc oidcDiscovery
		at  time.Time
	}{}
)

// resolveEndpoints fills endpoints the provider does not configure explicitly
// from the issuer's discovery document (cached for an hour).
func resolveEndpoints(ctx context.Context, p store.OIDCProvider) (store.OIDCProvider, error) {
	if p.AuthorizationEndpoint != "" && p.TokenEndpoint != "" && p.JWKSURL != "" {
		return p, nil
	}
	discoveryMu.Lock()
	cached, ok := discoveryCache[p.Issuer]
	discoveryMu.Unlock()
	if !ok || time.Since(cached.at) > time.Hour {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.Issuer, "/")+"/.well-known/openid-configuration", nil)
		if err != nil {
			return p, err
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			return p, fmt.Errorf("OIDC discovery failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return p, fmt.Errorf("OIDC discovery returned %d", resp.StatusCode)
		}
		var doc oidcDiscovery
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
			return p, fmt.Errorf("OIDC discovery document is invalid: %w", err)
		}
		if strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(p.Issuer, "/") {
			return p, fmt.Errorf("OIDC discovery issuer %q does not match configured issuer %q", doc.Issuer, p.Issuer)
		}
		cached.doc, cached.at = doc, time.Now()
		discoveryMu.Lock()
		discoveryCache[p.Issuer] = cached
		discoveryMu.Unlock()
	}
	if p.AuthorizationEndpoint == "" {
		p.AuthorizationEndpoint = cached.doc.AuthorizationEndpoint
	}
	if p.TokenEndpoint == "" {
		p.TokenEndpoint = cached.doc.TokenEndpoint
	}
	if p.JWKSURL == "" {
		p.JWKSURL = cached.doc.JWKSURI
	}
	if p.AuthorizationEndpoint == "" || p.TokenEndpoint == "" || p.JWKSURL == "" {
		return p, errors.New("OIDC provider is missing its authorization, token or JWKS endpoint")
	}
	return p, nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// publicBaseURL is where the IdP redirects back to. Set RELAYOPS_PUBLIC_URL in
// production; otherwise it is derived from the request.
func publicBaseURL(r *http.Request) string {
	if v := strings.TrimRight(os.Getenv("RELAYOPS_PUBLIC_URL"), "/"); v != "" {
		return v
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = fh
	}
	return scheme + "://" + host
}

// safeRedirect only allows same-origin paths, never "//host" or absolute URLs.
func safeRedirect(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") || strings.ContainsAny(p, "\r\n") {
		return "/"
	}
	return p
}

func (s *Server) oidcPublicProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.ListOIDCProviders(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	type pub struct {
		Name     string `json:"name"`
		StartURL string `json:"start_url"`
	}
	out := []pub{}
	for _, p := range providers {
		if p.Active {
			out = append(out, pub{Name: p.Name, StartURL: "/api/auth/oidc/" + url.PathEscape(p.Name) + "/start"})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetOIDCProviderByName(r.Context(), r.PathValue("provider"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "provider_not_found", "unknown or inactive identity provider")
		return
	}
	if p, err = resolveEndpoints(r.Context(), p); err != nil {
		writeErr(w, http.StatusBadGateway, "provider_unavailable", err.Error())
		return
	}
	st := store.OIDCLoginState{
		State: randomToken(32), Provider: p.Name, CodeVerifier: randomToken(48), Nonce: randomToken(24),
		Redirect: safeRedirect(r.URL.Query().Get("redirect")), ExpiresAt: time.Now().Add(oidcStateTTL),
	}
	if err := s.store.SaveOIDCLoginState(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}

	// Bind state and PKCE code verifier to the initiating browser session via secure HttpOnly cookie
	http.SetCookie(w, &http.Cookie{
		Name:     oidcBindCookieName,
		Value:    st.State + "." + s.oidcBindSignature(st.State, st.CodeVerifier),
		Path:     "/api/auth/oidc/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		MaxAge:   int(oidcStateTTL.Seconds()),
	})

	challenge := sha256.Sum256([]byte(st.CodeVerifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.ClientID},
		"redirect_uri":          {publicBaseURL(r) + "/api/auth/oidc/callback"},
		"scope":                 {p.Scopes},
		"state":                 {st.State},
		"nonce":                 {st.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(p.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	http.Redirect(w, r, p.AuthorizationEndpoint+sep+q.Encode(), http.StatusFound)
}

// oidcFailure renders a sign-in error without leaking internals.
func oidcFailure(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = failurePage.Execute(w, reason)
}

var failurePage = template.Must(template.New("f").Parse(`<!doctype html><meta charset="utf-8"><title>Sign-in failed</title>
<body style="font-family:system-ui;margin:3rem"><h1>Sign-in failed</h1><p>{{.}}</p><p><a href="/login">Back to sign-in</a></p></body>`))

// handoffPage stores the new session where the dashboard reads it and opens it.
var handoffPage = template.Must(template.New("h").Parse(`<!doctype html><meta charset="utf-8"><title>Signing in…</title>
<script>sessionStorage.setItem('relayops_token', {{.Token}}); localStorage.removeItem('relayops_token'); location.replace({{.Redirect}});</script>
<noscript>JavaScript is required to finish signing in.</noscript>`))

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		oidcFailure(w, http.StatusUnauthorized, "The identity provider returned an error: "+e)
		return
	}

	stateParam := q.Get("state")
	if stateParam == "" {
		oidcFailure(w, http.StatusBadRequest, "This sign-in link is invalid or has expired. Start again.")
		return
	}

	// Verify initiating browser binding cookie exists
	cookie, err := r.Cookie(oidcBindCookieName)
	if err != nil || cookie == nil || cookie.Value == "" {
		slog.Warn("oidc callback rejected: browser binding cookie missing", "remote_ip", clientIP(r))
		oidcFailure(w, http.StatusForbidden, "Sign-in state does not match the initiating browser session.")
		return
	}

	// Clear binding cookie immediately
	http.SetCookie(w, &http.Cookie{
		Name:     oidcBindCookieName,
		Value:    "",
		Path:     "/api/auth/oidc/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	// Verify the binding (state and PKCE code verifier) before consuming the
	// state, so a forged or foreign cookie can neither use nor burn another
	// browser's sign-in, and a tampered replay is refused as such.
	pending, _, err := s.store.GetOIDCLoginState(r.Context(), stateParam)
	if err != nil {
		oidcFailure(w, http.StatusBadRequest, "This sign-in link is invalid or has expired. Start again.")
		return
	}
	expectedCookieVal := pending.State + "." + s.oidcBindSignature(pending.State, pending.CodeVerifier)
	matched := subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(expectedCookieVal)) == 1
	if !matched {
		legacySig := s.oidcBindSignature(pending.State, "")
		if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(legacySig)) == 1 {
			matched = true
		}
	}
	if !matched {
		slog.Warn("oidc callback rejected: browser binding cookie mismatched or tampered", "remote_ip", clientIP(r))
		oidcFailure(w, http.StatusForbidden, "Sign-in state does not match the initiating browser session.")
		return
	}

	st, err := s.store.ConsumeOIDCLoginState(r.Context(), stateParam)
	if err != nil {
		oidcFailure(w, http.StatusBadRequest, "This sign-in link is invalid or has expired. Start again.")
		return
	}

	// Strict PKCE code verifier validation (RFC 7636 Section 4.1)
	if len(st.CodeVerifier) < 43 || len(st.CodeVerifier) > 128 {
		slog.Warn("oidc rejected: invalid PKCE code verifier length", "len", len(st.CodeVerifier))
		oidcFailure(w, http.StatusBadRequest, "Invalid PKCE code verifier length.")
		return
	}
	p, err := s.store.GetOIDCProviderByName(r.Context(), st.Provider)
	if err != nil {
		oidcFailure(w, http.StatusUnauthorized, "The identity provider is no longer active.")
		return
	}
	if p, err = resolveEndpoints(r.Context(), p); err != nil {
		oidcFailure(w, http.StatusBadGateway, err.Error())
		return
	}
	idToken, err := exchangeCode(r.Context(), p, q.Get("code"), publicBaseURL(r)+"/api/auth/oidc/callback", st.CodeVerifier)
	if err != nil {
		slog.Warn("oidc code exchange failed", "provider", p.Name, "err", err)
		oidcFailure(w, http.StatusUnauthorized, "The identity provider did not accept the sign-in.")
		return
	}
	claims, err := s.jwks.VerifyOIDC(idToken, p.JWKSURL, p.Issuer, p.ClientID, time.Now())
	if err != nil {
		slog.Warn("oidc id token rejected", "provider", p.Name, "err", err)
		oidcFailure(w, http.StatusUnauthorized, "The identity token could not be verified.")
		return
	}
	if nonce, _ := claims["nonce"].(string); nonce == "" || nonce != st.Nonce {
		oidcFailure(w, http.StatusUnauthorized, "The identity token does not belong to this sign-in.")
		return
	}
	user, status, reason := s.provisionFromClaims(r.Context(), p, claims)
	if reason != "" {
		oidcFailure(w, status, reason)
		return
	}
	token := newSessionID()
	if _, err := s.store.CreateAdminSession(r.Context(), user.ID, store.HashKey(token), clientIP(r), r.UserAgent(), 12*time.Hour); err != nil {
		s.fail(w, err)
		return
	}
	_ = s.store.UpdateAdminUserLogin(r.Context(), user.ID)
	_ = s.store.CreateRichAuditLog(r.Context(), store.AuditLog{
		Actor: user.Email, ActorID: user.ID, ActorEmail: user.Email, ActorRole: user.Role, Action: "LOGIN",
		ResourceType: "admin_user", ResourceID: user.ID, Details: map[string]any{"method": "oidc", "provider": p.Name},
		ClientIP: clientIP(r), UserAgent: r.UserAgent(),
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_ = handoffPage.Execute(w, map[string]string{"Token": token, "Redirect": st.Redirect})
}

func exchangeCode(ctx context.Context, p store.OIDCProvider, code, redirectURI, verifier string) (string, error) {
	if code == "" {
		return "", errors.New("missing authorization code")
	}
	if verifier == "" {
		return "", errors.New("missing PKCE code verifier")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {p.ClientID},
		"code_verifier": {verifier},
	}
	if p.ClientSecret != "" {
		form.Set("client_secret", p.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || out.IDToken == "" {
		return "", fmt.Errorf("token endpoint returned %d %s", resp.StatusCode, out.Error)
	}
	return out.IDToken, nil
}

// claimValues returns a claim as a list of strings (string, bool or array claims).
func claimValues(claims map[string]any, name string) []string {
	switch v := claims[name].(type) {
	case string:
		return []string{v}
	case bool:
		return []string{fmt.Sprint(v)}
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// mappedAccess applies a provider's claim mappings: the highest platform role
// granted, and the highest role granted in each tenant (by slug).
func mappedAccess(p store.OIDCProvider, claims map[string]any) (platformRole string, tenantRoles map[string]string, matched bool) {
	tenantRoles = map[string]string{}
	for _, m := range p.ClaimMappings {
		hit := false
		for _, v := range claimValues(claims, m.Claim) {
			if v == m.Value {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		matched = true
		if m.Tenant == "" {
			if roleRank[m.Role] > roleRank[platformRole] && m.Role != "superadmin" {
				platformRole = m.Role
			}
		} else if roleRank[m.Role] > roleRank[tenantRoles[m.Tenant]] {
			tenantRoles[m.Tenant] = m.Role
		}
	}
	return platformRole, tenantRoles, matched
}

// provisionFromClaims finds or creates the account for a verified ID token and
// synchronises its IdP-derived access. A non-empty reason means access is denied.
func (s *Server) provisionFromClaims(ctx context.Context, p store.OIDCProvider, claims map[string]any) (store.AdminUser, int, string) {
	email, _ := claims["email"].(string)
	email = strings.ToLower(strings.TrimSpace(email))
	sub, _ := claims["sub"].(string)
	if email == "" || sub == "" {
		return store.AdminUser{}, http.StatusUnauthorized, "The identity provider did not supply an email address."
	}
	if v, present := claims["email_verified"]; !present || (v != true && v != "true") {
		return store.AdminUser{}, http.StatusForbidden, "Your email address is not verified with the identity provider (email_verified=true claim required)."
	}
	if len(p.AllowedDomains) > 0 {
		domain := email[strings.LastIndex(email, "@")+1:]
		allowed := false
		for _, d := range p.AllowedDomains {
			if strings.EqualFold(strings.TrimPrefix(d, "@"), domain) {
				allowed = true
			}
		}
		if !allowed {
			return store.AdminUser{}, http.StatusForbidden, "Your email domain is not allowed to sign in with this provider."
		}
	}

	platformRole, tenantSlugs, matched := mappedAccess(p, claims)
	if len(p.ClaimMappings) > 0 && !matched {
		return store.AdminUser{}, http.StatusForbidden, "Your account is not in any group that grants access to RelayOps."
	}
	if len(p.ClaimMappings) == 0 {
		platformRole = p.DefaultRole
	}
	if platformRole == "" {
		platformRole = "developer" // tenant-only access; real permissions come from memberships
	}
	name, _ := claims["name"].(string)

	user, err := s.store.GetAdminUserByEmail(ctx, email)
	switch {
	case err == nil:
		if !user.Active {
			return store.AdminUser{}, http.StatusForbidden, "Your account has been deactivated."
		}
		// An account already bound to this provider must keep the same subject:
		// a reassigned email must not take over someone else's account.
		if user.SSOProvider == p.Name && user.SSOSub != "" && user.SSOSub != sub {
			return store.AdminUser{}, http.StatusForbidden, "This email is bound to a different identity at the provider."
		}
		role := platformRole
		if user.Role == "superadmin" {
			role = "superadmin" // superadmin is only granted and removed manually
		}
		if user, err = s.store.SetAdminUserSSO(ctx, user.ID, p.Name, sub, name, role); err != nil {
			return store.AdminUser{}, http.StatusInternalServerError, "Sign-in failed."
		}
	case errors.Is(err, store.ErrNotFound) || errors.Is(err, errNoRows):
		if name == "" {
			name = email
		}
		if user, err = s.store.CreateAdminUser(ctx, store.AdminUser{Email: email, Name: name, Role: platformRole,
			SSOProvider: p.Name, SSOSub: sub, Team: "Federated"}); err != nil {
			return store.AdminUser{}, http.StatusInternalServerError, "Sign-in failed."
		}
	default:
		return store.AdminUser{}, http.StatusInternalServerError, "Sign-in failed."
	}

	// Tenant memberships granted by this provider are replaced on every sign-in,
	// so removing someone from an IdP group removes their tenant access.
	roles := map[string]string{}
	for slug, role := range tenantSlugs {
		t, err := s.store.GetTenantBySlug(ctx, slug)
		if err != nil {
			slog.Warn("oidc claim mapping references an unknown tenant", "provider", p.Name, "tenant", slug)
			continue
		}
		roles[t.ID] = role
	}
	if err := s.store.ReplaceSourcedMemberships(ctx, user.ID, "idp:"+p.Name, roles); err != nil {
		return store.AdminUser{}, http.StatusInternalServerError, "Sign-in failed."
	}
	return user, 0, ""
}

// ---------------------------------------------------------------------------
// Provider administration (platform superadmin)
// ---------------------------------------------------------------------------

var providerNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}$`)

func validProviderURL(raw string, required bool) error {
	if raw == "" {
		if required {
			return errors.New("is required")
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("must be an absolute URL")
	}
	host := u.Hostname()
	if u.Scheme == "https" || (u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return nil
	}
	return errors.New("must use https")
}

func (s *Server) saveOIDCProvider(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	if user.Role != "superadmin" {
		writeErr(w, http.StatusForbidden, "forbidden", "managing identity providers requires a superadmin")
		return
	}
	var in struct {
		store.OIDCProvider
		Active *bool `json:"active"` // omitted = active
	}
	if !decode(w, r, &in) {
		return
	}
	p := in.OIDCProvider
	p.Active = in.Active == nil || *in.Active
	p.Name = r.PathValue("name")
	if !providerNamePattern.MatchString(p.Name) {
		writeErr(w, http.StatusBadRequest, "validation_failed", "provider name must be 2-63 lowercase letters, digits, '-' or '_'")
		return
	}
	for field, check := range map[string]error{
		"issuer":                 validProviderURL(p.Issuer, true),
		"jwks_url":               validProviderURL(p.JWKSURL, false),
		"authorization_endpoint": validProviderURL(p.AuthorizationEndpoint, false),
		"token_endpoint":         validProviderURL(p.TokenEndpoint, false),
	} {
		if check != nil {
			writeErr(w, http.StatusBadRequest, "validation_failed", field+" "+check.Error())
			return
		}
	}
	if strings.TrimSpace(p.ClientID) == "" {
		writeErr(w, http.StatusBadRequest, "validation_failed", "client_id is required")
		return
	}
	if p.DefaultRole == "" {
		p.DefaultRole = "auditor"
	}
	if !store.ValidMembershipRole(p.DefaultRole) {
		writeErr(w, http.StatusBadRequest, "validation_failed", "default_role must be one of admin, operator, auditor, developer")
		return
	}
	for i, m := range p.ClaimMappings {
		if m.Claim == "" || m.Value == "" || !store.ValidMembershipRole(m.Role) {
			writeErr(w, http.StatusBadRequest, "validation_failed", fmt.Sprintf("claim_mappings[%d] needs claim, value and a role of admin, operator, auditor or developer", i))
			return
		}
		if m.Tenant != "" {
			if _, err := s.store.GetTenantBySlug(r.Context(), m.Tenant); err != nil {
				writeErr(w, http.StatusBadRequest, "validation_failed", fmt.Sprintf("claim_mappings[%d] references unknown tenant %q", i, m.Tenant))
				return
			}
		}
	}
	saved, err := s.store.SaveOIDCProvider(r.Context(), p)
	if err == nil {
		s.audit(r, "SAVE_OIDC_PROVIDER", "oidc_provider", saved.ID, map[string]any{"name": saved.Name, "issuer": saved.Issuer, "mappings": len(saved.ClaimMappings)})
	}
	s.respond(w, http.StatusOK, saved, err)
}

func (s *Server) deleteOIDCProvider(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	if user.Role != "superadmin" {
		writeErr(w, http.StatusForbidden, "forbidden", "managing identity providers requires a superadmin")
		return
	}
	name := r.PathValue("name")
	err := s.store.DeleteOIDCProvider(r.Context(), name)
	if err == nil {
		s.audit(r, "DELETE_OIDC_PROVIDER", "oidc_provider", name, map[string]any{"name": name})
	}
	s.noContent(w, err)
}
