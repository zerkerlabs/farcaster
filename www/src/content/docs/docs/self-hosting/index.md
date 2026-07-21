---
title: Self-hosting & operations
description: Deploying, configuring, and upgrading a self-hosted Farcaster gateway.
---

For getting a first instance running, see [Install](/docs/install/) and the
[Quickstart](/docs/quickstart/). This section covers running Farcaster in
production:

- **[Deployment](/docs/self-hosting/deployment/)** — the single-binary model,
  containers, health probes, horizontal scaling, and graceful shutdown.
- **[Configuration reference](/docs/self-hosting/configuration/)** — every
  environment variable the gateway reads, generated directly from the gateway's
  config manifest (so a new setting can't ship undocumented).
- **[Postgres](/docs/self-hosting/postgres/)** — the durable store, automatic
  migrations, and the dev-only in-memory fallback.
- **[KMS & secrets](/docs/self-hosting/kms-and-secrets/)** — how stored
  credentials are envelope-encrypted, the `FARCASTER_KMS_KEY` master key, and
  secret hygiene.
- **[Upgrades](/docs/self-hosting/upgrades/)** — binary replacement, migrations
  on boot, and rolling deploys.
