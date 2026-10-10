-- 006_release_safety_and_traffic.sql
-- Release safety (revision-isolated policy, coordinated auto-rollback, traffic-split canaries)
-- and upstream traffic management (multi-target load balancing, retries, circuit breakers,
-- active health checks), plus distributed trace correlation on request logs.

-- 1. Upstream traffic policy: targets, load balancing, retries, circuit breaker, health checks.
ALTER TABLE apis
    ADD COLUMN IF NOT EXISTS traffic_policy JSONB NOT NULL DEFAULT '{}'::jsonb;

-- 2. Traffic-split canaries: a canary revision can receive a percentage of traffic and/or
--    every request carrying a routing header, on every gateway node (not only canary nodes).
ALTER TABLE config_revisions
    ADD COLUMN IF NOT EXISTS traffic_percent INT  NOT NULL DEFAULT 0 CHECK (traffic_percent BETWEEN 0 AND 100),
    ADD COLUMN IF NOT EXISTS canary_header   TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS canary_header_value TEXT NOT NULL DEFAULT '';

-- Nodes report the canary revision they hold alongside their stable revision.
ALTER TABLE node_acknowledgements
    ADD COLUMN IF NOT EXISTS canary_revision BIGINT NOT NULL DEFAULT 0;

-- 3. Durable, cluster-wide auto-rollback settings and state (single row).
CREATE TABLE IF NOT EXISTS auto_rollback_settings (
    id                            INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    enabled                       BOOLEAN NOT NULL DEFAULT true,
    error_rate_threshold_percent  DOUBLE PRECISION NOT NULL DEFAULT 5.0 CHECK (error_rate_threshold_percent > 0 AND error_rate_threshold_percent <= 100),
    evaluation_window_seconds     INT NOT NULL DEFAULT 60  CHECK (evaluation_window_seconds BETWEEN 10 AND 86400),
    min_requests                  INT NOT NULL DEFAULT 5   CHECK (min_requests >= 1),
    cooldown_seconds              INT NOT NULL DEFAULT 60  CHECK (cooldown_seconds >= 0),
    cooldown_until                TIMESTAMPTZ,
    last_triggered_revision       BIGINT NOT NULL DEFAULT 0,
    last_triggered_at             TIMESTAMPTZ,
    last_triggered_reason         TEXT NOT NULL DEFAULT '',
    last_evaluated_at             TIMESTAMPTZ,
    last_evaluated_by             TEXT NOT NULL DEFAULT '',
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by                    TEXT NOT NULL DEFAULT ''
);
INSERT INTO auto_rollback_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

-- 4. Trace correlation for request diagnosis.
ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS trace_id TEXT NOT NULL DEFAULT '';

-- Fleet-wide rollback evaluation aggregates by revision over a recent window.
CREATE INDEX IF NOT EXISTS request_logs_rev_ts_idx ON request_logs (config_revision, ts);

-- 5. Fresh installs seed revision 1 with an explicit ID (migration 003), which left the
--    BIGSERIAL sequence behind and made the first publish fail with a duplicate key.
SELECT setval(pg_get_serial_sequence('config_revisions', 'revision'),
              GREATEST((SELECT COALESCE(max(revision), 1) FROM config_revisions), 1));
