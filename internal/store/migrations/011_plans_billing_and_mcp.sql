-- 011_plans_billing_and_mcp.sql
-- Tiered subscription plan billing ($20/month Developer Tier, $99/month Pro Tier)
-- and AI/MCP governance telemetry support.

ALTER TABLE plans ADD COLUMN IF NOT EXISTS price_monthly_usd NUMERIC(10,2) NOT NULL DEFAULT 0.00;
ALTER TABLE plans ADD COLUMN IF NOT EXISTS tier TEXT NOT NULL DEFAULT 'free';

-- Commercial plans are created explicitly through the versioned plan API or
-- reviewed declarative configuration. Migrations must not insert commercial
-- offerings or overwrite customer prices, limits, tiers or descriptions.
