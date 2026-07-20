-- 007_agents_emit_receipts.sql
-- Per-agent Treeship-receipt toggle (spec 0002 Q9). When true, a tamper-evident
-- trust receipt is emitted to Treeship after each proxied invocation completes.
-- Default false: receipt emission is opt-in and off by default, so the proxy
-- path adds no work unless an agent opts in. Backward-compatible (ADR-0008).
ALTER TABLE agents
    ADD COLUMN IF NOT EXISTS emit_receipts BOOLEAN NOT NULL DEFAULT FALSE;
