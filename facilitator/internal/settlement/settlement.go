// Package settlement is the facilitator's record of every on-chain settlement
// and the nonce-idempotency it enforces before a wei of gas is spent (spec 0007
// Decision 9). It lives in the facilitator's *own* Postgres — a separate trust
// domain from the gateway's database — and exists for three reasons: idempotency
// (a retried authorization must never be broadcast twice), support/audit (a
// durable per-account row for every settlement attempt), and the future
// commercial billing/dashboard reads (a later surface).
//
// The idempotency guarantee is built on one atomic primitive: [Store.Claim].
// A settle is keyed by (account, authorization nonce); the first caller to claim
// a key inserts an in-flight row and is told it owns the broadcast, while every
// concurrent or later caller for the same key is handed the existing row and
// told *not* to broadcast. That single atomic step is what makes retries and
// concurrent duplicates single-flight rather than double-spend gas — the
// on-chain nonce is authoritative single-use, but the facilitator dedupes
// *before* broadcast so a retry never wastes gas on a guaranteed revert.
//
// The orchestration that calls this (T5) decides what to do with a returned
// row — replay a settled row's tx hash, report an in-flight or failed one — this
// package only owns the record and the atomic claim.
package settlement

import (
	"context"
	"errors"
	"math/big"
	"time"
)

// ErrNotFound is returned by Get when no settlement matches the (account,
// nonce) key.
var ErrNotFound = errors.New("settlement not found")

// ErrInvalidTransition is returned by MarkSettled/MarkFailed when the row is
// not StatusInFlight and the call does not exactly replay the row's existing
// terminal outcome (see MarkSettled, MarkFailed) — e.g. a different tx hash
// on an already-settled row, or a MarkFailed on a StatusSettled row. The row
// is left unchanged.
var ErrInvalidTransition = errors.New("settlement: invalid state transition")

// Status is the lifecycle of a settlement row.
type Status string

const (
	// StatusInFlight is the initial state set by Claim: the authorization has
	// been claimed for broadcast but no terminal on-chain outcome is recorded
	// yet. A concurrent claim on the same key sees this and must not broadcast.
	StatusInFlight Status = "in_flight"

	// StatusSettled is a successful settlement: the transfer was included
	// on-chain and TxHash is set. A duplicate claim replays this same tx hash.
	StatusSettled Status = "settled"

	// StatusFailed is a terminal failure (on-chain revert, inclusion timeout
	// treated as terminal by the caller, etc.); ErrorReason carries a coarse
	// reason. A duplicate claim reports the same failure without re-broadcast.
	StatusFailed Status = "failed"
)

// Settlement is one row: a single settlement attempt for one payment
// authorization, owned by one facilitator account.
type Settlement struct {
	ID string

	// AccountID is the facilitator account (managed-tier operator) this
	// settlement belongs to. Every read is scoped by it — no account sees
	// another's rows.
	AccountID string

	// Nonce is the EIP-3009 authorization nonce (hex), the payer-chosen
	// single-use value that identifies the authorization. Together with
	// AccountID it is the idempotency key: unique per (account, nonce).
	Nonce string

	// Payer and PayTo are the authorization's from/to, kept for audit and
	// support (not part of the idempotency key).
	Payer string
	PayTo string

	// Network and Asset identify the chain and token settled on; Value is the
	// transferred amount as a decimal string (256-bit — the x402 wire encodes
	// amounts as strings, never JSON numbers; that semantic is preserved here).
	Network string
	Asset   string
	Value   string

	// ValidAfter and ValidBefore are the authorization's EIP-3009 validity
	// window (unix seconds, as decimal strings — matching the x402 wire).
	// Kept so a row can be reconstructed into a full x402types.Authorization
	// without a request to draw from: the periodic sweep (spec 0007 follow-up
	// #197) reconciles a stale in_flight row directly from the store, and
	// chain.Client.Reconcile needs ValidBefore to tell "not yet settled" apart
	// from "can never settle now" (see chain.Client.Reconcile's doc).
	ValidAfter  string
	ValidBefore string

	// TxHash is the on-chain transaction hash, empty until the settlement
	// reaches StatusSettled (or a failed broadcast leaves it empty).
	TxHash string

	Status Status

	// ErrorReason is a coarse failure reason, set only for StatusFailed. It
	// never carries internal detail or key material (AGENTS.md invariant #3).
	ErrorReason string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ClaimKey is the input to [Store.Claim]: the idempotency key (AccountID +
// Nonce) plus the authorization fields recorded on the row that a first claim
// inserts. Only AccountID and Nonce participate in dedupe; the rest are stored
// for audit.
type ClaimKey struct {
	AccountID   string
	Nonce       string
	Payer       string
	PayTo       string
	Network     string
	Asset       string
	Value       string
	ValidAfter  string
	ValidBefore string
}

// DailyCeiling is the per-account daily-guardrail-ceiling parameters [Store.Claim]
// enforces atomically with the nonce claim (spec 0007 T6, Decision 8): Since is
// the UTC-day window start (guardrail.StartOfUTCDay), Amount is this request's
// settle amount, and Limit is the account's configured ceiling.
//
// Claim counts both StatusInFlight and StatusSettled rows as committed spend
// for the window, not just StatusSettled ones. If only settled rows counted,
// two concurrent claims for the same account could each read the same
// (lower) committed total before either finishes settling, each pass the
// ceiling check independently, and collectively exceed it — exactly the race
// this type exists to close. An in-flight row stops counting once it
// transitions to StatusFailed (a later claim naturally sees the reduced
// total); it keeps counting through StatusSettled.
type DailyCeiling struct {
	Since  time.Time
	Amount *big.Int
	Limit  *big.Int
}

// Store persists settlement rows and enforces nonce idempotency. Implementations
// must be safe for concurrent use, and Claim must be atomic: for a given
// (account, nonce), across any number of concurrent callers and across process
// instances, at most one caller may receive claimed=true. Claim must also
// serialize the ceiling check against every other concurrent claim for the
// same account (not just the same nonce) so the daily ceiling cannot be
// exceeded by a burst of distinct-nonce requests (see DailyCeiling).
type Store interface {
	// Claim atomically reserves the right to settle the authorization named by
	// key, subject to ceiling. If no row exists for (AccountID, Nonce):
	//   - if the account's committed total for ceiling's window plus
	//     ceiling.Amount would exceed ceiling.Limit, Claim returns
	//     (nil, false, guardrail.ErrDailyCeilingExceeded) and inserts no row —
	//     the nonce remains unclaimed for a future request;
	//   - otherwise it inserts a row with StatusInFlight and returns
	//     (row, true, nil) — this caller owns the broadcast.
	// If a row already exists for (AccountID, Nonce), Claim returns
	// (existing, false, nil) without re-checking the ceiling (the amount was
	// already committed when the row was first claimed) — the caller must NOT
	// broadcast: it replays the existing outcome (a settled row's tx hash, or
	// reports the in-flight/failed row).
	Claim(ctx context.Context, key ClaimKey, ceiling DailyCeiling) (row *Settlement, claimed bool, err error)

	// MarkSettled transitions a StatusInFlight row to StatusSettled with the
	// given tx hash and returns the updated row. It is idempotent: calling it
	// again on a row already StatusSettled with the same txHash returns the
	// existing row unchanged. Any other call on a non-in-flight row — a
	// different txHash on an already-settled row, or any call on a
	// StatusFailed row — returns ErrInvalidTransition and mutates nothing.
	// Unknown id returns ErrNotFound.
	MarkSettled(ctx context.Context, id, txHash string) (*Settlement, error)

	// MarkFailed transitions a StatusInFlight row to StatusFailed with a
	// coarse reason and, when the chain client returned one (a reverted or
	// timed-out settlement can still have broadcast), the on-chain tx hash —
	// txHash is empty when no transaction was ever broadcast. It is
	// idempotent: calling it again on a row already StatusFailed with the same
	// reason and txHash returns the existing row unchanged. Any other call on
	// a non-in-flight row — a different reason/txHash on an already-failed
	// row, or any call on a StatusSettled row — returns ErrInvalidTransition
	// and mutates nothing. Unknown id returns ErrNotFound.
	MarkFailed(ctx context.Context, id, reason, txHash string) (*Settlement, error)

	// Get returns the settlement for (accountID, nonce), or ErrNotFound.
	Get(ctx context.Context, accountID, nonce string) (*Settlement, error)

	// List returns an account's settlements, newest first — scoped to the
	// account so there is no cross-tenant leakage.
	List(ctx context.Context, accountID string) ([]*Settlement, error)

	// SumSettled returns the total Value of every StatusSettled settlement for
	// accountID created at or after since — for audit/reporting (e.g. the
	// future commercial dashboard). It is not the guardrail-ceiling
	// enforcement path: Claim's DailyCeiling counts in-flight rows too (see
	// DailyCeiling), which this deliberately does not, so do not use this to
	// gate a settle decision.
	SumSettled(ctx context.Context, accountID string, since time.Time) (*big.Int, error)

	// StaleInFlight returns every StatusInFlight row across all accounts
	// created before olderThan, oldest first. It backs the periodic sweep
	// (spec 0007 follow-up #197): unlike every other read here, it is
	// deliberately not scoped to one account — the sweep is an internal
	// operational loop, not a caller-facing read, so cross-tenant visibility
	// into "which rows are stuck" is fine (no settlement *content* beyond what
	// the sweep already needs to reconcile is exposed to any tenant).
	StaleInFlight(ctx context.Context, olderThan time.Time) ([]*Settlement, error)
}
