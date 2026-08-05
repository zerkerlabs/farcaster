// Package receipt models the trust receipts Zerker emits to Treeship after a
// proxied invocation completes. Treeship is an external system (ADR-0003), so
// this package defines only the payload and the emitter interface; the concrete
// Treeship HTTP emitter is wired separately and is a follow-up.
package receipt

import (
	"context"
	"time"
)

// Receipt is the auditable summary of one proxied invocation, handed to Treeship
// for a tamper-evident trust receipt. It carries only metadata that is safe to
// record — never secret material or request/response bodies (invariant #4).
type Receipt struct {
	InvocationID   string
	TenantID       string
	AgentID        string
	Mode           string // "transactional" | "streaming"
	Status         string // "succeeded" | "failed"
	UpstreamStatus *int   // upstream HTTP status; nil if the upstream was never reached
	LatencyMS      *int64
	RequestSize    *int64
	ResponseSize   *int64
	CompletedAt    time.Time
}

// Emitter delivers a receipt to the trust backend. Implementations must be safe
// for concurrent use. Emit may block (e.g. an HTTP round-trip), so callers run
// it off the request path; the returned error is advisory only — emission is
// fail-open, so a failed receipt must never fail the underlying invocation
// (spec 0002 Q9).
// A nil Emitter means receipts are disabled: the proxy skips emission entirely
// (see Handler.emitReceipt's nil guard), so there is no separate no-op type.
type Emitter interface {
	Emit(ctx context.Context, r Receipt) error
}
