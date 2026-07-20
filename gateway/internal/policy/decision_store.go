package policy

import (
	"context"
	"log/slog"
	"time"
)

const (
	// decisionIDPrefix is the per-type prefix for a policy decision's opaque
	// "<prefix>_<uuidv7>" ID (ADR-0009).
	decisionIDPrefix = "pdec"

	// decisionDefaultLimit is the ListRecent page size when the caller passes a
	// non-positive limit. Mirrors the invocations list default.
	decisionDefaultLimit = 20
)

// storedFromRecorded projects a captured RecordedDecision into a StoredDecision
// with the store-assigned id and createdAt, copying the MCPTool pointer so the
// stored record never aliases the caller's value.
func storedFromRecorded(id string, d RecordedDecision, createdAt time.Time) *StoredDecision {
	rec := &StoredDecision{
		ID:          id,
		TenantID:    d.TenantID,
		AgentID:     d.AgentID,
		Protocol:    d.Protocol,
		Action:      d.Decision.Action,
		MatchedRule: d.Decision.MatchedRule,
		Reason:      d.Decision.Reason,
		CreatedAt:   createdAt,
	}
	if d.MCPTool != nil {
		tool := *d.MCPTool
		rec.MCPTool = &tool
	}
	return rec
}

// StoredDecision is one persisted policy Decision plus the identity the store
// assigns it (spec 0009, ticket T5). It is what GET /v1/policy/decisions
// returns — the read side of the RecordedDecision the enforcement point (T4)
// captures. Reason is the coarse, rule-position explanation the evaluator
// produced; a request body is never part of it (invariant #3).
type StoredDecision struct {
	// ID is a server-assigned "pdec_<uuidv7>" opaque identifier (ADR-0009).
	ID       string
	TenantID string
	AgentID  string
	Protocol string
	MCPTool  *string
	Action   Action
	// MatchedRule is the 1-based rule position that produced Action, or empty
	// when Action came from the policy's default/on_error (Decision, evaluate.go).
	MatchedRule string
	Reason      string
	// CreatedAt is when the decision was recorded, assigned by the store.
	CreatedAt time.Time
}

// DecisionStore persists policy decisions and reads them back for a tenant
// (spec 0009, ticket T5). Every method is scoped to a tenantID — no
// implementation may read or write another tenant's decisions (invariant #2,
// AGENTS.md; spec 0009 "Multi-tenant isolation": cross-tenant = 404).
//
// Insert returns an error so callers can decide how to handle a write failure;
// the inline enforcement path wraps a DecisionStore in a StoreRecorder, which
// makes capture async and fail-open (a store hiccup must never affect the
// proxied call the decision describes).
type DecisionStore interface {
	// Insert persists d, assigning it an ID and CreatedAt, and returns the
	// stored record.
	Insert(ctx context.Context, d RecordedDecision) (*StoredDecision, error)

	// ListRecent returns the tenant's most-recent decisions first, capped at
	// limit. A tenant with no decisions yields an empty slice, not an error.
	ListRecent(ctx context.Context, tenantID string, limit int) ([]*StoredDecision, error)
}

// StoreRecorder adapts a DecisionStore to the DecisionRecorder seam the
// enforcement point calls (spec 0009, ticket T5). Record persists the decision
// and is fail-open: a store write failure is logged, never propagated — capture
// must never add latency to, or fail, the proxied call it describes (the
// handler already invokes Record off the request path). Reads go straight to
// the wrapped store via GET /v1/policy/decisions, not through this adapter.
type StoreRecorder struct {
	store  DecisionStore
	logger *slog.Logger
}

// NewStoreRecorder returns a StoreRecorder writing to store. logger must be
// non-nil; capture failures are logged at Error level and otherwise swallowed.
func NewStoreRecorder(store DecisionStore, logger *slog.Logger) *StoreRecorder {
	return &StoreRecorder{store: store, logger: logger}
}

// Record implements DecisionRecorder.
func (r *StoreRecorder) Record(ctx context.Context, d RecordedDecision) {
	if _, err := r.store.Insert(ctx, d); err != nil {
		r.logger.Error("policy decision capture: persist decision",
			"tenant", d.TenantID, "agent", d.AgentID, "action", d.Decision.Action, "err", err)
	}
}
