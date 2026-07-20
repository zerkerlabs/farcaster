-- 011_agents_pricing.sql
-- Adds agent pricing columns (spec 0005, ticket T1, Decision 5), mirroring
-- migration 010's additive pattern:
--   price_amount  — integer amount in the asset's smallest unit (e.g. USDC
--                    6-decimals), as TEXT; NULL = unpriced (route/agent not
--                    gated).
--   price_asset   — asset symbol (e.g. "USDC").
--   price_network — network id (e.g. "base").
--   price_pay_to  — operator payout address; no server-held key, this is a
--                    public address only (Decision 9).
--   price_tools   — optional per-mcp_tool overrides: {tool_name: amount}.
--                    NULL when none; only meaningful for protocol="mcp"
--                    agents (Decision 5).
-- All nullable; no backfill (forward-only, existing agents get NULLs). No
-- model/validation/API/proxy changes here — those are follow-up tickets.

ALTER TABLE agents
    ADD COLUMN IF NOT EXISTS price_amount  TEXT,
    ADD COLUMN IF NOT EXISTS price_asset   TEXT,
    ADD COLUMN IF NOT EXISTS price_network TEXT,
    ADD COLUMN IF NOT EXISTS price_pay_to  TEXT,
    ADD COLUMN IF NOT EXISTS price_tools   JSONB;
