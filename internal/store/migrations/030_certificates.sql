-- Certificate management (docs/CERTIFICATE_MANAGEMENT.md).

-- The certificates each node holds, reported with its acknowledgements and
-- heartbeats: metadata only (subject, issuer, serial, validity, fingerprint),
-- never keys.
CREATE TABLE IF NOT EXISTS node_certificates (
    node_id      TEXT PRIMARY KEY,
    certificates JSONB NOT NULL DEFAULT '[]'::jsonb,
    reported_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Expiry alerts already sent, so control-plane replicas send each one once a day.
CREATE TABLE IF NOT EXISTS certificate_alerts (
    fingerprint TEXT NOT NULL,
    severity    TEXT NOT NULL,
    alerted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (fingerprint, severity)
);

-- ACME account key and certificates shared by every node with database
-- access. Values are sealed with AES-256-GCM by the nodes
-- (RELAYOPS_ACME_CACHE_KEY); the database never holds a usable key.
CREATE TABLE IF NOT EXISTS acme_cache (
    cache_key  TEXT PRIMARY KEY,
    data       BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Which node is ordering a certificate, so nodes sharing the cache order
-- each hostname once. A lease expires so a node that died mid-order does not
-- block issuance.
CREATE TABLE IF NOT EXISTS acme_order_leases (
    host       TEXT PRIMARY KEY,
    holder     TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
