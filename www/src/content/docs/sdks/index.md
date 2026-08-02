---
title: SDKs
description: Go and TypeScript clients, generated from the shared x402 wire contract.
---

Farcaster ships Go and TypeScript SDKs (`sdk/go/`, `sdk/ts/`), generated from
the same `x402types` OpenAPI contract the gateway and facilitator speak — so a
client can never drift from the wire format the servers actually implement.

Both SDKs are early: they re-export the generated wire types today, with
client methods landing as the SDKs mature. A generated reference (from Go doc
comments and TS types) is a fast-follow once there's a client surface to
document.
