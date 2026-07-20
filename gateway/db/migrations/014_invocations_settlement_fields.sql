-- 014_invocations_settlement_fields.sql
-- Adds the settlement sub-record columns to invocations (spec 0006, ticket
-- T3), mirroring migration 012's payment-metadata additive pattern:
--   settlement_status     — pending | settled | settlement_failed |
--                           settled_upstream_failed (spec 0006 "the settlement
--                           sub-record"). pending is included now so the later
--                           async orchestration flip (Decision 2) needs no
--                           schema change.
--   settlement_tx_hash    — on-chain tx reference from the facilitator's
--                           settle response
--   settled_amount        — what the payer paid, smallest unit, as TEXT
--   operator_amount       — what reached pay_to (settled_amount minus
--                           facilitator_fee)
--   facilitator_fee       — facilitator-reported fee split; "0"/absent in the
--                           OSS/self-host path (Decision 6)
--   settlement_attempts   — bounded-retry attempt count
--   settlement_reason     — coarse failure reason, on failure only
--   settled_at            — when the facilitator settle succeeded
-- All nullable; NULL for unpriced/gate-only routes or until T4 populates them.
-- No settlement logic here — that is ticket T4 (and T6 for
-- settled_upstream_failed).

ALTER TABLE invocations
    ADD COLUMN IF NOT EXISTS settlement_status   TEXT
        CHECK (settlement_status IN ('pending', 'settled', 'settlement_failed', 'settled_upstream_failed')),
    ADD COLUMN IF NOT EXISTS settlement_tx_hash  TEXT,
    ADD COLUMN IF NOT EXISTS settled_amount      TEXT,
    ADD COLUMN IF NOT EXISTS operator_amount     TEXT,
    ADD COLUMN IF NOT EXISTS facilitator_fee     TEXT,
    ADD COLUMN IF NOT EXISTS settlement_attempts INT,
    ADD COLUMN IF NOT EXISTS settlement_reason   TEXT,
    ADD COLUMN IF NOT EXISTS settled_at          TIMESTAMPTZ;
