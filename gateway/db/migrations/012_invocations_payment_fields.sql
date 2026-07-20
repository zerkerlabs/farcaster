-- 012_invocations_payment_fields.sql
-- Adds x402 payment-metadata columns to invocations (spec 0005, ticket T5),
-- mirroring migration 009's additive pattern:
--   payment_network — network id of the payment authorization (e.g. "base")
--   payment_asset   — asset symbol/address paid with (e.g. "USDC")
--   payment_amount  — amount paid, in the asset's smallest unit, as TEXT
--   payment_payer   — payer's address, parsed from the verified authorization
--   payment_nonce   — the authorization's nonce (best-effort replay tracking)
-- All nullable; NULL for unpriced routes or until T5b populates them. No
-- capture logic here — that is ticket T5b.

ALTER TABLE invocations
    ADD COLUMN IF NOT EXISTS payment_network TEXT,
    ADD COLUMN IF NOT EXISTS payment_asset   TEXT,
    ADD COLUMN IF NOT EXISTS payment_amount  TEXT,
    ADD COLUMN IF NOT EXISTS payment_payer   TEXT,
    ADD COLUMN IF NOT EXISTS payment_nonce   TEXT;
