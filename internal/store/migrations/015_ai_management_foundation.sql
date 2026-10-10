-- 015_ai_management_foundation.sql
-- RelayOps AI Management: provider connections, model deployments, AI services,
-- atomic budget ledger, evaluation suites, release manifests, and agent tool grants.

-- 1. Provider Connections
CREATE TABLE IF NOT EXISTS ai_provider_connections (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    provider_type      TEXT NOT NULL CHECK (provider_type IN ('openai', 'anthropic', 'ollama', 'azure_openai', 'custom')),
    base_url           TEXT NOT NULL,
    api_key_secret_ref TEXT NOT NULL DEFAULT '',
    capabilities       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ai_provider_connections_tenant_idx ON ai_provider_connections(tenant_id);

-- 2. Model Deployments
CREATE TABLE IF NOT EXISTS ai_model_deployments (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    connection_id            UUID NOT NULL REFERENCES ai_provider_connections(id) ON DELETE CASCADE,
    model_name               TEXT NOT NULL,
    deployment_name          TEXT NOT NULL,
    context_window_tokens    INT NOT NULL DEFAULT 128000 CHECK (context_window_tokens > 0),
    max_output_tokens        INT NOT NULL DEFAULT 4096 CHECK (max_output_tokens > 0),
    input_price_per_million  NUMERIC(10, 4) NOT NULL DEFAULT 0,
    output_price_per_million NUMERIC(10, 4) NOT NULL DEFAULT 0,
    enabled                  BOOLEAN NOT NULL DEFAULT true,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ai_model_deployments_tenant_idx ON ai_model_deployments(tenant_id);

-- 3. AI Services
CREATE TABLE IF NOT EXISTS ai_services (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    api_id                      UUID REFERENCES apis(id) ON DELETE SET NULL,
    name                        TEXT NOT NULL,
    alias                       TEXT NOT NULL,
    primary_model_deployment_id UUID REFERENCES ai_model_deployments(id) ON DELETE SET NULL,
    allowed_models              JSONB NOT NULL DEFAULT '[]'::jsonb,
    routing_policy              TEXT NOT NULL DEFAULT 'single' CHECK (routing_policy IN ('single', 'fallback', 'balanced')),
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ai_services_tenant_idx ON ai_services(tenant_id);

-- 4. AI Budget Accounts (atomic reservation ledger)
CREATE TABLE IF NOT EXISTS ai_budget_accounts (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    consumer_id          UUID REFERENCES consumers(id) ON DELETE CASCADE,
    currency             TEXT NOT NULL DEFAULT 'USD',
    monthly_budget_cents BIGINT NOT NULL DEFAULT 0 CHECK (monthly_budget_cents >= 0),
    current_spend_cents  BIGINT NOT NULL DEFAULT 0 CHECK (current_spend_cents >= 0),
    reserved_spend_cents BIGINT NOT NULL DEFAULT 0 CHECK (reserved_spend_cents >= 0),
    strict_enforcement   BOOLEAN NOT NULL DEFAULT true,
    reset_day_of_month   INT NOT NULL DEFAULT 1 CHECK (reset_day_of_month >= 1 AND reset_day_of_month <= 31),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ai_budget_accounts_tenant_idx ON ai_budget_accounts(tenant_id);

-- 5. AI Budget Reservations (transactional pre-call reservations)
CREATE TABLE IF NOT EXISTS ai_budget_reservations (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id     UUID NOT NULL REFERENCES ai_budget_accounts(id) ON DELETE CASCADE,
    request_id     TEXT NOT NULL UNIQUE,
    reserved_cents BIGINT NOT NULL CHECK (reserved_cents >= 0),
    settled_cents  BIGINT NOT NULL DEFAULT 0 CHECK (settled_cents >= 0),
    status         TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'settled', 'released', 'expired')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '5 minutes')
);

CREATE INDEX IF NOT EXISTS ai_budget_reservations_acc_idx ON ai_budget_reservations(account_id);

-- 6. AI Evaluation Suites (for evaluated releases)
CREATE TABLE IF NOT EXISTS ai_eval_suites (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    service_id  UUID REFERENCES ai_services(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    rubric      JSONB NOT NULL DEFAULT '{}'::jsonb,
    test_cases  JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ai_eval_suites_tenant_idx ON ai_eval_suites(tenant_id);

-- 7. AI Release Manifests (immutable bindings of model, prompt, policy, and qualification proof)
CREATE TABLE IF NOT EXISTS ai_release_manifests (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    service_id           UUID NOT NULL REFERENCES ai_services(id) ON DELETE CASCADE,
    version              INT NOT NULL DEFAULT 1,
    model_deployment_id  UUID NOT NULL REFERENCES ai_model_deployments(id) ON DELETE CASCADE,
    system_prompt        TEXT NOT NULL DEFAULT '',
    eval_run_id          TEXT,
    qualification_status TEXT NOT NULL DEFAULT 'draft' CHECK (qualification_status IN ('draft', 'qualified', 'rejected', 'active')),
    evidence_digest      TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ai_release_manifests_service_idx ON ai_release_manifests(service_id);

-- 8. AI Agent Grants & MCP Tools
CREATE TABLE IF NOT EXISTS ai_agent_grants (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    consumer_id              UUID REFERENCES consumers(id) ON DELETE CASCADE,
    agent_name               TEXT NOT NULL,
    allowed_tools            JSONB NOT NULL DEFAULT '[]'::jsonb,
    max_tool_calls_per_run   INT NOT NULL DEFAULT 20 CHECK (max_tool_calls_per_run > 0),
    require_human_approval   BOOLEAN NOT NULL DEFAULT true,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ai_agent_grants_tenant_idx ON ai_agent_grants(tenant_id);
