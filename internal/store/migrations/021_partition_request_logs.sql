-- 021_partition_request_logs.sql
-- request_logs becomes a table partitioned by day (UTC) on ts. Retention then
-- drops whole partitions instead of DELETEing rows, which keeps vacuum and
-- index bloat out of the hot write path.
--
-- * Unique keys must include the partition key: the primary key becomes
--   (id, ts) and replay idempotency uses (log_id, ts). A log's ts is fixed
--   when it is recorded, so a redelivered log still conflicts.
-- * relayops_ensure_log_partitions(from, to) creates daily partitions; the
--   retention job calls it so partitions always exist ahead of time. Rows
--   outside every partition land in request_logs_default.
-- * Existing rows are copied in this transaction. On very large installs run
--   this release in a maintenance window, or purge old logs first.

CREATE OR REPLACE FUNCTION relayops_ensure_log_partitions(from_day date, to_day date)
RETURNS integer LANGUAGE plpgsql AS $fn$
DECLARE
    d date := from_day;
    created integer := 0;
    part text;
BEGIN
    WHILE d <= to_day LOOP
        part := 'request_logs_p' || to_char(d, 'YYYYMMDD');
        IF to_regclass(part) IS NULL THEN
            BEGIN
                EXECUTE format(
                    'CREATE TABLE %I PARTITION OF request_logs FOR VALUES FROM (%L) TO (%L)',
                    part,
                    (d::timestamp AT TIME ZONE 'UTC'),
                    ((d + 1)::timestamp AT TIME ZONE 'UTC'));
                created := created + 1;
            EXCEPTION
                -- The default partition already holds rows for this day, or a
                -- concurrent node created it first: leave the rows where they are.
                WHEN check_violation OR duplicate_table THEN NULL;
            END;
        END IF;
        d := d + 1;
    END LOOP;
    RETURN created;
END;
$fn$;

DO $$
DECLARE
    first_day date;
BEGIN
    IF EXISTS (SELECT 1 FROM pg_partitioned_table pt JOIN pg_class c ON c.oid = pt.partrelid
               WHERE c.relname = 'request_logs' AND c.relnamespace = 'public'::regnamespace) THEN
        RETURN; -- already partitioned
    END IF;

    ALTER TABLE request_logs RENAME TO request_logs_unpartitioned;
    ALTER INDEX IF EXISTS request_logs_pkey RENAME TO request_logs_unpartitioned_pkey;
    ALTER INDEX IF EXISTS request_logs_ts_idx RENAME TO request_logs_unpartitioned_ts_idx;
    ALTER INDEX IF EXISTS request_logs_api_ts_idx RENAME TO request_logs_unpartitioned_api_ts_idx;
    ALTER INDEX IF EXISTS request_logs_model_idx RENAME TO request_logs_unpartitioned_model_idx;
    ALTER INDEX IF EXISTS request_logs_decision_idx RENAME TO request_logs_unpartitioned_decision_idx;
    ALTER INDEX IF EXISTS request_logs_rev_idx RENAME TO request_logs_unpartitioned_rev_idx;
    ALTER INDEX IF EXISTS request_logs_rev_ts_idx RENAME TO request_logs_unpartitioned_rev_ts_idx;
    ALTER INDEX IF EXISTS request_logs_log_id_uq RENAME TO request_logs_unpartitioned_log_id_uq;
    ALTER INDEX IF EXISTS request_logs_tenant_ts_idx RENAME TO request_logs_unpartitioned_tenant_ts_idx;

    -- Same columns, order and defaults (including id's sequence).
    CREATE TABLE request_logs (LIKE request_logs_unpartitioned INCLUDING DEFAULTS) PARTITION BY RANGE (ts);
    ALTER SEQUENCE request_logs_id_seq OWNED BY request_logs.id;

    ALTER TABLE request_logs ADD CONSTRAINT request_logs_pkey PRIMARY KEY (id, ts);
    CREATE UNIQUE INDEX request_logs_log_id_uq ON request_logs (log_id, ts);
    CREATE INDEX request_logs_ts_idx ON request_logs (ts DESC);
    CREATE INDEX request_logs_api_ts_idx ON request_logs (api_id, ts DESC);
    CREATE INDEX request_logs_model_idx ON request_logs (model) WHERE model <> '';
    CREATE INDEX request_logs_decision_idx ON request_logs (decision_reason) WHERE decision_reason <> '';
    CREATE INDEX request_logs_rev_idx ON request_logs (config_revision);
    CREATE INDEX request_logs_rev_ts_idx ON request_logs (config_revision, ts);
    CREATE INDEX request_logs_tenant_ts_idx ON request_logs (tenant_id, ts DESC);

    CREATE TABLE request_logs_default PARTITION OF request_logs DEFAULT;

    -- Partitions for every day that has rows, plus a week ahead, before copying.
    SELECT LEAST(COALESCE(min(ts)::date, current_date), current_date) INTO first_day FROM request_logs_unpartitioned;
    PERFORM relayops_ensure_log_partitions(first_day, (now() AT TIME ZONE 'UTC')::date + 7);

    INSERT INTO request_logs SELECT * FROM request_logs_unpartitioned;
    DROP TABLE request_logs_unpartitioned;
END $$;
