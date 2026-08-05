---
title: Install
description: Run the Zerker gateway locally, or point it at your own identity provider in production.
---

Zerker ships as a single Go binary. There is no separate database, cache,
or sidecar required to start it — Postgres is optional (see below).

## Requirement: OIDC configuration

The gateway **requires OIDC configuration to start.** `ZERKER_OIDC_ISSUER`
and `ZERKER_OIDC_AUDIENCE` must be set, or the process exits immediately
with `auth: IssuerURL is required`. This is deliberate — there is no
unauthenticated path to bring the gateway up (see
[Auth & multi-tenancy](/concepts/auth-and-multi-tenancy/)).

## Local dev (mock OIDC)

Clone the repo and run the bundled dev target, which boots a throwaway mock
OIDC issuer alongside the gateway and writes a ready-to-use bearer token to
`/tmp/zerker-dev-token`:

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
ZERKER_OIDC_ISSUER=https://your-idp.example.com \
ZERKER_OIDC_AUDIENCE=your-audience \
  make run
```

By default the gateway keeps state in memory (agents are lost on restart). Set
`ZERKER_DATABASE_URL` to back it with Postgres instead:

```bash
ZERKER_DATABASE_URL="postgres://user:pass@localhost:5432/zerker?sslmode=disable" \
  make run
```

Requires Go 1.26+ to build from source.

Next: the [Quickstart](/quickstart/) walks through registering an agent,
proxying a call, and gating a paid tool.
