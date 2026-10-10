-- 003_secure_pilot_and_visibility.sql
-- Release 1: Secure Developer Registration, Strict Governance, Redis Failure Policies,
-- Audit Trail, Config Revisions & Request Decision Explorer.

-- 1. API Governance & Visibility
ALTER TABLE apis
    ADD COLUMN IF NOT EXISTS visibility           TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public', 'private', 'internal')),
    ADD COLUMN IF NOT EXISTS require_approval      BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS is_draft             BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS quota_failure_policy TEXT NOT NULL DEFAULT 'fail_open' CHECK (quota_failure_policy IN ('fail_open', 'fail_closed'));

-- 2. Consumer Status & Abuse Protection
ALTER TABLE consumers
    ADD COLUMN IF NOT EXISTS status               TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('pending_approval', 'active', 'suspended')),
    ADD COLUMN IF NOT EXISTS registration_ip      TEXT NOT NULL DEFAULT '';

-- 3. Subscription Approval Workflow
ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS status               TEXT NOT NULL DEFAULT 'approved' CHECK (status IN ('pending', 'approved', 'rejected'));

-- Update trigger function to broadcast on subscriptions and apis update
CREATE OR REPLACE FUNCTION relayops_notify_config() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('relayops_config',
        json_build_object('table', TG_TABLE_NAME, 'op', TG_OP, 'at', now())::text);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- 4. Audit Trail for Enterprise Compliance
CREATE TABLE IF NOT EXISTS audit_logs (
    id            BIGSERIAL PRIMARY KEY,
    ts            TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor         TEXT NOT NULL DEFAULT 'system',
    action        TEXT NOT NULL, -- CREATE, UPDATE, DELETE, APPROVE, REJECT, ROLLBACK
    resource_type TEXT NOT NULL, -- api, consumer, subscription, plan, key, revision
    resource_id   TEXT NOT NULL,
    details       JSONB NOT NULL DEFAULT '{}'::jsonb,
    client_ip     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_logs_ts_idx ON audit_logs (ts DESC);
CREATE INDEX IF NOT EXISTS audit_logs_res_idx ON audit_logs (resource_type, resource_id);

-- 5. Named Admin Users & Roles
CREATE TABLE IF NOT EXISTS admin_users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'operator' CHECK (role IN ('admin', 'operator', 'viewer')),
    token_hash    TEXT NOT NULL,
    active        BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seed default admin user matching admin token
INSERT INTO admin_users (email, name, role, token_hash)
VALUES (
    'admin@relayops.local',
    'Platform Administrator',
    'admin',
    encode(sha256('relayops-admin'::bytea), 'hex')
) ON CONFLICT (email) DO NOTHING;

-- 6. Config Revisions & Fleet Configuration Proof
CREATE TABLE IF NOT EXISTS config_revisions (
    revision      BIGSERIAL PRIMARY KEY,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    TEXT NOT NULL DEFAULT 'system',
    description   TEXT NOT NULL DEFAULT '',
    snapshot_data JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE IF NOT EXISTS node_acknowledgements (
    node_id       TEXT NOT NULL,
    revision      BIGINT NOT NULL REFERENCES config_revisions(revision) ON DELETE CASCADE,
    applied_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    routes_count  INT NOT NULL DEFAULT 0,
    keys_count    INT NOT NULL DEFAULT 0,
    took_ms       DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (node_id, revision)
);
CREATE INDEX IF NOT EXISTS node_ack_rev_idx ON node_acknowledgements (revision);

-- Seed initial revision
INSERT INTO config_revisions (revision, description)
VALUES (1, 'Initial baseline system configuration')
ON CONFLICT (revision) DO NOTHING;

-- 7. Request Decision Explorer Foundation in Request Logs
ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS decision_reason      TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS auth_status          TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS subscription_status  TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS rate_limit_status    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS upstream_duration_ms DOUBLE PRECISION NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS request_logs_decision_idx ON request_logs (decision_reason) WHERE decision_reason <> '';
