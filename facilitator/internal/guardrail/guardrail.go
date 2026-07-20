// Package guardrail implements the facilitator's per-account guardrails (spec
// 0007 T6, Decision 8): the policy limits that bound the blast radius of a
// compromised client certificate. Every facilitator account settles under
// three limits — the (network, asset) pairs it may use, a conservative
// per-transaction maximum, and a per-account daily ceiling — sourced from
// service-wide conservative defaults unless the account carries its own
// override (account.Account.Guardrails).
//
// Check is a pure function, mirroring the verify package's convention: it
// performs no I/O and every rejection is a typed sentinel a caller can pin in
// tests, while the /settle handler (T5) collapses all of them to a single
// coarse response so no configured limit value ever leaks to the caller. It
// runs after independent re-verification and before nonce claim and on-chain
// submission — an over-limit request never spends gas.
package guardrail

import (
	"errors"
	"math/big"
	"strings"
	"time"
)

// Typed rejection reasons. Every failing check in Check returns one of these
// sentinels so a caller can pin the exact reason in tests (errors.Is) while
// the /settle handler collapses all of them to a single coarse response
// (AGENTS.md invariant #3 — no internal limit value leaks).
var (
	// ErrKindNotAllowed is returned when the requirement's (network, asset)
	// pair is not in the account's allowed set.
	ErrKindNotAllowed = errors.New("guardrail: network/asset not allowed for this account")
	// ErrAmountExceedsMax is returned when the settle amount exceeds the
	// account's conservative per-transaction maximum.
	ErrAmountExceedsMax = errors.New("guardrail: amount exceeds per-transaction maximum")
	// ErrDailyCeilingExceeded is returned when adding the settle amount to the
	// account's already-settled total for the current UTC day would exceed its
	// daily ceiling.
	ErrDailyCeilingExceeded = errors.New("guardrail: daily ceiling exceeded")
	// ErrMixedAssetKinds is returned by Limits.Validate when AllowedKinds
	// references more than one distinct asset. The daily ceiling sums raw
	// settle amounts across every Kind in AllowedKinds, so a policy that
	// allow-lists two assets with different decimal precision would let that
	// sum mix incompatible units. Until the ceiling is tracked per (network,
	// asset) instead of per account, every Kind in a Limits must share the
	// same asset.
	ErrMixedAssetKinds = errors.New("guardrail: allowed kinds must share a single asset")
)

// Kind is one (network, asset) pair an account is permitted to settle.
type Kind struct {
	Network string
	Asset   string
}

// Limits is one account's complete guardrail policy (spec 0007 Decision 8):
// the (network, asset) pairs it may settle, a conservative per-transaction
// maximum settle amount, and a per-account daily ceiling.
//
// There is no "unlimited" sentinel for MaxSettleAmount or DailyCeiling —
// guardrails exist to be conservative, so both must be a concrete,
// non-negative amount. A nil amount is a misconfiguration for the caller to
// reject closed, not an implicit "no limit" (see settle.Handler). An empty or
// nil AllowedKinds is a valid, if extreme, policy: the account may settle
// nothing.
//
// Per-account overrides (account.Account.Guardrails) replace a service
// default wholesale — Limits is not merged field-by-field with the default,
// so an override must specify every field it wants enforced.
//
// AllowedKinds may name more than one (network, asset) pair, but every Kind
// must share the same asset (see Validate, ErrMixedAssetKinds): DailyCeiling
// is enforced as a single sum across all of an account's settles regardless
// of which allowed Kind each used, so mixing assets with different decimal
// precision in one Limits would compare incompatible units.
type Limits struct {
	AllowedKinds    []Kind
	MaxSettleAmount *big.Int
	DailyCeiling    *big.Int
}

// allowed reports whether network/asset is in AllowedKinds. Asset addresses
// are compared case-insensitively, matching verify.Policy's convention (hex
// checksums vary only in casing).
func (l Limits) allowed(network, asset string) bool {
	for _, k := range l.AllowedKinds {
		if k.Network == network && strings.EqualFold(k.Asset, asset) {
			return true
		}
	}
	return false
}

// Validate reports whether l is a well-formed guardrail policy: every Kind in
// AllowedKinds must share the same asset (see ErrMixedAssetKinds). Callers
// that resolve a Limits from configuration or an account override — not
// Check/CheckKindAndAmount, which assume an already-valid Limits — should
// call Validate and reject closed on error, the same as a missing
// MaxSettleAmount or DailyCeiling.
func (l Limits) Validate() error {
	if len(l.AllowedKinds) == 0 {
		return nil
	}
	first := l.AllowedKinds[0].Asset
	for _, k := range l.AllowedKinds[1:] {
		if !strings.EqualFold(k.Asset, first) {
			return ErrMixedAssetKinds
		}
	}
	return nil
}

// CheckKindAndAmount enforces the two guardrails that depend only on this
// request — the requirement's (network, asset) pair is in
// limits.AllowedKinds, and amount does not exceed limits.MaxSettleAmount —
// without looking at any prior state. It performs no I/O.
//
// The third guardrail, the per-account daily ceiling, is deliberately not
// part of this function: it depends on the account's already-committed total
// for the day, and checking it against a snapshot read here (before a nonce
// claim commits) would let concurrent settles for the same account each read
// the same prior total, each pass independently, and collectively exceed the
// ceiling. settle.Handler enforces the ceiling atomically with the nonce
// claim instead (see settlement.Store.Claim's DailyCeiling parameter).
func CheckKindAndAmount(limits Limits, network, asset string, amount *big.Int) error {
	if !limits.allowed(network, asset) {
		return ErrKindNotAllowed
	}
	if amount.Cmp(limits.MaxSettleAmount) > 0 {
		return ErrAmountExceedsMax
	}
	return nil
}

// Check is CheckKindAndAmount followed by the daily-ceiling check against
// priorToday — the account's already-settled total for the current UTC day
// (settlement.Store.SumSettled, windowed from StartOfUTCDay). It performs no
// I/O. Production code does not call this end-to-end form: priorToday must
// be read atomically with the nonce claim to avoid the race described on
// CheckKindAndAmount, so settle.Handler calls CheckKindAndAmount directly and
// leaves the ceiling to Store.Claim. Check remains for tests and any caller
// that already holds a consistent priorToday snapshot. limits.MaxSettleAmount
// and limits.DailyCeiling must be non-nil — callers are responsible for
// rejecting a misconfigured Limits before calling Check (see settle.Handler).
func Check(limits Limits, network, asset string, amount, priorToday *big.Int) error {
	if err := CheckKindAndAmount(limits, network, asset, amount); err != nil {
		return err
	}
	total := new(big.Int).Add(priorToday, amount)
	if total.Cmp(limits.DailyCeiling) > 0 {
		return ErrDailyCeilingExceeded
	}
	return nil
}

// StartOfUTCDay truncates t to midnight UTC on its calendar day — the window
// boundary the per-account daily ceiling resets on.
func StartOfUTCDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
