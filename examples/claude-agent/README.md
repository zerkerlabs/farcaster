# claude-agent — turn Claude Code into a gateway agent

A ~150-line reference upstream that wraps a local Claude Code install in a
persistent HTTP service, so the Zerker gateway can catalog, proxy, meter,
and price it like any other agent. This is the smallest honest version of
"upgrade my coding agent into a permanent agent."

The division of labor is the whole point:

| Concern | Owner |
|---|---|
| Auth, tenancy, rate limits, policy | gateway |
| Payment gating (x402), invocation capture, analytics | gateway |
| The actual model call and its specialty | this process |

## Run it

```
cd examples/claude-agent
AGENT_SPECIALTY="You are the payments-team expert for our x402 integration. \
Answer only within that scope." GOWORK=off go run .

curl -s localhost:9200/invoke -d '{"prompt":"What does a 402 response mean?"}'
```

Requires a Claude Code install (`claude` on PATH, or set `CLAUDE_BIN`). Each
request shells out to `claude -p --output-format json` and passes through the
result plus `duration_ms` and `total_cost_usd` — so you can see what a call
costs you before deciding what it costs your callers.

## Register it on the gateway

The gateway's SSRF guard (correctly) refuses loopback and private upstreams,
so give the wrapper a public URL the gateway can reach — deploy it, or tunnel
it (`cloudflared tunnel --url http://localhost:9200`). Then:

```
TOKEN=$(cat /tmp/zerker-dev-token)
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"payments-expert","description":"Claude, specialized on our x402 docs",
       "upstream_url":"https://<public-host>/invoke"}' \
  localhost:8080/v1/agents
```

Proxy a call through the gateway, then put a price on it and watch unpaid
calls get held at the 402 gate:

```
curl -H "Authorization: Bearer $TOKEN" -d '{"prompt":"..."}' \
  localhost:8080/v1/proxy/<agt_id>

curl -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"pricing":{"amount":"10000","asset":"USDC","network":"base","pay_to":"0x…"}}' \
  localhost:8080/v1/agents/<agt_id>
```

## Security

This process executes caller-supplied prompts with the host's Claude
credentials. Its only intended caller is the gateway's proxy — that is where
auth, tenancy, rate limits, and payment live. Never expose it directly to
the internet; run it where only the gateway (or the tunnel the gateway
points at) can reach it.

## Production shape

Swap the per-request `claude -p` exec for a long-running Claude Agent SDK
service (persistent sessions, tools, memory) behind the same HTTP contract.
The gateway neither knows nor cares what is behind the URL — that contract
stability is what makes "manage and measure first, monetize later" work.

This module is intentionally not in `go.work` and not in the CI matrix: it
is an example, not a shipped surface (hence `GOWORK=off` when building
inside the repo tree). Add it to both if it should be gated.
