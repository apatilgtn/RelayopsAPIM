-- 019_request_log_sample_weight.sql
-- Request-log sampling. Gateways may persist only a fraction of successful
-- requests (RELAYOPS_LOG_SAMPLE_RATE); each kept row carries the number of
-- requests it stands for. Errors are always kept with weight 1. Aggregates sum
-- sample_weight instead of counting rows. Existing rows are unsampled (1).

ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS sample_weight REAL NOT NULL DEFAULT 1;
