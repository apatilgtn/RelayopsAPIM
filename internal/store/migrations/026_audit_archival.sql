CREATE TABLE audit_archives (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 digest TEXT NOT NULL,
 record_ids BIGINT[] NOT NULL,
 created_by TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 purged_at TIMESTAMPTZ
);
ALTER TABLE audit_archives ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON audit_archives FROM PUBLIC;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='anon') THEN REVOKE ALL ON audit_archives FROM anon; END IF;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='authenticated') THEN REVOKE ALL ON audit_archives FROM authenticated; END IF;
END $$;
