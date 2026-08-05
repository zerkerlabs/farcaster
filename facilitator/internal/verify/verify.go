// Package verify implements the facilitator's independent re-verification of
// an x402 settle request (spec 0007 Decision 4, a hard invariant): the
// facilitator holds the gas key, so it never trusts that the calling gateway
// already verified a payment. Reverify is a pure function — no chain read, no
// store write, no side effects of any kind — so every rejection it returns is
// established before a wei of gas is spent.
//
// This is deliberately an independent re-implementation, not a call into the
// gateway's local verifier (gateway/internal/httpapi/x402_verify.go /
// x402_eip712.go): the two modules must never share verification code, so a
// bug in one is not automatically a bug in the other.
package verify

import (
	"errors"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/zerkerlabs/gateway/x402types"
)

// Typed rejection reasons. Every failing check in Reverify returns one of
// these sentinels so a caller can pin the exact reason in tests (errors.Is)
// while the /settle handler (T5) collapses all of them to a single coarse
// SettleResponse{Success:false} — no internal detail leaks to the gateway
// (AGENTS.md invariant #3).
var (
	// ErrUnsupportedKind is returned when the requirement's (network, asset)
	// pair is not one this facilitator is configured to settle.
	ErrUnsupportedKind = errors.New("facilitator: network/asset not in policy")
	// ErrRecipientMismatch is returned when the authorization's `to` is not
	// the requirement's payTo.
	ErrRecipientMismatch = errors.New("facilitator: recipient does not match payTo")
	// ErrAmountFormat is returned when the authorization value or the
	// requirement's maxAmountRequired is not a base-10 integer.
	ErrAmountFormat = errors.New("facilitator: non-integer amount")
	// ErrAmountExceedsMax is returned when the authorization value exceeds
	// the requirement's maxAmountRequired.
	ErrAmountExceedsMax = errors.New("facilitator: value exceeds maxAmountRequired")
	// ErrValidityFormat is returned when a validity bound is not a base-10
	// integer (unix seconds).
	ErrValidityFormat = errors.New("facilitator: invalid validity window")
	// ErrNotYetValid is returned when the authorization's validAfter is in
	// the future relative to now.
	ErrNotYetValid = errors.New("facilitator: authorization not yet valid")
	// ErrExpired is returned when now is at or past the authorization's
	// validBefore.
	ErrExpired = errors.New("facilitator: authorization expired")
)

// Kind is one (network, asset) pair the facilitator is configured to settle,
// mirroring an entry of what GET /supported advertises (spec 0007 T7). ChainID
// is the EIP-155 chain ID the EIP-712 domain separator binds the signature to
// (e.g. Base mainnet vs Base Sepolia have distinct IDs and are therefore
// distinct Kinds even though both carry USDC).
type Kind struct {
	Network string
	Asset   string
	ChainID int64

	// AssetName and AssetVersion are the EIP-712 domain name/version of the
	// asset contract (e.g. "USD Coin" / "2" for Circle's USDC deployments).
	AssetName    string
	AssetVersion string
}

// Policy is the set of (network, asset) pairs this facilitator will settle.
// Reverify rejects any request outside the policy before any other check
// runs, so an out-of-policy request never pays for a signature recovery.
type Policy struct {
	Kinds []Kind
}

// lookup returns the Kind matching network/asset, if any. Asset addresses are
// compared case-insensitively (hex checksums vary only in casing).
func (p Policy) lookup(network, asset string) (Kind, bool) {
	for _, k := range p.Kinds {
		if k.Network == network && strings.EqualFold(k.Asset, asset) {
			return k, true
		}
	}
	return Kind{}, false
}

// Reverify independently re-checks a settle request against policy as of now,
// before any gas is spent (spec 0007 Decision 4): the requirement's
// (network, asset) is in policy; the authorization's recipient and amount are
// bound by the requirement; the validity window brackets now; and the EIP-3009
// signature recovers to the authorization's `from`. It performs no chain read,
// store write, or other side effect — a nil return means every check passed;
// any other return is one of the typed rejections above (or an error wrapping
// one, from the signature machinery) and callers must not submit.
//
// Cheap checks run before the signature recovery (the one expensive,
// cryptographic check) so an out-of-policy or out-of-bounds request never
// pays for it.
func Reverify(req x402types.SettleRequest, policy Policy, now time.Time) error {
	reqmts := req.PaymentRequirements
	kind, ok := policy.lookup(reqmts.Network, reqmts.Asset)
	if !ok {
		return ErrUnsupportedKind
	}

	authz := req.PaymentPayload.Payload.Authorization

	if !strings.EqualFold(authz.To, reqmts.PayTo) {
		return ErrRecipientMismatch
	}

	value, ok := new(big.Int).SetString(authz.Value, 10)
	if !ok {
		return ErrAmountFormat
	}
	maxRequired, ok := new(big.Int).SetString(reqmts.MaxAmountRequired, 10)
	if !ok {
		return ErrAmountFormat
	}
	if value.Sign() < 0 || maxRequired.Sign() < 0 {
		return ErrAmountFormat
	}
	if value.Cmp(maxRequired) > 0 {
		return ErrAmountExceedsMax
	}

	validAfter, err := strconv.ParseInt(authz.ValidAfter, 10, 64)
	if err != nil {
		return ErrValidityFormat
	}
	validBefore, err := strconv.ParseInt(authz.ValidBefore, 10, 64)
	if err != nil {
		return ErrValidityFormat
	}
	nowUnix := now.Unix()
	if nowUnix < validAfter {
		return ErrNotYetValid
	}
	if nowUnix >= validBefore {
		return ErrExpired
	}

	return verifySignature(authz, req.PaymentPayload.Payload.Signature, kind)
}
