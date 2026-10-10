-- 023_mcp_gateway.sql
-- Model Context Protocol (MCP) servers as a gateway protocol: per-tool
-- authorisation, an approved (pinned) tool catalog and per-tool limits.

ALTER TABLE apis ADD COLUMN IF NOT EXISTS mcp_policy JSONB NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE apis DROP CONSTRAINT IF EXISTS apis_protocol_check;
ALTER TABLE apis ADD CONSTRAINT apis_protocol_check CHECK (protocol IN ('http', 'graphql', 'grpc', 'mcp'));
