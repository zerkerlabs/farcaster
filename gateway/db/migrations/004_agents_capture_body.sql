-- 004_agents_capture_body.sql
-- Adds the per-agent body-capture toggle to the agents table (spec 0002 Q10).
-- Default FALSE so existing agents do not capture request/response bodies
-- without an explicit opt-in.

ALTER TABLE agents
    ADD COLUMN IF NOT EXISTS capture_body BOOLEAN NOT NULL DEFAULT FALSE;
