// Package e2e holds the surface-7 end-to-end test (spec 0007 T8): a client
// presenting a valid mTLS certificate settles a real testnet USDC transfer
// through the assembled /settle path and the tx hash + settlement row are
// asserted.
//
// The test is gated behind the `e2e` build tag AND a set of FACILITATOR_E2E_*
// environment variables (a Base Sepolia RPC, a funded gas wallet, and a
// USDC-holding payer key). It is excluded from the default `make check` so CI
// stays green without live chain access; run it with `make -C facilitator
// e2e-test` once the env is provisioned (PO/ops — Decision 2/9).
package e2e
