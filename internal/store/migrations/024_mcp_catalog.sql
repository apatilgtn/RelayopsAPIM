-- 024_mcp_catalog.sql
-- The MCP catalog: every tool and prompt definition seen on an MCP API (by
-- gateways or by discovery), with its review status. Pending and rejected
-- definitions that differ from the approved pin are blocked fleet-wide;
-- approved definitions carry the input schema used to validate arguments.

CREATE TABLE IF NOT EXISTS mcp_catalog (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    api_id      UUID NOT NULL REFERENCES apis(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('tool', 'prompt')),
    name        TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    definition  JSONB NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'resolved')),
    source      TEXT NOT NULL DEFAULT 'observed' CHECK (source IN ('observed', 'discovered')),
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    seen_by     TEXT NOT NULL DEFAULT '',
    decided_by  TEXT NOT NULL DEFAULT '',
    decided_at  TIMESTAMPTZ,
    UNIQUE (api_id, kind, name, fingerprint)
);
CREATE INDEX IF NOT EXISTS mcp_catalog_status_idx ON mcp_catalog (status, tenant_id);

-- Gateways reload only when an entry appears or its status really changes,
-- not on every last_seen refresh (row-level, so unchanged rows fire nothing).
DROP TRIGGER IF EXISTS mcp_catalog_insert_notify ON mcp_catalog;
CREATE TRIGGER mcp_catalog_insert_notify AFTER INSERT ON mcp_catalog
    FOR EACH ROW EXECUTE FUNCTION relayops_notify_config();
DROP TRIGGER IF EXISTS mcp_catalog_status_notify ON mcp_catalog;
CREATE TRIGGER mcp_catalog_status_notify AFTER UPDATE OF status ON mcp_catalog
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION relayops_notify_config();
