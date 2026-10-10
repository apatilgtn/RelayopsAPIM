-- 018_dataplane_node_credentials.sql
-- Per-node credentials for gateway-only nodes calling the control plane's
-- node API. Each token is bound to one node ID, stored only as a SHA-256
-- hash, and can be revoked or expire. Replaces the shared node token, which
-- keeps working while it is configured.

CREATE TABLE IF NOT EXISTS dataplane_node_credentials (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id       TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    token_hash    TEXT NOT NULL UNIQUE,
    created_by    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    last_seen_at  TIMESTAMPTZ,
    last_seen_ip  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS dataplane_node_credentials_node_idx ON dataplane_node_credentials (node_id);
