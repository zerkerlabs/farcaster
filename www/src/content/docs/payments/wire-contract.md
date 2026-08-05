---
title: The wire contract
description: The X-PAYMENT header, the 402 challenge, and the EIP-3009 authorization payload — generated from x402types/openapi.yaml.
---

x402 layers three shapes onto ordinary HTTP: a `402` challenge, a signed
payment payload the caller replies with, and (once settlement is enabled) a
settlement receipt. Zerker Gateway's gateway, the facilitator, and both SDKs all
generate their types from the same
[`x402types/openapi.yaml`](https://github.com/zerkerlabs/gateway/blob/main/x402types/openapi.yaml)
schema — see [Architecture](/concepts/architecture/) — so this page
describes the schema, not a hand-copied approximation of it. The rendered,
always-current version of every type below lives in the
[API reference](/api-reference/x402/).

## The `402` challenge

An unpaid call to a priced route gets `402` with a body listing what would
satisfy it:

```http
HTTP/1.1 402 Payment Required
Content-Type: application/json

{
  "x402Version": 1,
  "accepts": [{
    "scheme": "exact",
    "network": "base",
    "maxAmountRequired": "10000",
    "asset": "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913",
    "payTo": "0xOperatorWallet...",
    "resource": "/v1/proxy/agt_01J9X8.../",
    "maxTimeoutSeconds": 60
  }]
}
```

Each entry in `accepts` is a `PaymentRequirements` object — `scheme` (v1:
`exact` only), `network` (v1: `base` only), `maxAmountRequired` and `asset`
(the price, as a decimal string, and the USDC contract address), `payTo` (the
operator's payout address), `resource`, and `maxTimeoutSeconds`.

## The `X-PAYMENT` header

The caller signs an EIP-3009 `transferWithAuthorization` and retries with it,
base64-encoded JSON in the `X-PAYMENT` request header:

```json
{
  "x402Version": 1,
  "scheme": "exact",
  "network": "base",
  "payload": {
    "signature": "0x...",
    "authorization": {
      "from": "0xPayer...",
      "to": "0xOperatorWallet...",
      "value": "10000",
      "validAfter": "0",
      "validBefore": "1793000000",
      "nonce": "0x..."
    }
  }
}
```

This decodes to a `Payment` — `x402Version`, `scheme`, `network`, and a
`payload` (a `PaymentPayload`: the EIP-712 `signature` over an
`Authorization`). The `Authorization` is the EIP-3009
[`transferWithAuthorization`](https://eips.ethereum.org/EIPS/eip-3009)
arguments the caller signed: `from`, `to`, `value`, `validAfter`,
`validBefore`, `nonce`. Every 256-bit amount and validity bound is a decimal
or hex **string** — these don't fit a JSON number safely, and parsing/
validation of the string is the consuming service's job, not the wire
format's.

The gateway verifies this payload against the `PaymentRequirements` it
issued — signature validity, `value` ≥ `maxAmountRequired`, `asset` and
`network` match, `to` equals the configured `payTo`, and the authorization is
within its validity window. See
[Gate vs settle](/payments/gate-vs-settle/) for what happens next.

## The settlement wire contract

Once a facilitator is configured, the gateway hands the same `Payment` and
the `PaymentRequirements` it verified to the facilitator as a
`SettleRequest`, and the facilitator answers with a `SettleResponse`:

```json
// SettleRequest
{
  "x402Version": 1,
  "paymentPayload": { "...": "the same Payment the gateway verified" },
  "paymentRequirements": { "...": "the same PaymentRequirements from the 402" }
}
```

```json
// SettleResponse
{
  "success": true,
  "transaction": "0xabc...",
  "network": "base",
  "payer": "0xPayer...",
  "facilitatorFee": "0"
}
```

A failed settle reports `{ "success": false, "errorReason": "..." }` — a
single coarse reason, never internal detail. See
[Endpoints](/facilitator/endpoints/) for how `/settle` uses it.
`facilitatorFee` is an optional extension: a fee-recovering facilitator may
report one, but the self-hosted default (and the gateway's managed facilitator in
v1) always returns `"0"` — the gateway's facilitator takes no cut of the
transfer itself; see [Custody posture](/facilitator/custody/).

## Full reference

Every field, requirement, and status code for `Payment`, `PaymentPayload`,
`Authorization`, `PaymentRequirements`, `SettleRequest`, and `SettleResponse`
is in the [x402 wire-contract reference](/api-reference/x402/), rendered
directly from `x402types/openapi.yaml` — the same schema this page describes
by hand, so the two can never silently diverge in shape.
