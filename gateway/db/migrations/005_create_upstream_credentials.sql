-- 005_create_upstream_credentials.sql
-- Creates per-tenant KEK storage and the upstream_credentials table for
-- envelope-encrypted credential storage (spec 0002 §Q2). Also wires the FK
-- constraint on agents.credential_ref, which was added as an untyped TEXT
-- placeholder by migration 002.

-- Per-tenant Key Encryption Keys wrapped by the KMS provider. Multiple rows
-- per tenant exist after a KEK rotation; the row with the latest created_at
-- is "current". version is the opaque token returned by KMSProvider.WrapKey
-- and is passed back to UnwrapKey so the provider can select the correct KMS
-- key version when decrypting.
CREATE TABLE IF NOT EXISTS tenant_keks (
    tenant_id   TEXT        NOT NULL,
    version     TEXT        NOT NULL,
    wrapped_kek BYTEA       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, version)
);

-- Upstream credentials with envelope encryption support.
-- For source='managed': encrypted_dek is the per-secret DEK wrapped by the
-- per-tenant KEK identified by kek_version; encrypted_secret is the AES-256-GCM
-- ciphertext of the actual secret. Plaintext never lands in any column
-- (invariant #4, AGENTS.md).
-- For source='vault': vault_ref is the path in the external secrets store;
-- the encryption columns are NULL.
CREATE TABLE IF NOT EXISTS upstream_credentials (
    id               TEXT        NOT NULL PRIMARY KEY,
    tenant_id        TEXT        NOT NULL,
    name             TEXT        NOT NULL,
    auth_type        TEXT        NOT NULL
                                 CHECK (auth_type IN ('bearer', 'api_key', 'none')),
    source           TEXT        NOT NULL
                                 CHECK (source IN ('managed', 'vault')),
    -- managed-source columns (NULL when source='vault'):
    encrypted_dek    BYTEA,
    kek_version      TEXT,
    encrypted_secret BYTEA,
    masked_hint      TEXT,
    -- vault-source column (NULL when source='managed'):
    vault_ref        TEXT,
    -- credential version for optimistic rotation tracking:
    version          INT         NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Keep source and its columns consistent at the DB level so a stray SQL
    -- write or missed code path can't create a row that only fails opaquely at
    -- decrypt time. The application enforces this too; this is the backstop.
    CONSTRAINT source_columns_consistent CHECK (
        (source = 'managed'
            AND encrypted_dek IS NOT NULL
            AND kek_version IS NOT NULL
            AND encrypted_secret IS NOT NULL
            AND vault_ref IS NULL)
        OR (source = 'vault'
            AND vault_ref IS NOT NULL
            AND encrypted_dek IS NULL
            AND encrypted_secret IS NULL)
    )
);

-- Name uniqueness per tenant.
CREATE UNIQUE INDEX IF NOT EXISTS upstream_credentials_tenant_name_idx
    ON upstream_credentials (tenant_id, name);

-- Per-tenant listing.
CREATE INDEX IF NOT EXISTS upstream_credentials_tenant_id_idx
    ON upstream_credentials (tenant_id);

-- Efficient re-wrap queries during KEK rotation: find managed credentials
-- for a tenant that still use a specific KEK version.
CREATE INDEX IF NOT EXISTS upstream_credentials_tenant_kek_idx
    ON upstream_credentials (tenant_id, kek_version)
    WHERE source = 'managed';

-- Wire the FK: agents.credential_ref must now reference a real credential row.
-- NULL remains allowed (agent with no credential attached).
-- DEFERRABLE INITIALLY DEFERRED allows fixtures to insert agents and
-- credentials in any order within a transaction.
ALTER TABLE agents
    ADD CONSTRAINT agents_credential_ref_fk
    FOREIGN KEY (credential_ref)
    REFERENCES upstream_credentials(id)
    DEFERRABLE INITIALLY DEFERRED;
