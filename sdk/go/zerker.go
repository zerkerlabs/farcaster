// Package zerker is the Go SDK for the Zerker gateway and x402
// facilitator. Skeleton (ADR-0010): it re-exports the x402 wire types from the
// generated x402types contract so SDK users depend on one package; typed client
// methods land alongside the surfaces they call.
package zerker

import "github.com/zerkerlabs/farcaster/x402types"

// Payment is the decoded X-PAYMENT payload (x402types wire contract).
type Payment = x402types.Payment

// PaymentPayload is the `exact`-scheme payload (x402types wire contract).
type PaymentPayload = x402types.PaymentPayload

// Authorization is the EIP-3009 authorization (x402types wire contract).
type Authorization = x402types.Authorization

// PaymentRequirements is one 402-challenge entry (x402types wire contract).
type PaymentRequirements = x402types.PaymentRequirements

// SettleRequest is the POST /settle request body (x402types wire contract).
type SettleRequest = x402types.SettleRequest

// SettleResponse is the POST /settle response body (x402types wire contract).
type SettleResponse = x402types.SettleResponse

// Version is the x402 wire conformance version this SDK targets.
const Version = x402types.Version
