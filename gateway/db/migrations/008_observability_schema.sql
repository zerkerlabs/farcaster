-- 008_observability_schema.sql
-- Adds observability columns to invocations (spec 0003, implementation plan #1):
--   ttft_ms     — time-to-first-byte in ms on the streaming path; NULL for
--                 transactional or when no bytes were streamed
--   error_class — coarse error taxonomy set by the proxy on failure; NULL on success
--   model       — caller-supplied model name (X-Zerker-Model header); NULL if absent
-- All nullable; no backfill (forward-only per scoping decision 7).
-- Adds two indexes to support analytics + filtered list queries (spec 0003 §Behavior).

ALTER TABLE invocations
    ADD COLUMN IF NOT EXISTS ttft_ms     BIGINT,
    ADD COLUMN IF NOT EXISTS error_class TEXT,
    ADD COLUMN IF NOT EXISTS model       TEXT;

-- Analytics queries filter by tenant then time.
CREATE INDEX IF NOT EXISTS invocations_tenant_created_idx
    ON invocations (tenant_id, created_at);

-- Per-agent filtered list queries filter by tenant + agent then time.
CREATE INDEX IF NOT EXISTS invocations_tenant_agent_created_idx
    ON invocations (tenant_id, agent_id, created_at);
