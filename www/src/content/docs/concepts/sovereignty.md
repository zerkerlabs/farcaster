---
title: Sovereignty & no-custody
description: Why Zerker never holds a private key, and why that's the point for self-hosters.
---

Zerker's positioning is **sovereign, single-binary, no-custody**. Each word
is a specific, checkable claim, not marketing:

- **Sovereign.** The gateway is a single Go binary with no required external
  service beyond an OIDC issuer (and, optionally, Postgres). You can run it
  on-prem, in your own VPC, or fully air-gapped. Nothing about the OSS product
  requires calling out to Zerker's infrastructure.
- **Single binary.** No JVM, no Python runtime, no sidecar mesh to operate.
  One process to deploy, upgrade, and reason about.
- **No-custody.** This is the sharpest edge. When a caller pays for a gated
  tool call, Zerker verifies the payment authorization's signature and
  terms — it does **not** hold, forward, or take temporary control of the
  caller's or the operator's private key at any point. Compare this to a
  facilitator-custody model, where a third party signs or submits transactions
  on your behalf: Zerker's gateway simply never has a key to lose, leak, or
  be compelled to misuse.

## What this buys a self-hoster

If you are running in a regulated environment, or simply don't want a vendor
sitting between your traffic and your infrastructure, "no-custody" is the
property that makes self-hosting *safe* rather than merely *possible*. There
is no server-held signing key inside the gateway process for an attacker to
target, and no dependency on an external custodian's uptime or trust
assumptions for the gateway to keep functioning.

Collecting a verified payment on-chain — actually submitting the
transaction — is a distinct step handled by a **facilitator**, which you can
also self-host. See [Facilitator](/facilitator/) and
[The open-core boundary](/concepts/open-core-boundary/) for where that
capability sits.
