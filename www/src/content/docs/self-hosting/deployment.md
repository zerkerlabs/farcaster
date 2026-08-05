---
title: Deployment
description: Deployment topologies for the single-binary Zerker Gateway — containers, health probes, scaling, and graceful shutdown.
---

Zerker Gateway ships as a **single, statically-linked Go binary**. There is no
sidecar, no runtime, and no required external service beyond the process
itself — [Postgres](/self-hosting/postgres/) is optional but recommended
for any durable deployment. The binary reads its entire configuration from the
environment ([Configuration reference](/self-hosting/configuration/)).

## Minimum to start

The gateway **will not boot without OIDC configuration** — this is deliberate;
there is no unauthenticated path to bring it up (see
[Auth & multi-tenancy](/concepts/auth-and-multi-tenancy/)).

```bash
ZERKER_OIDC_ISSUER=https://issuer.example.com \
ZERKER_OIDC_AUDIENCE=zerker-gateway \
ZERKER_DATABASE_URL=postgres://user:pass@db:5432/zerker \
ZERKER_KMS_KEY=$(openssl rand -hex 32) \
  zerker-gateway
```

By default it listens on `:8080` (override with `ZERKER_ADDR`).

## Containers

Because the binary is static, a minimal base image is enough — there is
nothing to install alongside it:

```dockerfile
FROM gcr.io/distroless/static-debian12
COPY zerker-gateway /usr/local/bin/zerker-gateway
EXPOSE 8080
ENTRYPOINT ["zerker-gateway"]
```

Inject configuration as environment variables from your platform's secret
store; do not bake secrets into the image (see
[KMS & secrets](/self-hosting/kms-and-secrets/)).

## Health probes

Two operational routes sit outside `/v1` and require **no authentication**, so
load balancers and orchestrators can probe them directly:

| Route | Use |
| --- | --- |
| `GET /healthz` | Liveness/readiness — returns `{"status":"ok"}` once serving. |
| `GET /version` | Build metadata (version + commit) — useful to confirm a rollout. |

## Scaling

The gateway process is stateless — all durable state lives in
[Postgres](/self-hosting/postgres/) — so you can run several replicas
behind a load balancer and scale horizontally.

One caveat to size for: **per-caller rate limiting is in-process**, held in
each instance's memory rather than a shared store. Behind _N_ replicas, a
caller's effective limit is up to _N_ times the per-instance limit, since each
instance meters independently. Account for this when setting limits, or pin a
caller to an instance at the load balancer if you need a hard global bound.

## Graceful shutdown

On `SIGINT`/`SIGTERM` the gateway drains before exiting: it stops accepting new
connections, gives in-flight HTTP requests (including streaming responses) up
to 30 seconds to finish, then gives in-flight transactional proxy calls a
further 30 seconds to record a terminal status. Give the container a
termination grace period of at least **60 seconds** so orchestrated rollouts
don't cut active invocations short.
