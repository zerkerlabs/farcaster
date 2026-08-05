---
title: Quickstart
description: First agent, first proxied call, first gated tool — the three steps to a working Zerker Gateway.
---

This walks through the shortest path from a running gateway to a paid,
metered call: register an agent, proxy a call through it, then price it and
gate the call behind an x402 payment.

Start the gateway first — see [Install](/install/). Everything below
assumes `make dev-auth` is running and `TOKEN` holds the bearer token it
wrote to `/tmp/zerker-dev-token`:

```bash
TOKEN=$(cat /tmp/zerker-dev-token)
```

## 1. Register your first agent

An agent is a catalog entry: a name and an `upstream_url` Zerker Gateway will proxy
calls to.

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"name":"support-bot","description":"handles refunds","upstream_url":"https://your-upstream.example.com"}' \
     localhost:8080/v1/agents
```

The response includes the agent's `id` — use it below.

## 2. Proxy a call through it

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{}' \
     localhost:8080/v1/proxy/{id}
```

The gateway forwards the call to `upstream_url`, records the invocation
(latency, status, body if capture is enabled), and returns the upstream's
response. `POST /v1/proxy/{id}/stream` does the same for streaming calls;
`GET /v1/invocations` lists what was captured.

## 3. Gate a paid tool

Set a `price` on the agent to require a stablecoin payment before the gateway
will proxy a call to it. Pricing is USDC on Base:

```bash
curl -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"price":{"amount":"10000","asset":"USDC","network":"base","pay_to":"0xYourPayoutAddress"}}' \
     localhost:8080/v1/agents/{id}
```

Now a call without a payment is held at the gate:

```bash
curl -i -H "Authorization: Bearer $TOKEN" -X POST localhost:8080/v1/proxy/{id}
# HTTP/1.1 402 Payment Required
# {"x402Version":1,"accepts":[{"scheme":"exact","network":"base","asset":"...","payTo":"0xYourPayoutAddress", ...}]}
```

A caller that presents a valid, signed x402 payment authorization in the
`X-PAYMENT` header — one satisfying the `accepts` terms in the 402 body — is
verified and forwarded normally. The gateway never holds the caller's or the
operator's private key; it only verifies the signed authorization. See
[Sovereignty & no-custody](/concepts/sovereignty/) for why that matters,
and [OSS vs Commercial at a glance](/oss-vs-commercial/) for what happens
after verification (collecting the payment on-chain is a managed-tier
capability).
