# facilitator

The Zerker-hosted x402 **facilitator** — the `/settle` server the OSS gateway
POSTs verified payment authorizations to. It holds the gas key and submits the
EIP-3009 `transferWithAuthorization` on-chain (the gateway never does).

**Status: mTLS + account mapping + operational endpoints; settlement still a
skeleton.** `POST /settle`
requires a mutually-authenticated TLS connection (spec 0007 Decision 3): the
server requires and verifies a client certificate against a configured client
CA, then maps the certificate's identity to a **facilitator account**. A
connection presenting no certificate, or one that doesn't chain to the
configured client CA, is rejected at the TLS handshake; a valid certificate
that maps to no active account gets `403`. Neither path touches a chain. Once
authenticated, `handleSettle` still just decodes the shared wire contract
(`x402types.SettleRequest`/`SettleResponse`) and returns `501` — the on-chain
settle implementation (go-ethereum, chain RPC, gas-key custody, the fee model)
lands with the rest of **spec 0007**. This module owns those heavy
dependencies so the gateway module stays lean (ADR-0010).

`internal/verify` implements the independent re-verification `/settle` will
run before submitting anything on-chain (spec 0007 Decision 4, T2): signature
recovery, `value`/`payTo` bounds, the validity window, and the (network,
asset) policy check, as a pure function over `x402types.SettleRequest`. It is
not yet wired into `handleSettle` — that lands with the rest of the `/settle`
orchestration (T5), alongside nonce dedupe (T4) and on-chain submission (T3).

`GET /supported` and `GET /healthz` are real (spec 0007 T7), no auth required.
`/supported` advertises the `(scheme, network, asset)` kinds this deployment
settles. `/healthz` reports liveness always, and readiness additionally checks
RPC reachability and the gas wallet's balance against its low-water mark
(`internal/chain.ReadinessChecker`) — either failing returns `503`, and the
same check gates `/settle` so a drained gas tank or unreachable RPC fails
closed instead of letting a settlement silently revert.

Configure the server via `FACILITATOR_TLS_CERT_FILE` / `FACILITATOR_TLS_KEY_FILE`
(the facilitator's own server certificate) and `FACILITATOR_CLIENT_CA_FILE` (the
CA that signs caller client certificates). All three are required — the process
refuses to start otherwise, rather than falling back to a plaintext or
unauthenticated listener. Facilitator accounts are provisioned manually /
allowlisted for v1 (Decision 7); issuing production client certificates and the
real client CA are explicitly out of scope here.

Readiness is configured via `FACILITATOR_RPC_URL`, `FACILITATOR_NETWORK`,
`FACILITATOR_USDC_ADDRESS`, `FACILITATOR_GAS_WALLET_ADDRESS`, and
`FACILITATOR_GAS_LOW_WATER_MARK_WEI` (wei, base-10) — also all required. The
gas wallet address is configured independently of the eventual settle
`Signer` here; T5 wires the real signer selection (KMS/local), and the two
must name the same address in a real deployment.

`/settle` enforces per-account guardrails (spec 0007 T6, Decision 8) before
any nonce claim or on-chain submission: the allowed `(network, asset)` pairs,
a conservative per-transaction maximum, and a per-account daily ceiling. The
service-wide conservative defaults come from `FACILITATOR_GUARDRAIL_MAX_SETTLE_AMOUNT`
and `FACILITATOR_GUARDRAIL_DAILY_CEILING` (decimal strings in the asset's
smallest unit, matching the x402 wire's `value`/`maxAmountRequired`) — both
required, and the allowed set is always this deployment's one configured
`(network, asset)` kind. An individual `account.Account` may carry its own
`Guardrails` override, which replaces the default wholesale rather than
merging field-by-field. An over-limit request is rejected `429`, never
touching a chain.
