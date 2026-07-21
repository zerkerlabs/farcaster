---
title: Install
description: Run the Farcaster gateway locally, or point it at your own identity provider in production.
---

Farcaster ships as a single Go binary. There is no separate database, cache,
or sidecar required to start it — Postgres is optional (see below).

## Requirement: OIDC configuration

The gateway **requires OIDC configuration to start.** `FARCASTER_OIDC_ISSUER`
and `FARCASTER_OIDC_AUDIENCE` must be set, or the process exits immediately
with `auth: IssuerURL is required`. This is deliberate — there is no
unauthenticated path to bring the gateway up (see
[Auth & multi-tenancy](/docs/concepts/auth-and-multi-tenancy/)).

## Local dev (mock OIDC)

Clone the repo and run the bundled dev target, which boots a throwaway mock
OIDC issuer alongside the gateway and writes a ready-to-use bearer token to
`/tmp/farcaster-dev-token`:

```bash
git clone https://github.com/zerkerlabs/farcaster.git
cd farcaster
make dev-auth
```

```bash
# operational endpoints — no token required
curl localhost:8080/healthz     # -> {"status":"ok"}
curl localhost:8080/version
```

The mock issuer is dev-only and must never be used in production.

## Production

Point the gateway at your real identity provider (Auth0, Okta, Google, or any
OIDC-compliant issuer):

```bash
FARCASTER_OIDC_ISSUER=https://your-idp.example.com \
FARCASTER_OIDC_AUDIENCE=your-audience \
  make run
```

By default the gateway keeps state in memory (agents are lost on restart). Set
`FARCASTER_DATABASE_URL` to back it with Postgres instead:

```bash
FARCASTER_DATABASE_URL="postgres://user:pass@localhost:5432/farcaster?sslmode=disable" \
  make run
```

Requires Go 1.26+ to build from source.

Next: the [Quickstart](/docs/quickstart/) walks through registering an agent,
proxying a call, and gating a paid tool.
