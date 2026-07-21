---
title: Postgres
description: Configuring the gateway's PostgreSQL store — connection, automatic migrations, and the in-memory dev fallback.
---

Farcaster persists all durable state — registered agents, encrypted
credentials, invocation records, and settlement configuration — in
**PostgreSQL**. Point the gateway at a database with `FARCASTER_DATABASE_URL`
(a standard pgx/libpq DSN); `DATABASE_URL` is honored as a fallback for
platforms that inject that name.

```bash
FARCASTER_DATABASE_URL=postgres://user:pass@db:5432/farcaster?sslmode=require
```

The connection is pooled; no separate pool tuning is required to start.

## Migrations run automatically

The gateway **applies its schema migrations on boot** — there is no separate
migrate step to run in your deploy pipeline. A fresh database is brought up to
the current schema the first time the gateway connects to it; an existing one
is advanced as needed. See [Upgrades](/docs/self-hosting/upgrades/) for what
this means during a rolling deploy.

## The in-memory fallback (dev only)

If neither `FARCASTER_DATABASE_URL` nor `DATABASE_URL` is set, the gateway
falls back to a **non-durable in-memory store** and logs a warning at startup.
Every registered agent and credential is lost when the process restarts. This
is intended purely for local development and tests — **never run production
without a database.**

## Recommendations

- Use a managed Postgres (or an HA cluster) so the gateway's durability
  matches your uptime target — the gateway itself is stateless and disposable,
  but the database is not.
- Require TLS to the database (`sslmode=require` or stricter).
- Back up the database on your normal cadence: it holds the (envelope-encrypted)
  credential material — restoring it also requires the matching
  [KMS key](/docs/self-hosting/kms-and-secrets/).
