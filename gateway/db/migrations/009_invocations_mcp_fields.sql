-- 009_invocations_mcp_fields.sql
-- Adds MCP observability columns to invocations (spec 0004, ticket T2), mirroring
-- migration 008's additive pattern:
--   mcp_method — the parsed JSON-RPC method (e.g. "tools/call"); NULL for
--                non-MCP agents or until T4 populates it
--   mcp_tool   — for mcp_method="tools/call", the parsed params.name; NULL
--                otherwise
-- Both nullable; no backfill (forward-only). No population logic here — that
-- is ticket T4.

ALTER TABLE invocations
    ADD COLUMN IF NOT EXISTS mcp_method TEXT,
    ADD COLUMN IF NOT EXISTS mcp_tool   TEXT;
