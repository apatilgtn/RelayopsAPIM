-- 004_enterprise_platform.sql
-- Enterprise Access, Safe Configuration Releases, Developer Self-Service,
-- Decision Provenance & Traffic Replay.

-- 1. ENHANCED AUDIT LOGS WITH COMPLETE ACTOR ATTRIBUTION & STATE DIFFS
ALTER TABLE audit_logs
    ADD COLUMN IF NOT EXISTS actor_id      TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS actor_email   TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS actor_role    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS resource_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS before_state  JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS after_state   JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS state_diff    JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS user_agent    TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS audit_logs_actor_idx ON audit_logs (actor_email, actor_role);

-- 2. NAMED ADMIN USERS WITH RBAC & SSO SUPPORT
ALTER TABLE admin_users
    ADD COLUMN IF NOT EXISTS password_hash TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS sso_provider  TEXT NOT NULL DEFAULT '', -- 'local', 'oidc', 'saml'
    ADD COLUMN IF NOT EXISTS sso_sub       TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS team          TEXT NOT NULL DEFAULT 'Engineering',
    ADD COLUMN IF NOT EXISTS last_login_at TIMESTAMPTZ;

-- Ensure role check allows 'superadmin', 'operator', 'auditor', 'developer'
ALTER TABLE admin_users DROP CONSTRAINT IF EXISTS admin_users_role_check;
ALTER TABLE admin_users ADD CONSTRAINT admin_users_role_check 
    CHECK (role IN ('superadmin', 'admin', 'operator', 'auditor', 'developer'));

-- Update or seed initial superadmin with known password hash (bcrypt or secure token)
UPDATE admin_users SET role = 'superadmin', team = 'Platform Ops' WHERE email = 'admin@relayops.local';

-- 3. SAFE RELEASES: CONFIG REVISION STATUS, CANARY NODE GROUPS & ROLLBACK TARGETS
ALTER TABLE config_revisions
    ADD COLUMN IF NOT EXISTS status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'canary', 'rolled_back')),
    ADD COLUMN IF NOT EXISTS target_group    TEXT NOT NULL DEFAULT 'all', -- 'all', 'canary', or specific node_group
    ADD COLUMN IF NOT EXISTS parent_revision BIGINT,
    ADD COLUMN IF NOT EXISTS rollback_of     BIGINT;

ALTER TABLE node_acknowledgements
    ADD COLUMN IF NOT EXISTS node_group      TEXT NOT NULL DEFAULT 'default',
    ADD COLUMN IF NOT EXISTS is_canary       BOOLEAN NOT NULL DEFAULT false;

-- 4. DEVELOPER SELF-SERVICE: ACCOUNTS, APPLICATIONS & ZERO-DOWNTIME KEY ROTATION
CREATE TABLE IF NOT EXISTS developer_apps (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    consumer_id   UUID NOT NULL REFERENCES consumers(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    environment   TEXT NOT NULL DEFAULT 'production' CHECK (environment IN ('production', 'staging', 'development')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS dev_apps_consumer_idx ON developer_apps (consumer_id);

-- Add application link & zero-downtime key rotation columns to api_keys
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS app_id           UUID REFERENCES developer_apps(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS is_secondary     BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS grace_until      TIMESTAMPTZ, -- dual-active key grace period expiration
    ADD COLUMN IF NOT EXISTS rotated_from_id  UUID REFERENCES api_keys(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS api_keys_app_idx ON api_keys (app_id);

-- Add rejection reason to subscriptions
ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS app_id           UUID REFERENCES developer_apps(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS rejection_reason TEXT NOT NULL DEFAULT '';

-- 5. RELIABLE REQUEST EXPLANATIONS: POLICY PROVENANCE & REVISION ATTACHMENT
ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS config_revision    BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS matched_route      TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS policy_evaluations JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE INDEX IF NOT EXISTS request_logs_rev_idx ON request_logs (config_revision);
