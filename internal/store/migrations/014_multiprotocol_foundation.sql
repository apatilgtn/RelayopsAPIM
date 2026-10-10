-- 014_multiprotocol_foundation.sql
-- Foundation for multi-protocol gateway management: explicit API protocol,
-- GraphQL governance (schema/depth/cost), gRPC policies, and stream services.

-- 1. Explicit API application protocol (default 'http')
ALTER TABLE apis ADD COLUMN IF NOT EXISTS protocol TEXT NOT NULL DEFAULT 'http';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'apis_protocol_check'
    ) THEN
        ALTER TABLE apis ADD CONSTRAINT apis_protocol_check CHECK (protocol IN ('http', 'graphql', 'grpc'));
    END IF;
END $$;

-- 2. GraphQL and gRPC schema and governance policies
ALTER TABLE apis ADD COLUMN IF NOT EXISTS graphql_schema TEXT NOT NULL DEFAULT '';
ALTER TABLE apis ADD COLUMN IF NOT EXISTS graphql_policy JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE apis ADD COLUMN IF NOT EXISTS grpc_descriptor_set BYTEA;
ALTER TABLE apis ADD COLUMN IF NOT EXISTS grpc_policy JSONB NOT NULL DEFAULT '{}'::jsonb;

-- 3. Dedicated Stream Services (TCP/TLS byte proxying)
CREATE TABLE IF NOT EXISTS stream_services (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    listen_port        INT NOT NULL UNIQUE CHECK (listen_port > 0 AND listen_port < 65536),
    target_addresses   TEXT[] NOT NULL,
    tls_mode           TEXT NOT NULL DEFAULT 'passthrough' CHECK (tls_mode IN ('none', 'terminate', 'passthrough')),
    max_connections    INT NOT NULL DEFAULT 1000 CHECK (max_connections > 0),
    connect_timeout_ms INT NOT NULL DEFAULT 5000 CHECK (connect_timeout_ms > 0),
    enabled            BOOLEAN NOT NULL DEFAULT true,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS stream_services_tenant_idx ON stream_services(tenant_id);
