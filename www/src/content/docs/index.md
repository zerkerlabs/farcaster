---
title: What is Zerker
description: Zerker is a sovereign, single-binary gateway that routes, observes, and meters agent traffic — self-hosted, with no server-held key.
---

Zerker is the gateway to **manage, analyze, and productize agents and
agentic workflows**. It sits in front of your agent traffic as a single
self-hostable Go binary and turns raw calls — plain HTTP or MCP — into
something you can catalog, observe, secure, and (optionally) meter and charge
for.

Concretely, the gateway is:

- **A catalog** — register an agent or tool once (a name, an upstream URL,
  optional credentials), and callers reach it through Zerker instead of
  directly.
- **A proxy** — every call is routed to the agent's upstream, transactional or
  streaming, with retries, circuit breaking, and an SSRF guard enforced at
  request time.
- **An observability layer** — every proxied invocation is captured: request
  metadata, latency, error rate, time-to-first-token. No traffic moves through
  the gateway unrecorded.
- **A payment gate** — price a route or a specific MCP tool, and Zerker will
  hold a call behind an [x402](https://x402.gitbook.io/x402) payment
  authorization until a valid one is presented, without ever touching a
  private key itself.

## Who it's for

- **The self-hoster** — sovereign, regulated, or air-gapped operators who need
  to run their own agent infrastructure rather than route it through someone
  else's cloud. See [Sovereignty & no-custody](/concepts/sovereignty/).
- **The developer** — integrating an agent or tool behind the catalog, or
  calling one through the Go or TypeScript SDK.
- **The managed-tier buyer** — teams that want the OSS gateway's posture but
  don't want to operate settlement, billing, or a control plane themselves.
  See [OSS vs Commercial at a glance](/oss-vs-commercial/).

## Where to go next

- [Install](/install/) — run the gateway locally in one command.
- [Quickstart](/quickstart/) — register an agent, proxy a call, gate a
  paid tool.
- [Concepts](/concepts/architecture/) — how the gateway, the facilitator,
  and the SDKs fit together.
