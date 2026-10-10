-- 002_advanced_features.sql
-- Adds OIDC/JWKS support, daily/monthly quotas, AI gateway flags, OpenAPI specs, and LLM token tracking.

ALTER TABLE apis DROP CONSTRAINT IF EXISTS apis_auth_type_check;
ALTER TABLE apis ADD CONSTRAINT apis_auth_type_check CHECK (auth_type IN ('none', 'api_key', 'jwt', 'oidc'));

ALTER TABLE apis
    ADD COLUMN IF NOT EXISTS jwks_url          TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS oidc_issuer       TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS oidc_audience     TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS quota_per_day     INT  NOT NULL DEFAULT 0 CHECK (quota_per_day >= 0),
    ADD COLUMN IF NOT EXISTS quota_per_month   INT  NOT NULL DEFAULT 0 CHECK (quota_per_month >= 0),
    ADD COLUMN IF NOT EXISTS is_ai             BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS openapi_spec      JSONB NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE plans
    ADD COLUMN IF NOT EXISTS quota_per_day     INT  NOT NULL DEFAULT 0 CHECK (quota_per_day >= 0),
    ADD COLUMN IF NOT EXISTS quota_per_month   INT  NOT NULL DEFAULT 0 CHECK (quota_per_month >= 0);

UPDATE plans SET quota_per_day = 1000, quota_per_month = 20000 WHERE name = 'Free' AND quota_per_day = 0;
UPDATE plans SET quota_per_day = 50000, quota_per_month = 1000000 WHERE name = 'Pro' AND quota_per_day = 0;

ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS model             TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS tokens_prompt     INT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS tokens_completion INT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS tokens_total      INT  NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS request_logs_model_idx ON request_logs (model) WHERE model <> '';

-- Auto-seed NVIDIA AI Gateway API if not existing
INSERT INTO apis (name, description, base_path, upstream_url, strip_path, auth_type, rate_limit_per_minute, quota_per_day, quota_per_month, timeout_ms, cors_enabled, request_headers, is_ai, enabled)
VALUES (
    'nvidia-nim',
    'NVIDIA NIM AI Models (Llama 3.1, Mistral, Nemotron) with secret injection & streaming token tracking',
    '/ai/nvidia',
    'https://integrate.api.nvidia.com/v1',
    true,
    'api_key',
    60,
    500,
    10000,
    120000,
    true,
    '{"Authorization": "Bearer ${secret:NVIDIA_API_KEY}"}'::jsonb,
    true,
    true
)
ON CONFLICT (name) DO NOTHING;
