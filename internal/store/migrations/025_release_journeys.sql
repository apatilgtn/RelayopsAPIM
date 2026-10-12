CREATE TABLE release_drafts (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 tenant_id UUID NOT NULL REFERENCES tenants(id),
 version INT NOT NULL DEFAULT 1,
 configuration JSONB NOT NULL,
 suite_ids JSONB NOT NULL DEFAULT '[]',
 environment_id UUID REFERENCES test_environments(id),
 baseline_revision BIGINT NOT NULL DEFAULT 0,
 plan_hash TEXT NOT NULL DEFAULT '',
 candidate_revision BIGINT NOT NULL DEFAULT 0,
 deployment_id UUID REFERENCES apiops_deployments(id),
 created_by TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX release_drafts_tenant_idx ON release_drafts(tenant_id, updated_at DESC);
CREATE INDEX release_drafts_environment_idx ON release_drafts(environment_id);
CREATE INDEX release_drafts_deployment_idx ON release_drafts(deployment_id);
CREATE TABLE consumer_connections (
 consumer_id UUID NOT NULL REFERENCES consumers(id) ON DELETE CASCADE,
 api_id UUID NOT NULL REFERENCES apis(id) ON DELETE CASCADE,
 first_success_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 request_id TEXT NOT NULL,
 PRIMARY KEY(consumer_id,api_id)
);
CREATE INDEX consumer_connections_api_idx ON consumer_connections(api_id);
-- Only the RelayOps database owner/backend accesses these records. No Data API policies.
ALTER TABLE release_drafts ENABLE ROW LEVEL SECURITY;
ALTER TABLE consumer_connections ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON release_drafts,consumer_connections FROM PUBLIC;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='anon') THEN
  REVOKE ALL ON release_drafts,consumer_connections FROM anon;
 END IF;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='authenticated') THEN
  REVOKE ALL ON release_drafts,consumer_connections FROM authenticated;
 END IF;
END $$;
