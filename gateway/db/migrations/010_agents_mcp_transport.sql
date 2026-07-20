-- 010_agents_mcp_transport.sql
-- Adds MCP catalog columns to agents (spec 0004, ticket T1), mirroring
-- migration 002/007's additive pattern:
--   protocol             — "http" (default, current behavior) or "mcp"; NULL
--                          is treated as "http" by the application. No default
--                          or NOT NULL here so existing rows are untouched.
--   mcp_transport        — only meaningful when protocol="mcp"; v1 accepts
--                          only "streamable_http" (validation is enforced in
--                          application code, not a DB constraint, per spec
--                          0004 decision 2)
--   mcp_protocol_version — the MCP protocolVersion string the upstream
--                          negotiates (e.g. "2025-06-18"); informational only
-- All nullable; no backfill (forward-only, existing agents get NULLs). No
-- model/validation/API/proxy changes here — that is a follow-up ticket.

ALTER TABLE agents
    ADD COLUMN IF NOT EXISTS protocol             TEXT,
    ADD COLUMN IF NOT EXISTS mcp_transport        TEXT,
    ADD COLUMN IF NOT EXISTS mcp_protocol_version TEXT;
