-- 017_oidc_state_consumed_marker.sql
-- Sign-in states are marked consumed instead of deleted, so the callback can
-- verify the browser-binding cookie against the stored state (including a
-- replay of an already used one) before it consumes it. Expired rows are
-- still removed when new states are saved.

ALTER TABLE oidc_login_states ADD COLUMN IF NOT EXISTS consumed_at TIMESTAMPTZ;
