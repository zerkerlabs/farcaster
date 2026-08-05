---
title: MCP-native transport
description: Register a Model Context Protocol server as a catalog agent and get MCP-aware routing and observability, not just a dumb pipe.
---

The [routing & proxy](/gateway/proxy/) layer is protocol-agnostic by
design — it forwards whatever bytes the caller sends and returns whatever
bytes come back. That already lets [Model Context
Protocol](https://modelcontextprotocol.io) (MCP) traffic pass through
Zerker Gateway as raw JSON-RPC. Declaring `protocol=mcp` on the agent turns that
dumb pipe into an MCP-aware one: the same credential injection, SSRF
protection, and rate limiting, plus method- and tool-level observability
instead of an opaque request blob.

## Registering an MCP agent

Set `protocol` to `mcp` and `mcp_transport` to `streamable_http` — the only
transport the gateway supports today:

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"name":"docs-search","upstream_url":"https://mcp.example.com","protocol":"mcp","mcp_transport":"streamable_http"}' \
     localhost:8080/v1/agents
```

`upstream_url` is reused unchanged — MCP's Streamable HTTP transport is an
ordinary HTTPS endpoint, not a new resource type. `credential_ref` is reused
unchanged too, for MCP servers that authenticate with a static bearer token or
API key. `GET /v1/agents?protocol=mcp` filters the catalog down to just your
MCP servers; see [Agent Catalog](/gateway/catalog/).

**Only `streamable_http` is supported.** `stdio` is rejected at write time
with `400` — it is a local-process transport with no URL and no TLS, a
categorically different (client-side) shape than a stateless HTTP forwarder
handles; if the gateway ever supports it, it belongs in a client SDK, not the
gateway. Legacy HTTP+SSE is reserved but not yet accepted.

## No new endpoints — reuse the proxy surface

There is no separate "MCP endpoint." A `protocol=mcp` agent uses the exact
same `POST /v1/proxy/{id}` and `POST /v1/proxy/{id}/stream` calls as any other
agent (see [Routing & proxy](/gateway/proxy/)) — the request body is a
single MCP JSON-RPC request (`initialize`, `tools/list`, `tools/call`, ...),
and the streaming endpoint is where a call that upgrades to a server-sent
event stream gets proxied.

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -H 'Mcp-Session-Id: 9d3f2b1a-...' \
     -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_docs","arguments":{"query":"SSRF"}}}' \
     localhost:8080/v1/proxy/{id}/stream
```

## The gateway is a transparent pipe for the session

The caller owns the MCP session: it runs `initialize`, manages
`Mcp-Session-Id`, and sequences calls. The gateway forwards headers and bodies
with **no server-side session state** — `Mcp-Session-Id` already passes
through both directions with zero special-casing, since it isn't in the
proxy's stripped-header block-list. The gateway does not track capability
negotiation or cache `initialize` results.

## Method and tool-level observability

Unlike a generic upstream body (opaque to the gateway), MCP's JSON-RPC envelope
is a small, stable, known shape. The gateway parses it to extract the `method`
and, for `tools/call`, `params.name`, and stores them on the invocation record
as `mcp_method` / `mcp_tool` — visible on `GET /v1/invocations` and
`GET /v1/proxy/{id}/invocations/{inv_id}`. This is the difference between "an
agent had 4,210 calls" and "3,000 of those were `tools/call` invocations of
`search_docs`." See [Observability & analytics](/gateway/observability/).

## Retry safety

The proxy retries transient upstream failures (see
[Routing & proxy](/gateway/proxy/#credentials-retries-and-failure-modes)).
For MCP, that retry is gated on the parsed method: only known-idempotent calls
(`initialize`, `tools/list`, `resources/list`, `prompts/list`) may be retried.
**`tools/call` is never retried automatically** — a tool call can have side
effects, and an automatic retry inside an established session could
double-execute it.

## Security posture

SSRF protection is unchanged: an MCP `upstream_url` is validated at write time
and dial time exactly like any other upstream URL — this surface introduces
no new SSRF exposure, precisely because `stdio` (which would mean executing an
arbitrary local process) is excluded.

## Full reference

The `protocol`, `mcp_transport`, and `mcp_protocol_version` agent fields, and
the `mcp_method` / `mcp_tool` invocation fields, are documented in the
[gateway `/v1` REST reference](/api-reference/gateway/), generated from the
gateway's `openapi.yaml`.
