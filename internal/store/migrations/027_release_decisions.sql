-- Refuse ambiguous existing installations rather than silently choose a canary.
CREATE UNIQUE INDEX config_revisions_single_canary ON config_revisions(status) WHERE status='canary';
CREATE TABLE release_decisions (
 revision BIGINT PRIMARY KEY REFERENCES config_revisions(revision),
 evidence JSONB NOT NULL,
 evidence_digest TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE release_decisions ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON release_decisions FROM PUBLIC;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='anon') THEN REVOKE ALL ON release_decisions FROM anon; END IF;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='authenticated') THEN REVOKE ALL ON release_decisions FROM authenticated; END IF;
END $$;
