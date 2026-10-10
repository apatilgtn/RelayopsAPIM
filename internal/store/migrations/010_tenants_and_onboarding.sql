-- 010_tenants_and_onboarding.sql
-- Tenant isolation and enterprise identity onboarding.
--
-- Every API, plan and consumer belongs to a tenant; keys, subscriptions and
-- developer apps inherit the tenant of their consumer/API. Existing data moves
-- into the "default" tenant, so a single-tenant install behaves as before.
-- Base paths stay globally unique because all tenants share the data plane.

CREATE TABLE IF NOT EXISTS tenants (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       TEXT NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO tenants (id, slug, name) VALUES ('00000000-0000-0000-0000-000000000def', 'default', 'Default')
ON CONFLICT (id) DO NOTHING;

ALTER TABLE apis      ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000def' REFERENCES tenants(id);
ALTER TABLE plans     ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000def' REFERENCES tenants(id);
ALTER TABLE consumers ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000def' REFERENCES tenants(id);
CREATE INDEX IF NOT EXISTS apis_tenant_idx      ON apis (tenant_id);
CREATE INDEX IF NOT EXISTS plans_tenant_idx     ON plans (tenant_id);
CREATE INDEX IF NOT EXISTS consumers_tenant_idx ON consumers (tenant_id);

-- Names are unique per tenant.
ALTER TABLE plans     DROP CONSTRAINT IF EXISTS plans_name_key;
ALTER TABLE apis      DROP CONSTRAINT IF EXISTS apis_name_key;
ALTER TABLE consumers DROP CONSTRAINT IF EXISTS consumers_name_key;
ALTER TABLE plans     ADD CONSTRAINT plans_tenant_name_key     UNIQUE (tenant_id, name);
ALTER TABLE apis      ADD CONSTRAINT apis_tenant_name_key      UNIQUE (tenant_id, name);
ALTER TABLE consumers ADD CONSTRAINT consumers_tenant_name_key UNIQUE (tenant_id, name);

-- Request logs and audit entries record their tenant for scoped queries.
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS tenant_id UUID;
UPDATE request_logs l SET tenant_id = a.tenant_id FROM apis a WHERE l.api_id = a.id AND l.tenant_id IS NULL;
CREATE INDEX IF NOT EXISTS request_logs_tenant_ts_idx ON request_logs (tenant_id, ts DESC);
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS tenant_id UUID;
CREATE INDEX IF NOT EXISTS audit_logs_tenant_idx ON audit_logs (tenant_id, ts DESC);

-- Tenant memberships. An administrator with memberships is tenant-scoped: they
-- see and change only their tenants, with the role held in each. Memberships
-- with source 'idp:<provider>' are re-derived from IdP claims at every sign-in.
CREATE TABLE IF NOT EXISTS tenant_memberships (
    tenant_id  UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'auditor', 'developer')),
    source     TEXT NOT NULL DEFAULT 'manual',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX IF NOT EXISTS tenant_memberships_user_idx ON tenant_memberships (user_id);

-- OIDC authorization-code sign-in.
ALTER TABLE oidc_providers
    ADD COLUMN IF NOT EXISTS authorization_endpoint TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS token_endpoint         TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scopes                 TEXT  NOT NULL DEFAULT 'openid email profile',
    ADD COLUMN IF NOT EXISTS claim_mappings         JSONB NOT NULL DEFAULT '[]'::jsonb;

CREATE TABLE IF NOT EXISTS oidc_login_states (
    state         TEXT PRIMARY KEY,
    provider      TEXT NOT NULL,
    code_verifier TEXT NOT NULL,
    nonce         TEXT NOT NULL,
    redirect      TEXT NOT NULL DEFAULT '/',
    expires_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS oidc_login_states_expiry_idx ON oidc_login_states (expires_at);
