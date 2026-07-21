---
title: Gate vs settle
description: Two distinct steps behind one x402 flow — verifying a payment authorization, and actually collecting it.
---

x402 on Farcaster is two steps, shipped as two surfaces, with an open-core
line drawn exactly between them. Every priced route goes through the
**gate**; only a route on a tenant with a facilitator configured also goes
through **settlement**.

## The gate: verify, don't move money

The gate is **local, synchronous, and self-contained** — it runs inside the
gateway process on every call to a priced route, with no outbound call and no
signing key:

1. A request to a priced route with no `X-PAYMENT` header gets `402` with a
   challenge (see [The wire contract](/docs/payments/wire-contract/)).
2. The caller retries with a signed authorization in `X-PAYMENT`.
3. The gateway checks the EIP-712 signature, that `value` meets the price,
   that `asset`/`network`/`to` match what it advertised, and that the
   authorization is within its validity window.
4. On success, the call is forwarded exactly as an unpriced call would be. On
   failure, `402` again, with a coarse reason.

Nothing about this step touches a chain or a private key — it is pure
signature and parameter verification. It is why the gate is OSS: it's the
primitive that makes "self-host *and* monetize" credible on its own, with
zero dependency on Zerker's infrastructure. See
[Sovereignty & no-custody](/docs/concepts/sovereignty/).

A route with **no facilitator configured for its tenant** stops here. The
authorization is *verified*, not *collected* — the operator has cryptographic
proof the caller could pay, but no funds have moved. Because there is no
settlement step to authoritatively consume the on-chain nonce, the gate's
replay protection is **best-effort only**: a valid signed payload could in
principle be replayed against a gate-only route. This is a documented
boundary of gate-only mode, not a bug — enabling settlement is what closes it
(next section).

## Settlement: verify, then collect

Configuring a **facilitator** for a tenant (`PATCH /v1/settlement/config`
— see [Facilitator](/docs/facilitator/)) turns the same priced route into a
settlement-enabled one, and the lifecycle gains a step in between verify and
forward:

**verify (local, gate) → settle (via the facilitator) → forward**

Because the proxy is already asynchronous (`202` + poll, or a streaming
connection), settlement runs inside that lifecycle, *before* the upstream is
ever called:

- The gateway POSTs the verified authorization to the configured
  facilitator's `/settle`.
- **Only a successful settle results in the upstream being called.** A
  settle failure ends the invocation `settlement_failed` — the upstream never
  runs, so there is no "served but not paid" state to reconcile.
- On success, the facilitator has submitted the transfer on-chain and
  consumed the authorization's nonce — replay protection is now
  **authoritative**, not best-effort.
- The result is recorded on the invocation as a settlement sub-record
  (status, transaction hash, amounts, attempts) — the canonical receipt,
  exposed on `GET /v1/invocations/{id}` and filterable via
  `?settlement=failed`.

The gateway never holds a signing key at any point in this flow — it
delegates the one step that requires one (submitting the transaction) to the
facilitator. See [Custody posture](/docs/facilitator/custody/) for exactly
who holds what.

## What changes, at a glance

| | Gate only (no facilitator configured) | Gate + settlement (facilitator configured) |
|---|---|---|
| Verification | Local, synchronous | Local, synchronous (unchanged) |
| Money movement | None | Facilitator submits on-chain |
| Nonce replay protection | Best-effort | Authoritative (on-chain) |
| Settlement receipt | None | Settlement sub-record on the invocation |
| Facilitator required | No | Yes — self-hosted (OSS) or managed (Commercial) |
| Failure behavior | `402`, no invocation created | Settle failure → invocation ends `settlement_failed`, upstream never called |

Enabling settlement is **opt-in per tenant**: no facilitator configured means
every priced route behaves exactly as gate-only, unchanged. There is no mode
where a route is "half" settled — a request either never reaches a
facilitator, or it goes through the full verify → settle → forward sequence.

## Why the line sits here

Verification requires no outbound call and no key, so it stays in the free,
self-hostable core — it's the wedge that makes Farcaster's monetization story
credible without asking anyone to trust Zerker with anything. Collection
requires a party willing to hold a gas key and submit transactions, which is
a materially different trust and operational commitment — that's the
facilitator, and pointing at Zerker's managed one (rather than running your
own) is the commercial product. See
[The open-core boundary](/docs/concepts/open-core-boundary/) for the full
reasoning.
