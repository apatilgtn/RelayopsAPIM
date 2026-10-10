-- 016_apiops_foundation.sql
-- RelayOps APIOps: environment registry, deployment orchestrations,
-- and tamper-evident Release Passports linking reviewed plans to verified releases.

CREATE TABLE IF NOT EXISTS apiops_environments (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    is_production      BOOLEAN NOT NULL DEFAULT false,
    target_gateway_url TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS apiops_environments_tenant_idx ON apiops_environments(tenant_id);

CREATE TABLE IF NOT EXISTS apiops_deployments (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    environment_id         UUID REFERENCES apiops_environments(id) ON DELETE SET NULL,
    status                 TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'planned', 'verifying', 'canary', 'promoted', 'aborted', 'recovering')),
    commit_sha             TEXT NOT NULL DEFAULT '',
    repo_url               TEXT NOT NULL DEFAULT '',
    branch                 TEXT NOT NULL DEFAULT '',
    actor                  TEXT NOT NULL DEFAULT '',
    plan_hash              TEXT NOT NULL DEFAULT '',
    expected_base_revision BIGINT NOT NULL DEFAULT 0,
    candidate_revision     BIGINT NOT NULL DEFAULT 0,
    promoted_revision      BIGINT NOT NULL DEFAULT 0,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS apiops_deployments_tenant_idx ON apiops_deployments(tenant_id);
CREATE INDEX IF NOT EXISTS apiops_deployments_status_idx ON apiops_deployments(status);

CREATE TABLE IF NOT EXISTS apiops_release_passports (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id   UUID NOT NULL REFERENCES apiops_deployments(id) ON DELETE CASCADE,
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    manifest        JSONB NOT NULL DEFAULT '{}'::jsonb,
    evidence_digest TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS apiops_passports_deployment_idx ON apiops_release_passports(deployment_id);
CREATE INDEX IF NOT EXISTS apiops_passports_tenant_idx ON apiops_release_passports(tenant_id);
