---
title: Routing & proxy
description: How Farcaster forwards calls to a registered agent's upstream — transactional and streaming, verbatim body, header-carried metadata.
---

Once an agent has an `upstream_url` (see [Agent Catalog](/gateway/catalog/)),
Farcaster sits in its traffic path. A caller invokes the agent through
Farcaster instead of calling the upstream directly, and Farcaster resolves the
upstream, injects the agent's credential, forwards the call, and records what
happened.

## Two invocation modes, one addressing scheme

Farcaster offers two endpoints for the same agent, and the caller picks by use
case:

| Mode | Endpoint | Shape |
|------|----------|-------|
| **Transactional** | `POST /v1/proxy/{id}` | Accepts the call, returns `202` + `invocation_id` immediately, runs the upstream call server-side, caller polls for the result. |
| **Streaming** | `POST /v1/proxy/{id}/stream` | A long-lived connection; request and response bodies stream through verbatim, no buffering. |

Both return an invocation ID (`inv_<uuidv7>`) — in the body for transactional
calls, in the `X-Farcaster-Invocation-ID` response header for streaming — so
every call is addressable in [observability & analytics](/gateway/observability/)
regardless of which mode you used.

### Transactional

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{}' \
     localhost:8080/v1/proxy/{id}
# 202 Accepted, X-Farcaster-Invocation-ID: inv_...
# { "invocation_id": "inv_..." }
```

Poll `GET /v1/proxy/{id}/invocations/{inv_id}` for status and, once complete,
the captured response body. This suits long "thinking" flows where you'd
rather not hold a connection open.

### Streaming

```bash
curl -N -H "Authorization: Bearer $TOKEN" \
     -d '{}' \
     localhost:8080/v1/proxy/{id}/stream
```

`X-Farcaster-Invocation-ID` is set on the response before any upstream byte is
written, regardless of outcome. This suits token-by-token responses (SSE) or
hefty payloads where transactional polling would add latency.

### Pending agents are not invocable

Both endpoints return `422` for an agent with no `upstream_url` configured
(`status: pending`), and for a suspended or soft-deleted agent — before any
upstream call is made.

## Body verbatim, metadata in headers

Farcaster forwards the caller's request body **unmodified** to the upstream —
there is no Farcaster envelope to unwrap. Correlation and control metadata
rides in `X-Farcaster-*` response headers and a small set of request headers
instead:

- **`X-Request-ID`** — every proxied request carries one; Farcaster honors the
  caller's if present, otherwise injects one.
- **`X-Farcaster-Model`** — optional, caller-supplied, recorded on the
  invocation for observability (capped at 256 bytes; see
  [Observability & analytics](/gateway/observability/)).
- The caller's `Authorization` header is always stripped before forwarding —
  the upstream never sees the caller's OAuth token. Everything else forwards
  through a deliberate block-list, not a pass-all.

The transactional endpoint caps the request body at 32 MiB; the streaming
endpoint has no cap.

## Credentials, retries, and failure modes

Farcaster resolves the agent's `credential_ref` and injects it into the
upstream call at invocation time — never from the caller's request (see
[Agent Catalog](/gateway/catalog/#credentials-are-a-separate-resource)).
Transient upstream failures are retried, and a circuit breaker opens on a
consistently failing upstream. An unreachable upstream returns `502`; a
timeout returns `504`. Error bodies never carry upstream addresses or internal
detail.

## Policy enforcement

A tenant can attach a policy that governs which proxied calls are allowed. The
policy is evaluated inline on the proxy path — after the call is parsed and
**before** the [payment gate](#paid-agents), so a call that policy denies is
never told to pay. Each rule resolves to one of three actions:

- **allow** — forwarded unchanged (also the behavior when no rule matches and
  the policy's `default` is `allow`).
- **warn** — forwarded like allow, but the transactional response carries an
  `X-Farcaster-Policy-Warning` header and the decision is recorded.
- **deny** — the call is rejected with `403` and a single coarse reason; it is
  never forwarded.

The no-match action (`default`) and the failure action (`on_error`) are read
from the policy, not hardcoded — an operator chooses whether an unevaluable
call fails open or closed. A tenant with **no** policy configured sees zero
change: calls proxy exactly as they did before this surface existed.

### Known limitation: policy-store unavailability

`on_error` covers a failure of the policy **engine** (a loaded policy that the
evaluator cannot resolve). It does **not** cover a failure to *load* the policy
document itself — if the policy store is unavailable (e.g. a database timeout),
the tenant's `on_error` posture cannot be read, so it cannot be honored. In
that case the gateway currently **fails closed**: affected proxied calls return
`500` for the duration of the outage, regardless of the configured `on_error`
action. This is an accepted interim posture; wiring a configurable posture (or
a cached last-known policy) for store-load failures is tracked as a follow-up.

### Known limitation: `max_body_bytes` on chunked streaming requests

The streaming proxy path never buffers the request body, so a `max_body_bytes`
rule is evaluated against `Content-Length` rather than actual bytes read. A
chunked (or otherwise unknown-length) request has no `Content-Length`; the
gateway treats that as arbitrarily large so the rule fails toward matching
rather than being silently bypassable by omitting the header. A consequence is
that **every** chunked request to a `max_body_bytes`-guarded tool matches the
rule, regardless of how small the actual body turns out to be.

## Paid agents

An agent with `pricing` set gates invocation behind an
[x402](https://x402.gitbook.io/x402) payment: a request with no `X-PAYMENT`
header gets `402` with a payment challenge instead of being forwarded, and no
invocation record is created. See [Payments](/payments/) and
[Quickstart → Gate a paid tool](/quickstart/#3-gate-a-paid-tool).

## Full reference

Every status code, header, and schema for `/v1/proxy/{id}` and
`/v1/proxy/{id}/stream` is in the
[gateway `/v1` REST reference](/api-reference/gateway/), generated from the
gateway's `openapi.yaml`.
