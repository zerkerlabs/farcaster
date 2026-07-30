// Package receipt defines the narrow interface Rooms needs against an
// external, multi-party trust-receipt backend: emit one receipt for a
// cross-agent interaction — who called whom, in which room, when, and how it
// turned out — and read back the receipts recorded for a room. That is all
// Rooms consumes — this package does not design a receipt product; there is
// no schema for signing, verification, pagination, or filtering here.
//
// The real backend does not exist yet. This package therefore ships an
// in-memory Fake behind the same Emitter interface so Rooms can be built and
// tested today; a real client lands later without changing the interface
// (rooms/internal/memory is the same pattern for another external system with
// no client yet). gateway/internal/receipt.Emitter is the analogous seam on
// the gateway side; this package mirrors its shape without importing it,
// since the two live in separate modules and separate services.
//
// Emission is asynchronous and fails open — the opposite of the memory
// seam's fail-closed behaviour. A room operation's correctness never depends
// on a receipt landing, so a slow or down backend must never block or fail
// the interaction it would have receipted (see rooms/internal/httpapi's
// emitReceipt call site).
package receipt

import (
	"context"
	"time"
)

// Outcome values a Receipt may record.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
)

// Receipt is the auditable summary of one cross-agent interaction: an
// addressed message delivered as a proxied call from one room member to
// another member's agent through the gateway. It carries only metadata that
// is safe to record — never a credential, a token, or the message body
// (AGENTS.md invariant #4).
type Receipt struct {
	RoomID       string
	TenantID     string
	FromMemberID string
	ToMemberID   string
	ToAgentID    string
	// InvocationID is the gateway's identifier for the proxied call, the join
	// key between this receipt and the gateway's invocation record. Empty
	// when the call never reached one, e.g. a call refused before it was
	// ever sent to the gateway.
	InvocationID string
	// Outcome is OutcomeSucceeded or OutcomeFailed.
	Outcome string
	// FailureClass is the gateway.ErrorClass of a failed call, e.g.
	// "caller_error" or "upstream_failure". Empty when Outcome is
	// OutcomeSucceeded.
	FailureClass string
	OccurredAt   time.Time
}

// Emitter delivers a Receipt to the trust backend, and reads back the
// receipts recorded for a room. Implementations must be safe for concurrent
// use. Emit may block (e.g. an HTTP round-trip), so callers run it off the
// request path; the returned error is advisory only — emission is fail-open,
// so a failed or slow emission must never fail or delay the room operation it
// describes.
type Emitter interface {
	Emit(ctx context.Context, r Receipt) error

	// ListByRoom returns the receipts recorded for roomID within tenantID,
	// oldest first. A room with no receipts returns an empty slice and a nil
	// error — never an error. Implementations must never return a receipt
	// belonging to a different tenant, even for the same room ID.
	ListByRoom(ctx context.Context, tenantID, roomID string) ([]Receipt, error)
}
