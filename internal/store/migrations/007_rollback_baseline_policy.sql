-- 007_rollback_baseline_policy.sql
-- Explicit handling of insufficient baseline traffic in automatic rollback decisions.
ALTER TABLE auto_rollback_settings
    ADD COLUMN IF NOT EXISTS min_baseline_requests INT NOT NULL DEFAULT 20 CHECK (min_baseline_requests >= 0),
    ADD COLUMN IF NOT EXISTS insufficient_baseline_action TEXT NOT NULL DEFAULT 'absolute'
        CHECK (insufficient_baseline_action IN ('absolute', 'hold'));
