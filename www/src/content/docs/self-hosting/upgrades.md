---
title: Upgrades
description: Upgrading a self-hosted gateway — binary replacement, automatic migrations, and rolling deploys.
---

Upgrading is replacing the binary (or container image) and restarting. The
gateway carries its own schema migrations and applies them on boot, so there's
no separate migration step to sequence into your pipeline.

## Confirming the running version

`GET /version` returns the build's version and commit — no authentication
required — so you can confirm a rollout landed:

```bash
curl -s localhost:8080/version
```

The binary stamps these at build time; a deployed image reports the exact
commit it was built from.

## Migrations on upgrade

Because [migrations run automatically at startup](/self-hosting/postgres/),
the **first upgraded instance to boot applies any new schema changes** to the
shared database. For a high-availability deploy running several replicas, that
has one implication worth planning for:

- Roll instances **one at a time** (a standard rolling deploy) rather than
  replacing them all at once.
- During the window where upgraded and not-yet-upgraded instances both run
  against the migrated schema, keep upgrades to **adjacent versions** so the
  older instances still work against the new schema until they're replaced.

Take a database backup before a version jump, per your normal change-management
practice.

## The KMS key must not change

An upgrade must keep the **same [`ZERKER_KMS_KEY`](/self-hosting/kms-and-secrets/)**
as the running deployment. It's the root key for every stored credential;
starting an upgraded instance with a different (or absent) key means it can't
decrypt existing credentials. Carry the key forward unchanged unless you are
deliberately rotating it.
