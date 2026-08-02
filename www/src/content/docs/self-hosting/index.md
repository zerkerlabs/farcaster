---
title: Self-hosting & operations
description: Deploying, configuring, and upgrading a self-hosted Farcaster gateway.
---

For getting a first instance running, see [Install](/install/) and the
[Quickstart](/quickstart/). This section covers running Farcaster in
production:

- **[Deployment](/self-hosting/deployment/)** — the single-binary model,
  containers, health probes, horizontal scaling, and graceful shutdown.
- **[Configuration reference](/self-hosting/configuration/)** — every
  environment variable the gateway reads, generated directly from the gateway's
  config manifest (so a new setting can't ship undocumented).
- **[Postgres](/self-hosting/postgres/)** — the durable store, automatic
  migrations, and the dev-only in-memory fallback.
- **[KMS & secrets](/self-hosting/kms-and-secrets/)** — how stored
  credentials are envelope-encrypted, the `FARCASTER_KMS_KEY` master key, and
  secret hygiene.
- **[Upgrades](/self-hosting/upgrades/)** — binary replacement, migrations
  on boot, and rolling deploys.
