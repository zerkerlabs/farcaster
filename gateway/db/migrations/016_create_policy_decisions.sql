-- 016_create_policy_decisions.sql
-- Tenant-scoped policy decision capture (spec 0009, ticket T5): one row per
-- evaluated proxied call, recording what the enforcement point (T4) decided —
-- the "OSS captures" side of the ratified line (Decision 1) and the raw
-- material a commercial governance dashboard later aggregates. Written async,
-- fail-open, off the request path; read back by GET /v1/policy/decisions.
--
-- Many rows per tenant (unlike policies, which is one document per tenant), so
-- this mirrors the invocations shape: a synthetic pdec_<uuidv7> primary key,
-- tenant_id + a (tenant_id, created_at DESC) index for the recent-first read,
-- and the decision columns straight off policy.RecordedDecision + Decision.
--
-- Request bodies are never stored here (invariant #3): reason is the coarse,
-- rule-position explanation the evaluator produced, never call content.
CREATE TABLE IF NOT EXISTS policy_decisions (
    id           TEXT        NOT NULL PRIMARY KEY,
    tenant_id    TEXT        NOT NULL,
    agent_id     TEXT        NOT NULL,
    protocol     TEXT        NOT NULL,
    mcp_tool     TEXT,
    action       TEXT        NOT NULL,
    matched_rule TEXT        NOT NULL DEFAULT '',
    reason       TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS policy_decisions_tenant_created_idx
    ON policy_decisions (tenant_id, created_at DESC);
