-- 001_create_agents_table.sql
-- Creates the agents table with tenant scoping, a covering index on tenant_id,
-- and a partial unique index that enforces name uniqueness within a tenant only
-- among active agents (soft-deleted agents do not block name reuse).

CREATE TABLE IF NOT EXISTS agents (
    id          TEXT        NOT NULL PRIMARY KEY,
    tenant_id   TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    tags        TEXT[]      NOT NULL DEFAULT '{}',
    metadata    JSONB       NOT NULL DEFAULT '{}',
    status      TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- All per-tenant queries filter by tenant_id first; this index covers them.
CREATE INDEX IF NOT EXISTS agents_tenant_id_idx ON agents (tenant_id);

-- Name uniqueness is enforced per tenant but only among active agents, so that
-- soft-deleted names can be reused (spec 0001; ADR-0006).
CREATE UNIQUE INDEX IF NOT EXISTS agents_tenant_name_active_idx
    ON agents (tenant_id, name)
    WHERE status = 'active';
