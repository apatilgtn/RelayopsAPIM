-- 012_test_studio.sql
-- RelayOps API Test Studio: test suites, versions, environments, credentials,
-- runs, steps, artifacts, background jobs, gate policies, and evidence.

CREATE TABLE IF NOT EXISTS test_suites (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    api_id          UUID REFERENCES apis(id) ON DELETE SET NULL,
    name            TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    ownership       TEXT NOT NULL DEFAULT 'team',
    created_by      TEXT NOT NULL DEFAULT '',
    visibility      TEXT NOT NULL DEFAULT 'tenant',
    current_version INT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT test_suites_tenant_name_key UNIQUE (tenant_id, name)
);
CREATE INDEX IF NOT EXISTS test_suites_tenant_idx ON test_suites(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS test_suites_api_idx ON test_suites(api_id);

CREATE TABLE IF NOT EXISTS test_suite_versions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    suite_id        UUID NOT NULL REFERENCES test_suites(id) ON DELETE CASCADE,
    version         INT NOT NULL,
    schema_version  INT NOT NULL DEFAULT 1,
    content_hash    TEXT NOT NULL,
    definition      JSONB NOT NULL DEFAULT '{}'::jsonb,
    author          TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT test_suite_versions_suite_version_key UNIQUE (suite_id, version)
);
CREATE INDEX IF NOT EXISTS test_suite_versions_suite_idx ON test_suite_versions(suite_id, version DESC);

CREATE TABLE IF NOT EXISTS test_environments (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name                TEXT NOT NULL,
    gateway_target      TEXT NOT NULL DEFAULT '',
    variables           JSONB NOT NULL DEFAULT '{}'::jsonb,
    credential_bindings JSONB NOT NULL DEFAULT '{}'::jsonb,
    revision            INT NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT test_environments_tenant_name_key UNIQUE (tenant_id, name)
);
CREATE INDEX IF NOT EXISTS test_environments_tenant_idx ON test_environments(tenant_id);

CREATE TABLE IF NOT EXISTS test_credentials (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name                TEXT NOT NULL,
    provider_ref        TEXT NOT NULL DEFAULT '',
    credential_version  INT NOT NULL DEFAULT 1,
    permissions         JSONB NOT NULL DEFAULT '[]'::jsonb,
    masked_preview      TEXT NOT NULL DEFAULT '••••••••',
    created_by          TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT test_credentials_tenant_name_key UNIQUE (tenant_id, name)
);
CREATE INDEX IF NOT EXISTS test_credentials_tenant_idx ON test_credentials(tenant_id);

CREATE TABLE IF NOT EXISTS test_runs (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    suite_id                  UUID NOT NULL REFERENCES test_suites(id) ON DELETE CASCADE,
    suite_version_id          UUID REFERENCES test_suite_versions(id) ON DELETE SET NULL,
    suite_content_hash        TEXT NOT NULL DEFAULT '',
    environment_id            UUID REFERENCES test_environments(id) ON DELETE SET NULL,
    actor                     TEXT NOT NULL DEFAULT '',
    mode                      TEXT NOT NULL DEFAULT 'standard',
    lifecycle_state           TEXT NOT NULL DEFAULT 'queued',
    total_steps               INT NOT NULL DEFAULT 0,
    passed_steps              INT NOT NULL DEFAULT 0,
    failed_steps              INT NOT NULL DEFAULT 0,
    skipped_steps             INT NOT NULL DEFAULT 0,
    started_at                TIMESTAMPTZ,
    completed_at              TIMESTAMPTZ,
    cancelled_at              TIMESTAMPTZ,
    failure_reason            TEXT NOT NULL DEFAULT '',
    immutable_inputs          JSONB NOT NULL DEFAULT '{}'::jsonb,
    actual_revision           BIGINT NOT NULL DEFAULT 0,
    actual_candidate_revision  BIGINT NOT NULL DEFAULT 0,
    comparison_summary        JSONB NOT NULL DEFAULT '{}'::jsonb,
    expires_at                TIMESTAMPTZ,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS test_runs_tenant_idx ON test_runs(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS test_runs_suite_idx ON test_runs(suite_id, created_at DESC);
CREATE INDEX IF NOT EXISTS test_runs_state_idx ON test_runs(lifecycle_state, created_at);

CREATE TABLE IF NOT EXISTS test_run_steps (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id             UUID NOT NULL REFERENCES test_runs(id) ON DELETE CASCADE,
    step_index         INT NOT NULL,
    cohort             TEXT NOT NULL DEFAULT 'baseline',
    request_name       TEXT NOT NULL DEFAULT '',
    method             TEXT NOT NULL,
    url                TEXT NOT NULL,
    target_revision    BIGINT NOT NULL DEFAULT 0,
    observed_revision  BIGINT NOT NULL DEFAULT 0,
    duration_ms        DOUBLE PRECISION NOT NULL DEFAULT 0,
    status_code        INT NOT NULL DEFAULT 0,
    assertion_results  JSONB NOT NULL DEFAULT '[]'::jsonb,
    decision_policy    TEXT NOT NULL DEFAULT '',
    decision_reason    TEXT NOT NULL DEFAULT '',
    redacted_request   JSONB NOT NULL DEFAULT '{}'::jsonb,
    redacted_response  JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT test_run_steps_run_step_cohort_key UNIQUE (run_id, step_index, cohort)
);
CREATE INDEX IF NOT EXISTS test_run_steps_run_idx ON test_run_steps(run_id, step_index ASC, cohort);

CREATE TABLE IF NOT EXISTS test_run_artifacts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL REFERENCES test_runs(id) ON DELETE CASCADE,
    artifact_type   TEXT NOT NULL DEFAULT 'log',
    storage_key     TEXT NOT NULL,
    digest          TEXT NOT NULL,
    byte_size       BIGINT NOT NULL DEFAULT 0,
    content_type    TEXT NOT NULL DEFAULT 'application/json',
    retention_until TIMESTAMPTZ NOT NULL,
    deleted         BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS test_run_artifacts_retention_idx ON test_run_artifacts(retention_until) WHERE NOT deleted;

CREATE TABLE IF NOT EXISTS test_jobs (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id             UUID NOT NULL REFERENCES test_runs(id) ON DELETE CASCADE,
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    status             TEXT NOT NULL DEFAULT 'pending',
    claim_lease_until  TIMESTAMPTZ,
    heartbeat_at       TIMESTAMPTZ,
    attempts           INT NOT NULL DEFAULT 0,
    worker_id          TEXT NOT NULL DEFAULT '',
    next_available_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS test_jobs_claim_idx ON test_jobs(status, next_available_at) WHERE status IN ('pending', 'leased');

CREATE TABLE IF NOT EXISTS test_gate_policies (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    api_id              UUID REFERENCES apis(id) ON DELETE CASCADE,
    target_environment  TEXT NOT NULL DEFAULT 'canary',
    required_suite_ids  JSONB NOT NULL DEFAULT '[]'::jsonb,
    freshness_seconds   INT NOT NULL DEFAULT 3600,
    enforcement_enabled BOOLEAN NOT NULL DEFAULT false,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT test_gate_policies_scope_key UNIQUE (tenant_id, api_id, target_environment)
);
CREATE INDEX IF NOT EXISTS test_gate_policies_tenant_idx ON test_gate_policies(tenant_id, api_id);

CREATE TABLE IF NOT EXISTS test_gate_evidence (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    api_id             UUID REFERENCES apis(id) ON DELETE CASCADE,
    revision           BIGINT NOT NULL,
    run_id             UUID REFERENCES test_runs(id) ON DELETE SET NULL,
    target_fingerprint TEXT NOT NULL,
    eligible           BOOLEAN NOT NULL DEFAULT false,
    reasons            JSONB NOT NULL DEFAULT '[]'::jsonb,
    verified_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT test_gate_evidence_unique UNIQUE (tenant_id, api_id, revision, run_id)
);
CREATE INDEX IF NOT EXISTS test_gate_evidence_lookup_idx ON test_gate_evidence(tenant_id, api_id, revision);
