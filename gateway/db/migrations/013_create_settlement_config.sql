-- 013_create_settlement_config.sql
-- Tenant-scoped settlement configuration (spec 0006, ticket T1): points the
-- gateway at a per-tenant x402 facilitator. Absent config (no row for a
-- tenant) means priced routes stay gate-only (surface-5 behavior); settlement
-- is opt-in (Decision 1).
--
-- facilitator_credential_ref is an opaque reference into the existing
-- envelope-encrypted upstream_credentials store (internal/credential) — never
-- a bare secret column (invariant #4, AGENTS.md). The FK mirrors migration
-- 005's agents_credential_ref_fk; DEFERRABLE INITIALLY DEFERRED so fixtures
-- can insert credentials and config in either order within a transaction.
CREATE TABLE IF NOT EXISTS settlement_config (
    tenant_id                  TEXT        NOT NULL PRIMARY KEY,
    facilitator_url            TEXT        NOT NULL,
    facilitator_credential_ref TEXT        NOT NULL,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT settlement_config_credential_ref_fk
        FOREIGN KEY (facilitator_credential_ref)
        REFERENCES upstream_credentials(id)
        DEFERRABLE INITIALLY DEFERRED
);
