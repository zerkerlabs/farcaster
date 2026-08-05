---
title: Agent Catalog
description: Register, list, and manage the agents Zerker Gateway fronts — the system of record every other surface references by ID.
---

The catalog is the foundation everything else in the gateway builds on: a
tenant-scoped system of record for the agents Zerker Gateway fronts. Routing,
observability, and the x402 gate all reference a catalog entry by its `id`.

## The agent record

Registering an agent creates a record with a server-assigned `agt_<uuidv7>`
ID (immutable), a required `name` (unique per tenant), and optional
`description`, `tags`, and free-form `metadata`. Everything else — the
upstream to proxy to, credentials, protocol, pricing, rate limits — is layered
on as optional fields, so a catalog-only entry (no `upstream_url` yet) is
valid.

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"name":"support-bot","upstream_url":"https://your-upstream.example.com"}' \
     localhost:8080/v1/agents
```

## Status is derived, not set

An agent's `status` is computed from its configuration, never chosen directly
by the caller:

- **`pending`** — no `upstream_url` set. The catalog entry exists but is not
  invocable; see [Routing & proxy](/gateway/proxy/#pending-agents-are-not-invocable).
- **`active`** — `upstream_url` is set.
- **`inactive`** — soft-deleted (`DELETE /v1/agents/{id}`). The record is kept
  for audit/history and excluded from list results by default.

Independently, an agent can be **`suspended`** — a separate boolean that
blocks invocation without touching the catalog record or its `status`, useful
for pausing a misbehaving agent without losing its configuration.

## Protocol

Every agent declares a `protocol`: `http` (default, a generic HTTP upstream)
or `mcp` (a Model Context Protocol server). Setting `protocol=mcp` requires
`mcp_transport=streamable_http` and unlocks MCP-aware observability — see
[MCP-native transport](/gateway/mcp/). `GET /v1/agents?protocol=mcp`
filters the catalog to just one protocol, useful once you're running a mix
of REST and MCP upstreams.

## Credentials are a separate resource

An agent never stores a secret directly. `credential_ref` points at a
credential registered through `POST /v1/credentials` — write-only (the secret
goes in on create, `GET` returns only metadata and a masked last-4) and
envelope-encrypted at rest. Deleting a credential still referenced by an agent
returns `409`. The gateway injects the referenced credential into upstream calls
at invocation time; it is never echoed back to the caller and never logged.

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/agents` | Register a new agent |
| `GET` | `/v1/agents` | List the caller's agents, paginated, optionally filtered by `protocol` |
| `GET` | `/v1/agents/{id}` | Fetch one agent record |
| `PATCH` | `/v1/agents/{id}` | Field-level update (tri-state: absent = unchanged, `null` = clear, value = set) |
| `DELETE` | `/v1/agents/{id}` | Soft-delete (sets `status=inactive`) |
| `POST` / `GET` / `PATCH` / `DELETE` | `/v1/credentials` | Manage upstream credentials |

Every call requires a bearer token; every record is scoped to the caller's
tenant, and a cross-tenant ID returns `404` — never `403` — so a response can
never confirm another tenant's resource exists. See
[Auth & multi-tenancy](/concepts/auth-and-multi-tenancy/). Registering a
`name` that collides with an existing agent in your tenant returns `409`.

Full request/response schemas — every field, every status code — are in the
[gateway `/v1` REST reference](/api-reference/gateway/), generated from the
gateway's `openapi.yaml`.
