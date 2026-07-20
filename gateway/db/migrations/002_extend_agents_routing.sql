-- 002_extend_agents_routing.sql
-- Extends the agents table for the routing/proxy surface (spec 0002).
-- Adds upstream_url and credential_ref (both nullable); widens the status
-- check constraint to include 'pending' (an agent with no upstream_url is
-- pending and not yet invocable). Existing rows are not modified — backward
-- compatible per ADR-0008.

ALTER TABLE agents
    ADD COLUMN IF NOT EXISTS upstream_url    TEXT,
    ADD COLUMN IF NOT EXISTS credential_ref TEXT;

-- Widen the status domain from ('active','inactive') to include 'pending'.
-- PostgreSQL names an inline column-level CHECK as {table}_{column}_check.
ALTER TABLE agents DROP CONSTRAINT agents_status_check;
ALTER TABLE agents ADD CONSTRAINT agents_status_check
    CHECK (status IN ('active', 'inactive', 'pending'));

-- Widen name-uniqueness to cover both 'active' and 'pending' (it previously
-- only applied WHERE status='active', which let two pending agents — or a
-- pending + active pair — share a name, diverging from the in-memory store).
DROP INDEX IF EXISTS agents_tenant_name_active_idx;
CREATE UNIQUE INDEX IF NOT EXISTS agents_tenant_name_non_deleted_idx
    ON agents (tenant_id, name)
    WHERE status IN ('active', 'pending');
