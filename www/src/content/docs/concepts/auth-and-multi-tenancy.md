---
title: Auth & multi-tenancy
description: OAuth 2.0 / OIDC identity, and how every record is scoped to a tenant.
---

## Authentication: OAuth 2.0 / OIDC

Every request that touches agent data carries a bearer access token. Zerker
validates it against your configured OIDC issuer and derives two identities
from its claims:

- the **client / tenant** the call belongs to, and
- the **acting user** (the token subject) who made it.

Both are attached to the request and enforced by the storage layer — handlers
never scope queries by hand. The only unauthenticated routes are `/healthz`
and `/version`; every other endpoint requires a valid token, full stop. Static
API keys were considered and rejected: a key identifies a client but not
*which person at that client* made a given call, and per-user attribution is
a requirement, not a nicety.

## Multi-tenancy: Client → User

The identity model is **Client (tenant) → User**. Every domain record — an
agent, a credential, an invocation — carries a `tenant_id`, and every query is
scoped to the authenticated caller's tenant. Cross-tenant access returns
**404, not 403**: a caller from tenant A gets the same "not found" whether an
agent belongs to tenant B or doesn't exist at all, so a response can never be
used to confirm another tenant's resource exists.

This is enforced in the storage layer, not just in handlers, and is treated as
a security invariant: no cross-tenant data may appear in list results, reads,
or even error messages that reference resource IDs.

## One deployment, many tenants — or just you

The default deployment is **pooled multi-tenant**: one Zerker instance can
serve many independent client organizations, each fully isolated by
`tenant_id`. Nothing about self-hosting changes this model — if you run your
own instance for your own organization, you are simply the only tenant on it,
with the same row-level isolation guarantees.
