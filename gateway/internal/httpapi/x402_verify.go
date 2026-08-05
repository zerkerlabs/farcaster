package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/credential"
	"github.com/zerkerlabs/gateway/x402types"
)

// The x402 wire types (Payment, PaymentPayload, Authorization,
// PaymentRequirements) now live in the shared x402types module, generated from
// its OpenAPI contract (ADR-0010). This file keeps only the gateway-side
// verification logic that operates on them.

// Verification failures. They are distinct so unit tests can pin the exact
// reason, but the HTTP gate collapses them all to a single coarse 402 body
// ("payment verification failed") — no internal detail leaks to the caller
// (AGENTS.md invariant #3).
var (
	// errPaymentDecode covers a malformed X-PAYMENT header: bad base64 or bad JSON.
	errPaymentDecode = errors.New("x402: malformed payment payload")
	// errPaymentScheme is returned when the payment scheme does not match the
	// advertised requirement (v1: "exact").
	errPaymentScheme = errors.New("x402: scheme mismatch")
	// errPaymentNetwork is returned when the payment network does not match the
	// advertised requirement (v1: "base").
	errPaymentNetwork = errors.New("x402: network mismatch")
	// errPaymentRecipient is returned when the authorization's `to` is not the
	// operator's configured pay_to.
	errPaymentRecipient = errors.New("x402: recipient mismatch")
	// errPaymentAmountFormat is returned when an amount (the authorization value
	// or the required amount) is not a base-10 integer.
	errPaymentAmountFormat = errors.New("x402: non-integer amount")
	// errPaymentAmount is returned when the authorization value is below the
	// required amount.
	errPaymentAmount = errors.New("x402: amount below maxAmountRequired")
	// errPaymentValidityFormat is returned when a validity bound is not a base-10
	// integer (unix seconds).
	errPaymentValidityFormat = errors.New("x402: invalid validity window")
	// errPaymentNotYetValid is returned when the authorization's validAfter is in
	// the future relative to now.
	errPaymentNotYetValid = errors.New("x402: authorization not yet valid")
	// errPaymentExpired is returned when now is at or past the authorization's
	// validBefore.
	errPaymentExpired = errors.New("x402: authorization expired")
	// errPaymentReplayed is returned when the (payer, nonce) pair has already
	// been presented on a verified call (best-effort replay guard, spec 0005 T6
	// — "Replay is a known gate-only limitation"; not authoritative single-use
	// enforcement, which requires settlement, deferred).
	errPaymentReplayed = errors.New("x402: authorization already used")
)

// parseX402Payment base64-decodes then JSON-decodes an X-PAYMENT header value
// into an x402types.Payment. A decode failure (bad base64 or bad JSON) is folded into
// errPaymentDecode so the gate can treat any malformed header as a fail-closed
// 402 rather than a 5xx (spec 0005: "A malformed X-PAYMENT is a 402, never a
// 5xx"). The raw payload is never logged (AGENTS.md invariant #4).
func parseX402Payment(headerB64 string) (*x402types.Payment, error) {
	raw, err := base64.StdEncoding.DecodeString(headerB64)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errPaymentDecode, err)
	}
	var pmt x402types.Payment
	if err := json.Unmarshal(raw, &pmt); err != nil {
		return nil, fmt.Errorf("%w: %w", errPaymentDecode, err)
	}
	return &pmt, nil
}

// checkPaymentParams enforces the non-cryptographic parameter checks for an
// `exact`-scheme payment against the advertised requirement (spec 0005 T4a).
// It deliberately does NOT verify the signature and does NOT check the asset
// contract — both are bound in the EIP-712 domain and verified at T4b (#115).
// now is the reference time for the validity window (unix seconds); callers
// pass time.Now().UTC(). Each failure returns a distinct sentinel; on success
// it returns nil.
func checkPaymentParams(pmt *x402types.Payment, req x402types.PaymentRequirements, now time.Time) error {
	if pmt.Scheme != req.Scheme {
		return errPaymentScheme
	}
	if pmt.Network != req.Network {
		return errPaymentNetwork
	}

	authz := pmt.Payload.Authorization

	// Recipient must be the operator's pay_to. Hex addresses are case-insensitive
	// (EIP-55 checksums only vary the casing), so compare case-insensitively.
	if !strings.EqualFold(authz.To, req.PayTo) {
		return errPaymentRecipient
	}

	// Amounts are 256-bit integers transported as decimal strings; compare via
	// math/big. A non-integer value (or requirement) is rejected outright.
	value, ok := new(big.Int).SetString(authz.Value, 10)
	if !ok {
		return errPaymentAmountFormat
	}
	required, ok := new(big.Int).SetString(req.MaxAmountRequired, 10)
	if !ok {
		return errPaymentAmountFormat
	}
	if value.Cmp(required) < 0 {
		return errPaymentAmount
	}

	// Validity window: require validAfter <= now < validBefore (unix seconds).
	validAfter, err := strconv.ParseInt(authz.ValidAfter, 10, 64)
	if err != nil {
		return errPaymentValidityFormat
	}
	validBefore, err := strconv.ParseInt(authz.ValidBefore, 10, 64)
	if err != nil {
		return errPaymentValidityFormat
	}
	nowUnix := now.Unix()
	if nowUnix < validAfter {
		return errPaymentNotYetValid
	}
	if nowUnix >= validBefore {
		return errPaymentExpired
	}

	return nil
}

// PaymentVerifier confirms the cryptographic validity of an x402 payment
// authorization. It is the seam that isolates the signature crypto so each
// network (Base/USDC now, Solana next — spec 0005 Decision 3) is a distinct
// implementation behind the same interface.
type PaymentVerifier interface {
	// VerifySignature confirms the authorization's signature. The production
	// implementation is baseUSDCVerifier (EIP-712/secp256k1 for Base/USDC,
	// x402_eip712.go); tests inject a permissive verifier via WithPaymentVerifier.
	VerifySignature(pmt *x402types.Payment, req x402types.PaymentRequirements) error
}

// Settler settles a verified x402 payment authorization by handing it to a
// facilitator for on-chain collection (spec 0006 Decision 1). It sits beside
// PaymentVerifier: verification of the signature and parameters stays local
// (surface-5, this file) — Settler is the only seam that reaches the network,
// and only a tenant-configured facilitator's `/settle` endpoint, never a
// direct chain RPC (spec 0006 Decision 8). Alternate or self-hosted
// facilitators are just another implementation behind this interface; the
// production implementation is facilitatorSettler (x402_settle.go).
type Settler interface {
	// Settle submits a verified payment authorization to the facilitator at
	// facilitatorURL for collection. authType and credPlaintext are the
	// tenant's resolved facilitator credential (see resolveFacilitator);
	// credential.AuthTypeNone (or an empty credPlaintext) means no auth header
	// is sent. On success it returns the facilitator's settle result. Any
	// failure to collect — the facilitator unreachable, its response
	// malformed, or the facilitator reporting the settle as unsuccessful — is
	// returned as a *SettleError with a coarse Reason (spec 0006: "a
	// settlement failure is never a 5xx leaking internal detail"). Neither the
	// raw payment payload nor the credential is ever logged (AGENTS.md
	// invariant #4).
	Settle(ctx context.Context, facilitatorURL string, authType credential.AuthType, credPlaintext []byte, pmt *x402types.Payment, req x402types.PaymentRequirements) (*SettleResult, error)
}

// verifyX402 runs the full payment gate for a request against the advertised
// requirement: decode X-PAYMENT, check the parameters against now, verify the
// signature, then check the (payer, nonce) pair against the best-effort replay
// guard (spec 0005 T6). On success it returns the decoded payment so the
// caller can capture payment metadata on the invocation (spec 0005 T5). Any
// failure is returned to the caller as an error; the HTTP handler maps every
// failure to a single coarse 402 (invariant #3). The raw X-PAYMENT payload is
// never logged (invariant #4).
//
// The replay check runs last, after the signature is confirmed valid: an
// unverified (garbage or forged) payload never consumes a nonce slot, so it
// cannot be used to pre-emptively block a legitimate caller's authorization.
func (h *Handler) verifyX402(r *http.Request, req x402types.PaymentRequirements) (*x402types.Payment, error) {
	pmt, err := parseX402Payment(r.Header.Get("X-PAYMENT"))
	if err != nil {
		return nil, err
	}
	if err := checkPaymentParams(pmt, req, time.Now().UTC()); err != nil {
		return nil, err
	}
	if err := h.paymentVerifier.VerifySignature(pmt, req); err != nil {
		return nil, err
	}
	authz := pmt.Payload.Authorization
	if h.replayGuard.reserve(authz.From, authz.Nonce, time.Now().UTC()) {
		return nil, errPaymentReplayed
	}
	return pmt, nil
}

// paymentMetadata extracts the observability fields to record on the
// invocation from a verified x402 payment (spec 0005 T5, mirroring surface-4's
// mcp_method/mcp_tool additive capture). asset is the agent's configured
// Pricing.Asset symbol: v1 prices exactly one asset (USDC on Base, Decision 3)
// and it is not itself part of the signed authorization, so it comes from
// configuration rather than the wire payload.
func paymentMetadata(pmt *x402types.Payment, asset string) (network, assetOut, amount, payer, nonce *string) {
	authz := pmt.Payload.Authorization
	n := pmt.Network
	a := asset
	v := authz.Value
	f := authz.From
	nc := authz.Nonce
	return &n, &a, &v, &f, &nc
}
