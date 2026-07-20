-- Settlement rows for the hosted facilitator (spec 0007 T4, Decision 9).
-- One row per settlement attempt, owned by one facilitator account. The
-- (account_id, nonce) uniqueness is the nonce-idempotency guarantee: it lets an
-- INSERT ... ON CONFLICT DO NOTHING claim be the atomic single-flight primitive
-- so a retried or concurrent duplicate authorization is never broadcast twice.
CREATE TABLE settlements (
    id           TEXT        PRIMARY KEY,
    account_id   TEXT        NOT NULL,

    -- EIP-3009 authorization nonce (hex); with account_id it is the idempotency
    -- key. The payer-chosen, single-use value identifying the authorization.
    nonce        TEXT        NOT NULL,

    -- Authorization from/to, kept for audit/support (not part of the key).
    payer        TEXT        NOT NULL,
    pay_to       TEXT        NOT NULL,

    network      TEXT        NOT NULL,
    asset        TEXT        NOT NULL,
    -- 256-bit amount as a decimal string — the x402 wire encodes amounts as
    -- strings, never JSON numbers; that semantic is preserved in storage.
    value        TEXT        NOT NULL,

    -- On-chain tx hash; empty until the settlement reaches 'settled'.
    tx_hash      TEXT        NOT NULL DEFAULT '',

    -- 'in_flight' | 'settled' | 'failed'.
    status       TEXT        NOT NULL,
    -- Coarse failure reason, set only when status = 'failed'. Never internal
    -- detail or key material.
    error_reason TEXT        NOT NULL DEFAULT '',

    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The idempotency key: one settlement per (account, authorization nonce).
    UNIQUE (account_id, nonce)
);

-- Per-account reads (List, and the future billing/dashboard) are always scoped
-- by account_id, ordered newest-first.
CREATE INDEX settlements_account_created_idx
    ON settlements (account_id, created_at DESC);
