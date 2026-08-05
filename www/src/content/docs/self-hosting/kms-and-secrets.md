---
title: KMS & secrets
description: How the gateway envelope-encrypts stored credentials, the ZERKER_KMS_KEY master key, and secret-handling hygiene.
---

The gateway stores upstream credentials (API keys and the like) so it can
attach them to proxied calls. Those secrets are **envelope-encrypted at rest**:
a per-credential data key encrypts the secret, and a master key — the KMS
key — wraps the data keys. The database never holds plaintext credential
material.

## The master key: `ZERKER_KMS_KEY`

The built-in KMS provider takes its master key from `ZERKER_KMS_KEY`, a
**hex-encoded 32-byte key (64 hex characters)**:

```bash
ZERKER_KMS_KEY=$(openssl rand -hex 32)
```

If `ZERKER_KMS_KEY` is **not** set, the gateway generates a **random
ephemeral key** at startup. That key does not survive a restart — so any
credential encrypted under it becomes undecryptable once the process
restarts. The ephemeral mode is safe only for local development and in-process
tests. **Never run production without setting `ZERKER_KMS_KEY`.**

The key must stay **stable across restarts and upgrades**: it's the root of
trust for every stored credential. Losing it means losing access to all
credentials in the database. Treat it — and back it up — with the same care as
the database itself.

:::note
v1 ships the local, env-key KMS provider described here. The gateway's KMS
provider is an interface, so managed-KMS backends (e.g. cloud KMS) can be added
without changing how credentials are stored — but the env-key provider is the
only one in the box today.
:::

## Key rotation

**Rotating `ZERKER_KMS_KEY` itself is not supported today.** The built-in
provider reads a single master key from that one environment variable and
ignores any version token on decrypt — there's no way to run with an old and a
new master key at once. If you change `ZERKER_KMS_KEY` and restart, every
credential wrapped under the previous key becomes permanently undecryptable.
Treat the master key as fixed for the lifetime of the database, per the
warning above.

What *is* supported is **tenant KEK rotation**, via
`credential.Service.RotateKEK`: it generates a new per-tenant KEK, wraps it
under the currently-configured master key, and re-wraps that tenant's
credential data keys under the new KEK — all without touching the encrypted
secrets themselves or requiring the old master key. This rotates the
per-tenant key material on your normal schedule; it does not rotate
`ZERKER_KMS_KEY`.

## Secret-handling hygiene

- Deliver `ZERKER_KMS_KEY`, `ZERKER_DATABASE_URL`, and any provider
  credentials through your platform's **secret store or injected environment** —
  never a committed file. The repo's `.env*` is gitignored and read-denied by
  policy.
- Don't bake secrets into container images or CI logs.
- Rotate database credentials on your normal schedule. The master key itself
  has no rotation path today (see key rotation above); rotate tenant KEKs via
  `RotateKEK` instead.
