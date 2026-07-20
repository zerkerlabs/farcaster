-- 006_agent_invocation_policy.sql
-- Adds invocation-policy fields to agents (spec 0002, issue #34):
--   suspended             — blocks invocation without soft-deleting the catalog entry
--   invocation_rate_limit — per-agent rate-limit override (req/s); NULL = use global
--   invocation_burst      — per-agent burst-size override; NULL = use global

ALTER TABLE agents
    ADD COLUMN IF NOT EXISTS suspended             BOOLEAN          NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS invocation_rate_limit DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS invocation_burst      INTEGER;
