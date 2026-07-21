---
title: API reference
description: The x402 wire contract and the gateway's /v1 REST surface, rendered from their canonical OpenAPI schemas.
---

Two auto-generated references land here:

- **[The x402 wire contract](/docs/api-reference/x402/)** — rendered from
  [`x402types/openapi.yaml`](https://github.com/zerkerlabs/farcaster/blob/main/x402types/openapi.yaml),
  the same schema that generates the Go types and the SDKs. Every type is
  referenced from that file, never copied, so a field change there re-renders
  here with no manual edit. Zero hand-maintenance.
- **[The gateway `/v1` REST surface](/docs/api-reference/gateway/)** (agents,
  proxy, invocations, analytics, credentials, settlement config) — rendered
  from
  [`gateway/openapi.yaml`](https://github.com/zerkerlabs/farcaster/blob/main/gateway/openapi.yaml),
  the canonical contract hand-authored from the gateway's HTTP handlers.
  Adding or altering an endpoint there re-renders here with no manual edit.

Both are wired directly to their canonical schemas — no hand-copied endpoints
or types. A CI drift-guard fails the build if either schema changes without its
rendered reference regenerating cleanly against it — see the `check-www`
context in `.github/workflows/ci.yml`.
