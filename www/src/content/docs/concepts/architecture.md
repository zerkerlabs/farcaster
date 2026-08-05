---
title: Architecture
description: Gateway, facilitator, the shared x402 wire contract, and the SDKs — how Zerker Gateway's pieces fit together.
---

Zerker Gateway is a **Go workspace monorepo**: several independently-deployable
modules that share one wire contract and one build harness, rather than a
single monolith or a scattered polyrepo.

## The pieces

- **The gateway** (`gateway/`) — the product. A single Go binary: the catalog,
  the reverse-proxy/routing layer (plain HTTP and MCP), observability
  (invocation capture + analytics), auth, and the x402 payment **gate**
  (verification only). This is what you self-host.
- **The facilitator** (`facilitator/`) — the x402 `/settle` server: the piece
  that actually submits a verified payment authorization on-chain and returns
  a transaction hash. It is a **separate deployable** from the gateway on
  purpose — it is the only component that needs a gas key and a chain client
  (go-ethereum), so those heavy, security-sensitive dependencies never enter
  The gateway's module graph. You can self-host a facilitator (OSS), or use
  the managed one.
- **`x402types`** — the shared x402 wire contract, generated from an OpenAPI
  schema. The gateway, the facilitator, the SDKs, and this documentation site
  all consume the *same* generated types, so a payment payload, a settle
  request, or a payment requirement can never silently drift between services.
- **The SDKs** (`sdk/go/`, `sdk/ts/`) — Go and TypeScript clients generated
  from the same contract.

## Why a workspace, not one binary

The gateway's dependency surface is part of its security surface — it is the
thing sovereign/regulated operators audit before they'll run it. Splitting the
facilitator out keeps the gateway's `go.mod` free of chain/crypto-heavy
dependencies it doesn't need to do its job (routing, observing, and
*verifying*, not custodying or submitting transactions). One repo, one clone,
one `make check` fans out over every module — but the module boundary is real,
not cosmetic.

## Deployment shape

The gateway is a **pooled multi-tenant control plane by default** — one
deployment can serve multiple independent client organizations, with every
record scoped to a `tenant_id` (see
[Auth & multi-tenancy](/concepts/auth-and-multi-tenancy/)). A fully
self-hosted, in-VPC deployment for a single tenant is the same binary, same
data model — you're just the only tenant on it.
