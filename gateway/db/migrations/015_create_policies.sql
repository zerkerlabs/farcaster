-- 015_create_policies.sql
-- Tenant-scoped policy document (spec 0009, ticket T1): the per-tenant
-- allow/warn/deny ruleset the enforcement point (T4) evaluates proxied calls
-- against. One document per tenant, mirroring migration 013's
-- settlement_config shape (tenant_id as primary key, which is itself an
-- index — satisfies "indexed on tenant_id" without a redundant secondary
-- index). Absent row for a tenant means no policy is configured: the
-- surface is strictly additive (spec 0009 "No policy configured = no
-- behavior change").
--
-- rules is the ordered rule list, stored as JSONB: [{"action": "...",
-- "match": {...}}, ...]. No structural CHECK on rule shape here — the
-- authoring API (T2) validates the document before it reaches the store;
-- this migration is data model + storage only.
CREATE TABLE IF NOT EXISTS policies (
    tenant_id      TEXT        NOT NULL PRIMARY KEY,
    version        INT         NOT NULL DEFAULT 1,
    default_action TEXT        NOT NULL,
    on_error       TEXT        NOT NULL,
    rules          JSONB       NOT NULL DEFAULT '[]',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
