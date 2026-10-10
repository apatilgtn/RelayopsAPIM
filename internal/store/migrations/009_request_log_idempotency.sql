-- 009_request_log_idempotency.sql
-- Every request log carries a unique event ID assigned by the gateway, so a batch
-- that is delivered twice (spool replay after a lost acknowledgement, retries
-- across pod restarts) is stored once. Existing rows keep a NULL log_id; the
-- partial index avoids rewriting large log tables during the upgrade.
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS log_id UUID;
CREATE UNIQUE INDEX IF NOT EXISTS request_logs_log_id_uq ON request_logs (log_id) WHERE log_id IS NOT NULL;
