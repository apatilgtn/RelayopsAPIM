package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// ClaimMapping maps an IdP claim value to a platform role (Tenant empty) or to
// a role in a tenant. Claim values may be strings or arrays (e.g. "groups").
type ClaimMapping struct {
	Claim  string `json:"claim"`
	Value  string `json:"value"`
	Tenant string `json:"tenant,omitempty"` // tenant slug; empty = platform role
	Role   string `json:"role"`
}

type OIDCProvider struct {
	ID                    string         `json:"id"`
	Name                  string         `json:"name"`
	Issuer                string         `json:"issuer"`
	ClientID              string         `json:"client_id"`
	ClientSecret          string         `json:"client_secret,omitempty"`
	JWKSURL               string         `json:"jwks_url"`
	AuthorizationEndpoint string         `json:"authorization_endpoint"`
	TokenEndpoint         string         `json:"token_endpoint"`
	Scopes                string         `json:"scopes"`
	AllowedDomains        []string       `json:"allowed_domains"`
	DefaultRole           string         `json:"default_role"`
	ClaimMappings         []ClaimMapping `json:"claim_mappings"`
	Active                bool           `json:"active"`
	CreatedAt             time.Time      `json:"created_at"`
	UpdatedAt             time.Time      `json:"updated_at"`
}

const oidcCols = `id, name, issuer, client_id, client_secret, jwks_url, authorization_endpoint, token_endpoint, scopes,
	allowed_domains, default_role, claim_mappings, active, created_at, updated_at`

func scanOIDC(row pgx.Row) (OIDCProvider, error) {
	var p OIDCProvider
	var domRaw, mapRaw []byte
	err := row.Scan(&p.ID, &p.Name, &p.Issuer, &p.ClientID, &p.ClientSecret, &p.JWKSURL, &p.AuthorizationEndpoint,
		&p.TokenEndpoint, &p.Scopes, &domRaw, &p.DefaultRole, &mapRaw, &p.Active, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return OIDCProvider{}, mapErr(err)
	}
	_ = json.Unmarshal(domRaw, &p.AllowedDomains)
	_ = json.Unmarshal(mapRaw, &p.ClaimMappings)
	if p.AllowedDomains == nil {
		p.AllowedDomains = []string{}
	}
	if p.ClaimMappings == nil {
		p.ClaimMappings = []ClaimMapping{}
	}
	return p, nil
}

// ListOIDCProviders lists providers with client secrets removed.
func (s *Store) ListOIDCProviders(ctx context.Context) ([]OIDCProvider, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+oidcCols+` FROM oidc_providers ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []OIDCProvider{}
	for rows.Next() {
		p, err := scanOIDC(rows)
		if err != nil {
			return nil, err
		}
		p.ClientSecret = ""
		list = append(list, p)
	}
	return list, rows.Err()
}

// GetOIDCProviderByName returns an active provider, including its client secret.
func (s *Store) GetOIDCProviderByName(ctx context.Context, name string) (OIDCProvider, error) {
	return scanOIDC(s.Pool.QueryRow(ctx, `SELECT `+oidcCols+` FROM oidc_providers WHERE name = $1 AND active = true`, name))
}

// SaveOIDCProvider creates or replaces a provider by name. An empty client
// secret keeps the stored one.
func (s *Store) SaveOIDCProvider(ctx context.Context, p OIDCProvider) (OIDCProvider, error) {
	if p.AllowedDomains == nil {
		p.AllowedDomains = []string{}
	}
	if p.ClaimMappings == nil {
		p.ClaimMappings = []ClaimMapping{}
	}
	if p.Scopes == "" {
		p.Scopes = "openid email profile"
	}
	dom, _ := json.Marshal(p.AllowedDomains)
	maps, _ := json.Marshal(p.ClaimMappings)
	out, err := scanOIDC(s.Pool.QueryRow(ctx, `INSERT INTO oidc_providers
		(name, issuer, client_id, client_secret, jwks_url, authorization_endpoint, token_endpoint, scopes, allowed_domains, default_role, claim_mappings, active)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (name) DO UPDATE SET issuer=$2, client_id=$3,
			client_secret = CASE WHEN $4 = '' THEN oidc_providers.client_secret ELSE $4 END,
			jwks_url=$5, authorization_endpoint=$6, token_endpoint=$7, scopes=$8, allowed_domains=$9,
			default_role=$10, claim_mappings=$11, active=$12, updated_at=now()
		RETURNING `+oidcCols,
		p.Name, p.Issuer, p.ClientID, p.ClientSecret, p.JWKSURL, p.AuthorizationEndpoint, p.TokenEndpoint, p.Scopes,
		dom, p.DefaultRole, maps, p.Active))
	out.ClientSecret = ""
	return out, err
}

func (s *Store) DeleteOIDCProvider(ctx context.Context, name string) error {
	return s.execOne(ctx, `DELETE FROM oidc_providers WHERE name=$1`, name)
}

// ---------------------------------------------------------------------------
// Authorization-code sign-in state (shared by every control-plane node)
// ---------------------------------------------------------------------------

type OIDCLoginState struct {
	State        string
	Provider     string
	CodeVerifier string
	Nonce        string
	Redirect     string
	ExpiresAt    time.Time
}

func (s *Store) SaveOIDCLoginState(ctx context.Context, st OIDCLoginState) error {
	_, _ = s.Pool.Exec(ctx, `DELETE FROM oidc_login_states WHERE expires_at < now()`)
	_, err := s.Pool.Exec(ctx, `INSERT INTO oidc_login_states (state, provider, code_verifier, nonce, redirect, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, st.State, st.Provider, st.CodeVerifier, st.Nonce, st.Redirect, st.ExpiresAt)
	return err
}

// GetOIDCLoginState returns an unexpired state without consuming it, and
// whether it has already been consumed.
func (s *Store) GetOIDCLoginState(ctx context.Context, state string) (OIDCLoginState, bool, error) {
	var st OIDCLoginState
	var consumed bool
	err := s.Pool.QueryRow(ctx, `SELECT state, provider, code_verifier, nonce, redirect, expires_at, consumed_at IS NOT NULL
		FROM oidc_login_states WHERE state=$1 AND expires_at > now()`, state).
		Scan(&st.State, &st.Provider, &st.CodeVerifier, &st.Nonce, &st.Redirect, &st.ExpiresAt, &consumed)
	return st, consumed, mapErr(err)
}

// ConsumeOIDCLoginState atomically marks an unexpired, unused state consumed
// and returns it; it can be used once. The row is kept until it expires.
func (s *Store) ConsumeOIDCLoginState(ctx context.Context, state string) (OIDCLoginState, error) {
	var st OIDCLoginState
	err := s.Pool.QueryRow(ctx, `UPDATE oidc_login_states SET consumed_at = now()
		WHERE state=$1 AND expires_at > now() AND consumed_at IS NULL
		RETURNING state, provider, code_verifier, nonce, redirect, expires_at`, state).
		Scan(&st.State, &st.Provider, &st.CodeVerifier, &st.Nonce, &st.Redirect, &st.ExpiresAt)
	return st, mapErr(err)
}
