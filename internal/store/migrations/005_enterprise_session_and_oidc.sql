-- 005_enterprise_session_and_oidc.sql
-- Enterprise Persistent Session Management, Identity Lifecycle & OIDC Federation

-- 1. PERSISTENT ADMIN SESSIONS (DATABASE-BACKED IDENTITY LIFECYCLE)
CREATE TABLE IF NOT EXISTS admin_sessions (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash     TEXT UNIQUE NOT NULL,
    user_id        UUID NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    ip_address     TEXT NOT NULL DEFAULT '',
    user_agent     TEXT NOT NULL DEFAULT '',
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_active_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS admin_sessions_token_hash_idx ON admin_sessions (token_hash);
CREATE INDEX IF NOT EXISTS admin_sessions_user_id_idx ON admin_sessions (user_id);
CREATE INDEX IF NOT EXISTS admin_sessions_expires_at_idx ON admin_sessions (expires_at);

-- 2. ENTERPRISE OIDC IDENTITY PROVIDERS
CREATE TABLE IF NOT EXISTS oidc_providers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL UNIQUE,
    issuer          TEXT NOT NULL,
    client_id       TEXT NOT NULL,
    client_secret   TEXT NOT NULL DEFAULT '',
    jwks_url        TEXT NOT NULL DEFAULT '',
    allowed_domains JSONB NOT NULL DEFAULT '[]'::jsonb,
    default_role    TEXT NOT NULL DEFAULT 'operator' CHECK (default_role IN ('admin', 'operator', 'auditor', 'developer')),
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS oidc_providers_name_idx ON oidc_providers (name);
