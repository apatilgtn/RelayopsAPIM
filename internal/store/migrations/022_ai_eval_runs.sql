-- 022_ai_eval_runs.sql
-- Server-executed AI evaluation runs: the control plane sends each suite case
-- through the gateway to the release manifest's model, scores the responses
-- and stores the results. Qualifying a manifest with one of these runs makes
-- the evidence server-verified instead of caller-attested.

CREATE TABLE IF NOT EXISTS ai_eval_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    suite_id        UUID NOT NULL REFERENCES ai_eval_suites(id) ON DELETE CASCADE,
    manifest_id     UUID NOT NULL REFERENCES ai_release_manifests(id) ON DELETE CASCADE,
    status          TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'completed', 'failed')),
    min_pass_rate   DOUBLE PRECISION NOT NULL DEFAULT 0.9,
    summary         JSONB NOT NULL DEFAULT '{}'::jsonb,
    error           TEXT NOT NULL DEFAULT '',
    evidence_digest TEXT NOT NULL DEFAULT '',
    created_by      TEXT NOT NULL DEFAULT '',
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ai_eval_runs_manifest_idx ON ai_eval_runs (manifest_id, started_at DESC);
