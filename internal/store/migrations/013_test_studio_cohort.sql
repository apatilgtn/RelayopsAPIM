-- 013_test_studio_cohort.sql
-- Add cohort column to test_run_steps to allow distinct storage of baseline and candidate execution results.

ALTER TABLE test_run_steps ADD COLUMN IF NOT EXISTS cohort TEXT NOT NULL DEFAULT 'baseline';
ALTER TABLE test_run_steps DROP CONSTRAINT IF EXISTS test_run_steps_run_step_key;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'test_run_steps_run_step_cohort_key'
    ) THEN
        ALTER TABLE test_run_steps ADD CONSTRAINT test_run_steps_run_step_cohort_key UNIQUE (run_id, step_index, cohort);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS test_run_steps_run_cohort_idx ON test_run_steps(run_id, step_index ASC, cohort);
