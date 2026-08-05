# Zerker

> An open gateway to manage, analyze, and productize agents and agentic workflows.

Zerker is the control plane in front of agent traffic — routing, policy,
observability, and payment metering. It is a single Go binary you can self-host:
own your gateway, own your keys, no custody handed to anyone.

It composes with the rest of the Zerker stack:
[Treeship](https://github.com/zerkerlabs/treeship) (portable trust receipts) and
[Zmem](https://github.com/zerkerlabs/zmem) (verifiable agent memory).

**Documentation:** <https://docs.zerker.ai> — guides, concepts, and
the generated API reference. The site is built from [`www/`](www/) in this repo,
so docs ship with the code they describe. The product site lives separately at
<https://zerker.ai>.

## What's here

A Go workspace monorepo (`go.work`):

| Module | What it is |
|---|---|
| `gateway/` | The gateway service — catalog, MCP-native transport, routing proxy, auth, SSRF protection, per-tenant credential isolation, x402 payment gate |
| `facilitator/` | The self-hostable x402 `/settle` server — independently re-verifies a payment and submits it on-chain with **your own** gas key |
| `x402types/` | The shared x402 wire contract |
| `sdk/go/` | Go client SDK |

Plus `www/` — the product website and developer docs (Astro + Starlight). It is
not a Go module, so it sits outside `go.work` and carries its own gate.

## Quickstart

**The gateway requires OIDC configuration to start** — `ZERKER_OIDC_ISSUER`
and `ZERKER_OIDC_AUDIENCE` must be set, or the process exits immediately. This
is a deliberate security invariant; there is no bypass.

### Local dev (mock OIDC)

`make dev-auth` boots a throwaway mock OIDC issuer and the gateway together, and
writes a ready-to-use bearer token to `/tmp/zerker-dev-token`:

```bash
make dev-auth                   # mock issuer + gateway on :8080

# operational endpoints (no token):
curl localhost:8080/healthz     # -> {"status":"ok"}
curl localhost:8080/version

# authenticated endpoints:
TOKEN=$(cat /tmp/zerker-dev-token)
curl -H "Authorization: Bearer $TOKEN" localhost:8080/v1/agents
```

The mock issuer (`gateway/scripts/mock-oidc/`) is dev-only — never use it in
production.

### Production

Point `ZERKER_OIDC_ISSUER` at your IdP (Auth0, Okta, Google, …) and set
`ZERKER_OIDC_AUDIENCE` to the audience your IdP issues:

```bash
ZERKER_OIDC_ISSUER=https://your-idp.example.com \
ZERKER_OIDC_AUDIENCE=your-audience \
  make run
```

By default the gateway uses an in-memory store (agents are lost on restart). Set
`ZERKER_DATABASE_URL` to back it with Postgres:

```bash
ZERKER_DATABASE_URL="postgres://user:pass@localhost:5432/zerker?sslmode=disable" make run
```

## Build & test

```bash
make tools              # once per checkout: install pinned gofumpt + golangci-lint
make check              # the full gate: tidy, format, vet, lint, race-tested tests
make -C gateway check   # gate a single module
make -C www check       # gate the website (not covered by the root `make check`)
make run                # run the gateway locally (:8080)
make help               # list targets
```

`make check` is the contract for "is this shippable," and CI runs the identical
gate per module. It must be green before every PR.

## Contributing

See [`AGENTS.md`](AGENTS.md) for architecture, security invariants, and
conventions. PRs are reviewed for correctness, safety, test coverage, and
conformance to their stated acceptance criteria.

## License

[Apache License 2.0](LICENSE). © 2026 Zerker Labs.
