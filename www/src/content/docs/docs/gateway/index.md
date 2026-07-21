---
title: Gateway
description: The gateway's feature surface — catalog, routing & proxy, MCP-native transport, observability, and the x402 gate.
---

The gateway is the product: catalog, routing & proxy (transactional and
streaming), MCP-native transport, observability & analytics, the x402 payment
gate, and per-tenant credential isolation. All of it is OSS and self-hostable
— see [OSS vs Commercial at a glance](/docs/oss-vs-commercial/).

- **[Agent Catalog](/docs/gateway/catalog/)** — register, list, and manage the
  agents Farcaster fronts.
- **[Routing & proxy](/docs/gateway/proxy/)** — transactional and streaming
  invocation, verbatim body forwarding, credential injection.
- **[MCP-native transport](/docs/gateway/mcp/)** — register an MCP server as a
  catalog agent and get method/tool-aware routing and observability.
- **[Observability & analytics](/docs/gateway/observability/)** — list and
  inspect invocations, pull aggregate latency/error metrics per agent.

Payments and the facilitator are documented separately under
[Payments (x402)](/docs/payments/) and [Facilitator](/docs/facilitator/).
Every endpoint referenced above is also in the
[gateway `/v1` REST reference](/docs/api-reference/gateway/), generated from the
gateway's `openapi.yaml`. For the architecture that ties these pieces
together, start with [Concepts → Architecture](/docs/concepts/architecture/);
for a hands-on walkthrough, see the [Quickstart](/docs/quickstart/).
