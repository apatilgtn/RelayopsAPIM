-- =============================================================================
-- RelayOps APIM: Supabase Egress & Storage Optimization Script
-- Project: Relay Ops AI (moxleagjsaagypnwpgyp)
-- Purpose: Purge bloated access logs/test steps, disable accidental Realtime
--          replication on telemetry tables, and set up automated daily pruning.
-- Run in: Supabase Dashboard -> SQL Editor (https://supabase.com/dashboard/project/moxleagjsaagypnwpgyp/sql/new)
-- =============================================================================

-- -----------------------------------------------------------------------------
-- 1. INSPECT CURRENT STORAGE BREAKDOWN
-- -----------------------------------------------------------------------------
SELECT 
    schemaname,
    relname AS table_name,
    n_live_tup AS estimated_rows,
    pg_size_pretty(pg_total_relation_size(relid)) AS total_size,
    pg_size_pretty(pg_relation_size(relid)) AS data_size,
    pg_size_pretty(pg_total_relation_size(relid) - pg_relation_size(relid)) AS index_size
FROM pg_stat_user_tables
WHERE schemaname = 'public'
ORDER BY pg_total_relation_size(relid) DESC;

-- -----------------------------------------------------------------------------
-- 2. REMOVE TELEMETRY TABLES FROM SUPABASE REALTIME PUBLICATION
-- Critical: Supabase Realtime broadcasts every INSERT on replicated tables
-- over WebSockets, which can consume hundreds of megabytes of egress per hour!
-- -----------------------------------------------------------------------------
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_publication WHERE pubname = 'supabase_realtime') THEN
        -- Safely drop high-throughput telemetry tables from realtime broadcast
        BEGIN
            ALTER PUBLICATION supabase_realtime DROP TABLE public.request_logs;
            RAISE NOTICE 'Removed request_logs from supabase_realtime publication.';
        EXCEPTION WHEN OTHERS THEN
            -- Table was not in publication
        END;

        BEGIN
            ALTER PUBLICATION supabase_realtime DROP TABLE public.test_run_steps;
            RAISE NOTICE 'Removed test_run_steps from supabase_realtime publication.';
        EXCEPTION WHEN OTHERS THEN
            -- Table was not in publication
        END;

        BEGIN
            ALTER PUBLICATION supabase_realtime DROP TABLE public.test_jobs;
            RAISE NOTICE 'Removed test_jobs from supabase_realtime publication.';
        EXCEPTION WHEN OTHERS THEN
            -- Table was not in publication
        END;
    END IF;
END $$;

-- -----------------------------------------------------------------------------
-- 3. PURGE HISTORICAL LOGS & TEST RUN ARTIFACTS
-- -----------------------------------------------------------------------------
-- Prune access logs older than 24 hours
DELETE FROM public.request_logs 
WHERE ts < (now() - interval '24 hours');

-- Prune test run step execution payloads older than 7 days
DELETE FROM public.test_run_steps 
WHERE created_at < (now() - interval '7 days');

-- Prune test runs older than 7 days
DELETE FROM public.test_runs 
WHERE completed_at IS NOT NULL 
  AND completed_at < (now() - interval '7 days');

-- Clean up any orphaned test jobs
DELETE FROM public.test_jobs 
WHERE status IN ('completed', 'failed', 'interrupted') 
  AND created_at < (now() - interval '7 days');

-- -----------------------------------------------------------------------------
-- 4. VACUUM AND RECLAIM STORAGE (OPTIONAL)
-- Note: Supabase SQL Editor wraps multi-statement scripts in transactions,
-- where VACUUM cannot run. Postgres autovacuum will automatically clean up dead
-- tuples in the background. If you want to force VACUUM immediately, run these
-- one line at a time in a new SQL tab.
-- -----------------------------------------------------------------------------
-- VACUUM (ANALYZE) public.request_logs;
-- VACUUM (ANALYZE) public.test_run_steps;
-- VACUUM (ANALYZE) public.test_runs;
-- VACUUM (ANALYZE) public.test_jobs;

-- -----------------------------------------------------------------------------
-- 5. SCHEDULE AUTOMATED DAILY CLEANUP VIA PG_CRON (BUILT INTO SUPABASE)
-- Runs every morning at 03:00 UTC to permanently enforce < 200 MB storage.
-- -----------------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS pg_cron;

-- Unschedule prior job if present
SELECT cron.unschedule(jobid) 
FROM cron.job 
WHERE jobname = 'relayops-daily-telemetry-purge';

-- Schedule the automated daily cleanup
SELECT cron.schedule(
    'relayops-daily-telemetry-purge',
    '0 3 * * *', -- Daily at 03:00 UTC
    $$
        DELETE FROM public.request_logs WHERE ts < (now() - interval '24 hours');
        DELETE FROM public.test_run_steps WHERE created_at < (now() - interval '7 days');
        DELETE FROM public.test_runs WHERE completed_at IS NOT NULL AND completed_at < (now() - interval '7 days');
        DELETE FROM public.test_jobs WHERE status IN ('completed', 'failed', 'interrupted') AND created_at < (now() - interval '7 days');
    $$
);

-- -----------------------------------------------------------------------------
-- 6. VERIFY POST-CLEANUP STORAGE
-- -----------------------------------------------------------------------------
SELECT 
    'Cleanup Complete' AS status,
    pg_size_pretty(sum(pg_total_relation_size(relid))) AS total_database_size
FROM pg_stat_user_tables
WHERE schemaname = 'public';
