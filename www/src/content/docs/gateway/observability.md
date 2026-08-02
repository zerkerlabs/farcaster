---
title: Observability & analytics
description: List and inspect invocations, and pull aggregate latency/error metrics per agent — the read layer over the proxy's own capture.
---

Every call the [proxy](/gateway/proxy/) forwards is already recorded in
an invocation row — status, mode, timing, sizes, upstream status, and
(when enabled) the request/response bodies. This is the read/analytics layer
over that data: list and inspect individual invocations, or pull aggregate
metrics per agent and over time, without bolting on an external observability
stack.

## Listing invocations

```bash
curl -H "Authorization: Bearer $TOKEN" \
     "localhost:8080/v1/invocations?agent_id={id}&status=failed&limit=20"
```

`GET /v1/invocations` is tenant-scoped, offset-paginated, and filterable by
`agent_id`, `status` (`pending|running|succeeded|failed`), `mode`
(`transactional|streaming`), `error_class`, `model`, `settlement`, and a
`since`/`until` RFC 3339 window. Bodies are never returned on the list
endpoint.

## Fetching one invocation

```bash
curl -H "Authorization: Bearer $TOKEN" \
     localhost:8080/v1/invocations/{id}
```

`GET /v1/invocations/{id}` returns the full record, including
[MCP method/tool](/gateway/mcp/#method-and-tool-level-observability) and
payment/settlement fields when applicable. `req_body` / `resp_body` are
included **only** when body capture was on for that invocation (an
[agent-level toggle](/gateway/catalog/), off by default) **and** the
caller's token carries the `invocations:read_body` OAuth scope — metadata
reads don't need it. When present, bodies are base64-encoded and capped at
1 MiB; `body_captured` reflects whether capture was on regardless of whether
the caller can read the bodies.

## What gets captured

Beyond the obvious (status, latency, sizes, upstream status), three fields are
worth knowing about:

- **`ttft_ms`** — time to first byte on the streaming path; `null` for
  transactional invocations or when no bytes streamed.
- **`error_class`** — a coarse failure taxonomy set by the proxy on failure:
  `timeout`, `upstream_5xx`, `upstream_4xx`, `ssrf_blocked`,
  `credential_error`, `cancelled`, `internal`. `null` on success.
- **`model`** — caller-supplied only, via the `X-Farcaster-Model` request
  header (see [Routing & proxy](/gateway/proxy/#body-verbatim-metadata-in-headers)).
  Farcaster proxies arbitrary upstream agents, not a known LLM wire format, so
  it does not parse bodies to infer a model. MCP is the one exception — see
  [MCP-native transport](/gateway/mcp/).

## Aggregate analytics

```bash
curl -H "Authorization: Bearer $TOKEN" \
     "localhost:8080/v1/analytics?group_by=agent_id&bucket=day&since=2026-07-01T00:00:00Z"
```

`GET /v1/analytics` aggregates the tenant's invocations by `agent_id` and a
time bucket (`hour` or `day`), returning per-group `count`, `error_rate`,
`by_error_class`, and `latency_ms` / `ttft_ms` percentiles (p50/p95/p99). A
few things to know:

- **`since` is required** — there is no default window; an unbounded query is
  rejected with `400`.
- **The `[since, until]` range is capped at 31 days.** A wider range is `400`.
- The endpoint carries its **own, tighter rate limit** than the general `/v1`
  limiter, since percentile aggregation is heavier than a row fetch — a `429`
  here doesn't mean the rest of the API is throttled.
- An empty window returns `200` with an empty `groups` array, not `404`.
- In-flight (`pending`/`running`) invocations count toward `count` but are
  excluded from latency percentiles.

## Everything is tenant-scoped

Every query in this surface filters `tenant_id` first — the same convention
as every other surface (see [Auth & multi-tenancy](/concepts/auth-and-multi-tenancy/)).
An invocation ID belonging to another tenant returns `404`, never `403`, and
`/v1/analytics` only ever aggregates the caller's own tenant.

## Full reference

Every filter, field, and status code for `/v1/invocations`,
`/v1/invocations/{id}`, and `/v1/analytics` is in the
[gateway `/v1` REST reference](/api-reference/gateway/), generated from the
gateway's `openapi.yaml`.
