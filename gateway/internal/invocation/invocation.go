// Package invocation defines the Invocation domain type, its storage interface,
// and sentinel errors for the routing/proxy surface (spec 0002). Every proxied
// call — transactional or streaming — produces one Invocation record that tracks
// its lifecycle, timing, sizes, and (when the per-agent toggle is on) the raw
// request and response bodies.
package invocation

import (
	"context"
	"errors"
	"time"
)

// Mode identifies which proxy endpoint was used for the invocation.
type Mode string

const (
	// ModeTransactional corresponds to POST /v1/proxy/{id}: the caller receives
	// an inv_<id> immediately and polls for the result.
	ModeTransactional Mode = "transactional"
	// ModeStreaming corresponds to POST /v1/proxy/{id}/stream: a long-lived
	// connection that streams request and response chunks verbatim.
	ModeStreaming Mode = "streaming"
)

// Status is the lifecycle state of an invocation.
type Status string

const (
	// StatusPending means the invocation has been accepted but the upstream call
	// has not yet started (transactional mode only).
	StatusPending Status = "pending"
	// StatusRunning means the upstream call is in progress.
	StatusRunning Status = "running"
	// StatusSucceeded means the upstream returned a success response.
	StatusSucceeded Status = "succeeded"
	// StatusFailed means the upstream failed, timed out, or the gateway erred.
	StatusFailed Status = "failed"
)

// ErrNotFound is returned when an invocation ID does not exist within the
// caller's tenant. Callers must never distinguish "doesn't exist" from "exists
// but not yours" — both cases return this error (invariant #2, AGENTS.md).
var ErrNotFound = errors.New("invocation not found")

// SettlementStatus is the lifecycle state of an invocation's settlement
// sub-record (spec 0006). pending is included even though v1 settlement is
// synchronous, so the later async orchestration flip needs no contract change
// (spec 0006 Decision 2).
type SettlementStatus string

const (
	// SettlementStatusPending means settlement has not yet resolved.
	SettlementStatusPending SettlementStatus = "pending"
	// SettlementStatusSettled means the facilitator collected payment.
	SettlementStatusSettled SettlementStatus = "settled"
	// SettlementStatusSettlementFailed means the facilitator failed to collect
	// payment; the upstream is never called for this invocation.
	SettlementStatusSettlementFailed SettlementStatus = "settlement_failed"
	// SettlementStatusSettledUpstreamFailed means settlement succeeded but the
	// subsequent upstream call terminally failed (spec 0006 Decision 5).
	SettlementStatusSettledUpstreamFailed SettlementStatus = "settled_upstream_failed"
)

// ErrorClass is the coarse error taxonomy recorded by the proxy on every
// failure path (migration 007). NULL on success and on rows that predate the
// column — forward-only, no backfill (spec 0003 scoping decision 7).
type ErrorClass string

// Error taxonomy values — one per failure path enumerated in spec 0003.
const (
	ErrorClassTimeout         ErrorClass = "timeout"
	ErrorClassUpstream5xx     ErrorClass = "upstream_5xx"
	ErrorClassUpstream4xx     ErrorClass = "upstream_4xx"
	ErrorClassSSRFBlocked     ErrorClass = "ssrf_blocked"
	ErrorClassCredentialError ErrorClass = "credential_error" //nolint:gosec // taxonomy label, not a credential value
	ErrorClassCancelled       ErrorClass = "cancelled"
	ErrorClassInternal        ErrorClass = "internal"
)

// Invocation is the durable record for one proxied call through Farcaster.
//
// Body fields (RequestBody, ResponseBody) are nil when the per-agent
// body-capture toggle is off (the default — spec 0002 Q10). They are only
// populated when agent.CaptureBody is true at the time of the invocation.
type Invocation struct {
	ID             string
	TenantID       string
	AgentID        string
	Mode           Mode
	Status         Status
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
	UpstreamStatus *int        // HTTP status from the upstream; nil until completed
	LatencyMS      *int64      // total round-trip duration in ms; nil until completed
	RequestSize    *int64      // request body size in bytes; nil when not measured
	ResponseSize   *int64      // response body size in bytes; nil when not measured
	RequestBody    []byte      // nil when body capture is off
	ResponseBody   []byte      // nil when body capture is off
	TTFTMS         *int64      // time-to-first-byte in ms; nil for transactional or when no bytes streamed
	ErrorClass     *ErrorClass // coarse error taxonomy; nil on success
	Model          *string     // caller-supplied model name (X-Farcaster-Model header); nil if absent
	MCPMethod      *string     // parsed MCP JSON-RPC method (e.g. "tools/call"); nil for non-MCP agents (spec 0004)
	MCPTool        *string     // for mcp_method="tools/call", the parsed params.name; nil otherwise (spec 0004)
	PaymentNetwork *string     // x402 payment network (e.g. "base"); nil for unpriced routes (spec 0005)
	PaymentAsset   *string     // x402 payment asset (e.g. "USDC"); nil for unpriced routes (spec 0005)
	PaymentAmount  *string     // x402 payment amount, smallest unit, as TEXT; nil for unpriced routes (spec 0005)
	PaymentPayer   *string     // payer address from the verified x402 authorization; nil for unpriced routes (spec 0005)
	PaymentNonce   *string     // x402 authorization nonce (best-effort replay tracking); nil for unpriced routes (spec 0005)

	// Settlement sub-record (spec 0006) — the canonical receipt. All nil for
	// unpriced/gate-only routes and until settle-then-forward orchestration
	// (T4) populates them.
	SettlementStatus   *SettlementStatus // pending | settled | settlement_failed | settled_upstream_failed
	SettlementTxHash   *string           // on-chain tx reference from the facilitator's settle response
	SettledAmount      *string           // what the payer paid, smallest unit, as TEXT
	OperatorAmount     *string           // what reached pay_to (settled_amount - facilitator_fee)
	FacilitatorFee     *string           // facilitator-reported fee split; "0"/absent in the OSS/self-host path
	SettlementAttempts *int              // bounded-retry attempt count
	SettlementReason   *string           // coarse failure reason, on failure only
	SettledAt          *time.Time        // when the facilitator settle succeeded
}

// UpdateFields carries the fields that change over the lifetime of an
// invocation. A nil pointer means "leave this field unchanged".
//
// Body fields use pointer-to-slice semantics: a nil *[]byte means "absent
// (don't update)"; a non-nil pointer (even to a nil/empty slice) means "set to
// this value". This lets the proxy layer explicitly record an empty body.
type UpdateFields struct {
	Status         *Status
	CompletedAt    *time.Time
	UpstreamStatus *int
	LatencyMS      *int64
	RequestSize    *int64
	ResponseSize   *int64
	RequestBody    *[]byte
	ResponseBody   *[]byte
	TTFTMS         *int64
	ErrorClass     *ErrorClass
	Model          *string
	MCPMethod      *string
	MCPTool        *string
	PaymentNetwork *string
	PaymentAsset   *string
	PaymentAmount  *string
	PaymentPayer   *string
	PaymentNonce   *string

	SettlementStatus   *SettlementStatus
	SettlementTxHash   *string
	SettledAmount      *string
	OperatorAmount     *string
	FacilitatorFee     *string
	SettlementAttempts *int
	SettlementReason   *string
	SettledAt          *time.Time
}

// ListFilter specifies the optional predicates and pagination parameters for
// ListFiltered. Zero values mean "no constraint": empty string, nil pointer,
// zero int. Limit=0 is treated as the store default; callers should clamp it
// to the API maximum (100) before calling.
type ListFilter struct {
	AgentID          string            // empty = all agents within the tenant
	Status           *Status           // nil = all statuses
	Mode             *Mode             // nil = all modes
	ErrorClass       *ErrorClass       // nil = include all error classes (including NULL rows)
	Model            string            // empty = all models
	SettlementStatus *SettlementStatus // nil = all settlement statuses (including NULL rows)
	Since            *time.Time        // nil = no lower bound on created_at
	Until            *time.Time        // nil = no upper bound on created_at
	Limit            int               // 0 → implementation default; callers clamp to max
	Offset           int               // 0-based row offset
}

// Store persists and retrieves Invocation records. Every method is scoped to
// the tenantID argument — no implementation may read or mutate a record that
// belongs to a different tenant (ADR-0005; invariant #2, AGENTS.md).
type Store interface {
	// Create records a new invocation under tenantID. The caller sets AgentID,
	// Mode, Status (initial), RequestSize, and optionally RequestBody; the store
	// assigns ID, TenantID, CreatedAt, and UpdatedAt.
	Create(ctx context.Context, tenantID string, inv *Invocation) error

	// Get fetches one invocation by ID. Returns ErrNotFound if the ID does not
	// exist or belongs to a different tenant.
	Get(ctx context.Context, tenantID, id string) (*Invocation, error)

	// List returns invocations for agentID within tenantID, ordered by
	// created_at descending (most recent first). page is 1-based; perPage is the
	// page size. The second return value is the total count for the agent.
	List(ctx context.Context, tenantID, agentID string, page, perPage int) ([]*Invocation, int, error)

	// ListFiltered returns tenant-scoped invocations matching filter, ordered by
	// created_at descending (most recent first). The second return value is the
	// total count matching the filter before applying Limit/Offset.
	ListFiltered(ctx context.Context, tenantID string, filter ListFilter) ([]*Invocation, int, error)

	// Aggregate returns analytics groups for tenantID over q's [Since, Until]
	// range (inclusive), grouped by agent_id and time bucket (spec 0003). The
	// result is sorted by agent_id then bucket-start; an empty range yields an
	// empty slice (not an error).
	Aggregate(ctx context.Context, tenantID string, q AggregateQuery) ([]AggregateGroup, error)

	// Update applies the non-nil fields from UpdateFields to the identified
	// invocation and returns the updated record. Returns ErrNotFound if the ID
	// is absent or in another tenant.
	Update(ctx context.Context, tenantID, id string, fields UpdateFields) (*Invocation, error)
}
