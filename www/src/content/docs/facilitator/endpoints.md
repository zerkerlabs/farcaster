---
title: Endpoints
description: /settle, /supported, and /healthz — the facilitator's three HTTP endpoints, their auth, and their failure taxonomy.
---

The facilitator exposes exactly three endpoints. `/settle` is mutually
authenticated and money-moving; `/supported` and `/healthz` are open and
read-only.

## `POST /settle`

The money endpoint. Requires **mutual TLS** — the caller presents a client
certificate that the facilitator maps to a facilitator account. There is no
bearer token or API key path.

```http
POST /settle HTTP/1.1
Host: settle.zerker.dev
Content-Type: application/json

{
  "x402Version": 1,
  "paymentPayload": { "...": "the verified Payment (see The wire contract)" },
  "paymentRequirements": { "...": "the PaymentRequirements it satisfies" }
}
```

```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "success": true,
  "transaction": "0xabc...",
  "network": "base",
  "payer": "0xPayer...",
  "facilitatorFee": "0"
}
```

Behind that one route:

1. **Authenticate** — no valid client certificate ⇒ the TLS handshake is
   rejected before any request is processed; a well-formed certificate
   mapping to no active account ⇒ `403`. Neither path touches a chain.
2. **Independently re-verify** — signature recovery, `value` within
   `maxAmountRequired`, `to == payTo`, the validity window, and
   network/asset policy — never trusting that the caller already checked
   (see [Custody posture](/facilitator/custody/)).
3. **Guardrails** — the account's allowed `(network, asset)` set, a
   per-transaction maximum, and a per-account daily ceiling (its own
   override, or this deployment's conservative defaults). Over any limit is
   rejected before a nonce is claimed or any gas is spent.
4. **Dedupe by nonce** — a nonce already settled returns the *same*
   transaction hash without re-broadcasting; a nonce currently in flight is
   single-flighted rather than submitted twice.
5. **Submit and wait for first-block inclusion** — signs and broadcasts
   `transferWithAuthorization` via the configured [signer](/facilitator/signers/),
   then waits for the transfer to land in a block before returning.
6. **Record** — the outcome (transaction hash, amounts, status, attempts) is
   written to the facilitator's own settlement store — the receipt the
   gateway's settlement client turns into the invocation's settlement
   sub-record.

### Failure taxonomy: outcome vs. protocol vs. guardrail

`/settle` distinguishes three kinds of failure, on purpose, so a caller's
bounded retry can tell them apart:

| Failure | HTTP status | Example |
|---|---|---|
| **Collection outcome** — the payment itself didn't succeed | `200`, body `{"success":false,"errorReason":"payment could not be collected"}` | Re-verify rejection, insufficient balance, expired authorization, consumed nonce, on-chain revert, inclusion timeout |
| **Guardrail rejection** — this account is over a configured limit | `429` | Over the per-transaction max or the daily ceiling; a disallowed `(network, asset)` |
| **Protocol failure** — the request or the service itself couldn't proceed | `400` / `403` / `503` | Malformed body (`400`); unknown or inactive account (`403`); RPC unreachable, gas tank below the low-water mark, or an in-flight duplicate (`503`) |

A collection outcome always reports the same single coarse reason — never
which specific check failed — so nothing about the rejection leaks internal
detail. This is what the gateway's settlement client turns into
`settlement_failed` (see [Gate vs settle](/payments/gate-vs-settle/)).

## `GET /supported`

Open, no auth. Advertises the `(scheme, network, asset)` combinations this
deployment will settle, so a caller can check capability without attempting
(and failing) a settle:

```http
GET /supported HTTP/1.1
```

```http
HTTP/1.1 200 OK
Content-Type: application/json

{ "kinds": [ { "scheme": "exact", "network": "base", "asset": "0xUSDC..." } ] }
```

v1 settles exactly one `(scheme, network, asset)` kind per deployment
(`exact` / `base` / USDC), matching the gateway's payment gate.

## `GET /healthz`

Open, no auth. Reports **liveness** always; **readiness** additionally
checks RPC reachability and the gas wallet's balance against a configured
low-water mark. Either check failing fails readiness — a drained gas tank or
an unreachable RPC reports unhealthy rather than letting `/settle` silently
attempt (and fail) a submission. The same readiness check gates `/settle`
itself: below the low-water mark, `/settle` returns `503` rather than
accepting requests it can't fulfill.

## Full reference

The exact `SettleRequest`/`SettleResponse` schema is in
[The wire contract](/payments/wire-contract/), generated from
`x402types/openapi.yaml` — the same contract `/settle` speaks.
