-- 003_create_invocations.sql
-- Creates the invocations table for the routing/proxy surface (spec 0002).
-- Records every proxied call with lifecycle state, timing, sizes, and optional
-- body capture. Always tenant-scoped. Body columns are NULL when capture is off
-- (the default — spec 0002 Q10; per-agent toggle in migration 004).

CREATE TABLE IF NOT EXISTS invocations (
    id               TEXT        NOT NULL PRIMARY KEY,
    tenant_id        TEXT        NOT NULL,
    agent_id         TEXT        NOT NULL REFERENCES agents(id),
    mode             TEXT        NOT NULL CHECK (mode IN ('transactional', 'streaming')),
    status           TEXT        NOT NULL DEFAULT 'pending'
                                 CHECK (status IN ('pending', 'running', 'succeeded', 'failed')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at     TIMESTAMPTZ,
    upstream_status  INT,
    latency_ms       BIGINT,
    req_size         BIGINT,
    resp_size        BIGINT,
    req_body         BYTEA,
    resp_body        BYTEA
);

-- All per-tenant queries filter by tenant_id first.
CREATE INDEX IF NOT EXISTS invocations_tenant_id_idx ON invocations (tenant_id);
-- Per-agent listing queries join on agent_id within the tenant.
CREATE INDEX IF NOT EXISTS invocations_agent_tenant_idx ON invocations (agent_id, tenant_id);
