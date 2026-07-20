// Package agent defines the Agent domain type, its storage interface, and the
// sentinel errors used across the Agent Catalog surface (spec 0001) and routing
// surface (spec 0002).
package agent

import (
	"context"
	"errors"
	"time"
)

// Status is the lifecycle state of a registered agent.
type Status string

const (
	// StatusPending is the state for an agent that has no upstream_url and is
	// therefore not yet invocable (spec 0002).
	StatusPending Status = "pending"
	// StatusActive is the state for an agent that has an upstream_url and is
	// ready to receive proxied invocations.
	StatusActive Status = "active"
	// StatusInactive indicates the agent has been soft-deleted.
	StatusInactive Status = "inactive"
)

// ErrNotFound is returned when an agent ID does not exist within the caller's
// tenant. Callers must never distinguish "doesn't exist" from "exists but not
// yours" — both surfaces return this error so that existence is never confirmed
// across tenants (invariant #2, AGENTS.md; spec 0001).
var ErrNotFound = errors.New("agent not found")

// ErrNameConflict is returned when a name is already in use by another active
// agent within the same tenant.
var ErrNameConflict = errors.New("agent name already exists in tenant")

// Agent is the canonical record for a registered agent.
type Agent struct {
	ID            string
	TenantID      string
	Name          string
	Description   string
	Tags          []string
	Metadata      map[string]any
	Status        Status
	CreatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	UpstreamURL   *string // nil → StatusPending; non-nil → StatusActive (spec 0002)
	CredentialRef *string // FK placeholder; nil until a credential is attached
	CaptureBody   bool    // per-agent body-capture toggle; default false (spec 0002 Q10)
	EmitReceipts  bool    // per-agent Treeship-receipt toggle; default false (spec 0002 Q9)

	// Suspended marks the agent as invocation-suspended independently of its
	// lifecycle status. A suspended agent stays in the catalog (active or pending)
	// but rejects invocations with 422 (spec 0002 Q11/Q12).
	Suspended bool

	// InvocationRateLimit is the per-agent invocation rate-limit override in
	// requests per second. When non-nil it overrides the tenant-global limit for
	// this agent's invocation buckets (spec 0002 Q11/Q12). Nil = use global.
	InvocationRateLimit *float64

	// InvocationBurst is the burst size paired with InvocationRateLimit. When
	// InvocationRateLimit is set and this is nil, the gateway uses its default
	// burst depth.
	InvocationBurst *int

	// Protocol is the wire protocol this agent speaks: "http" (default,
	// current behavior) or "mcp" (spec 0004). The empty string is treated as
	// "http" by the store layer, so pre-migration rows are unaffected.
	Protocol string

	// MCPTransport is only meaningful when Protocol == "mcp". v1 accepts only
	// "streamable_http"; "stdio" and "sse" are rejected at write time
	// (spec 0004 decisions 1, 2). Nil when Protocol == "http".
	MCPTransport *string

	// MCPProtocolVersion is the MCP protocolVersion string the upstream
	// negotiates (e.g. "2025-06-18"). Informational only; not enforced.
	MCPProtocolVersion *string

	// Pricing is the x402 metering configuration for this agent (spec 0005).
	// Nil means the agent is unpriced — routes are not gated and behave exactly
	// as before (Decision 5). Non-nil gates the agent's routes on payment.
	Pricing *Pricing
}

// Pricing is the x402 metering configuration for an agent (spec 0005). All
// amounts are integer strings in the asset's smallest unit (e.g. USDC
// 6-decimals). The pay_to address is public; the gateway holds no key
// (Decision 9).
type Pricing struct {
	Amount  string            // integer in the asset's smallest unit (e.g. USDC 6-decimals)
	Asset   string            // asset symbol, e.g. "USDC"
	Network string            // network id, e.g. "base"
	PayTo   string            // operator payout address (public; no server-held key)
	Tools   map[string]string // optional per-mcp_tool overrides {tool: amount}; nil when none
}

// UpdateFields carries the mutable fields for a PATCH operation. A nil pointer
// means "leave this field unchanged".
//
// For nullable fields (UpstreamURL, CredentialRef, InvocationRateLimit,
// InvocationBurst), a non-nil pointer-to-nil means "set this field to null"; a
// non-nil pointer-to-value means "set this field". This double-pointer pattern
// lets PATCH distinguish "field absent from request" from "field explicitly set
// to null".
//
// Setting UpstreamURL to nil (or a pointer-to-nil) transitions the agent to
// StatusPending; setting it to a non-nil string transitions to StatusActive.
type UpdateFields struct {
	Name                *string
	Description         *string
	Tags                *[]string
	Metadata            *map[string]any
	UpstreamURL         **string  // nil=absent; &nil=clear; &s=set to s
	CredentialRef       **string  // nil=absent; &nil=clear; &s=set to s
	CaptureBody         *bool     // nil=absent; non-nil=set to this value
	EmitReceipts        *bool     // nil=absent; non-nil=set to this value
	Suspended           *bool     // nil=absent; non-nil=set to this value
	InvocationRateLimit **float64 // nil=absent; &nil=clear; &f=set to f
	InvocationBurst     **int     // nil=absent; &nil=clear; &n=set to n

	// Protocol is not nullable (empty normalizes to "http"), so it follows the
	// same single-pointer convention as Name: nil=absent, non-nil=set.
	Protocol           *string
	MCPTransport       **string // nil=absent; &nil=clear; &s=set to s
	MCPProtocolVersion **string // nil=absent; &nil=clear; &s=set to s

	// Pricing is nullable: nil=absent (leave unchanged), &nil=clear (unprice
	// the agent), &p=set to p (spec 0005).
	Pricing **Pricing // nil=absent; &nil=clear; &p=set
}

// AgentStore persists and retrieves Agent records. Every method is scoped to
// the tenantID argument — no implementation may read or mutate a record that
// belongs to a different tenant. Tenant scoping is enforced in the storage layer
// so no handler can accidentally leak cross-tenant data (ADR-0006).
type AgentStore interface {
	// Create registers a new agent under tenantID. The caller populates Name,
	// Description, Tags, Metadata, CreatedBy, and optionally UpstreamURL and
	// CredentialRef; the store assigns ID, Status, TenantID, CreatedAt, and
	// UpdatedAt. Status is derived from UpstreamURL: nil → StatusPending,
	// non-nil → StatusActive. Returns ErrNameConflict if an active or pending
	// agent with the same Name already exists in the tenant.
	Create(ctx context.Context, tenantID string, a *Agent) error

	// Get fetches one agent by ID. Returns ErrNotFound if the ID does not exist
	// or belongs to a different tenant.
	Get(ctx context.Context, tenantID, id string) (*Agent, error)

	// List returns the tenant's non-deleted agents (active and pending),
	// offset-paginated. page is 1-based; perPage is the page size. The second
	// return value is the total count of non-deleted agents for the tenant.
	// Inactive (soft-deleted) agents are excluded.
	//
	// protocol optionally filters the result to agents whose (normalized)
	// Protocol matches exactly; the empty string means "no filter" (spec 0004
	// decision 3). Callers must validate protocol is "" or a known value
	// before calling — the store does not reject unknown values.
	List(ctx context.Context, tenantID string, page, perPage int, protocol string) ([]*Agent, int, error)

	// Update applies non-nil fields from UpdateFields to the identified agent
	// and returns the updated record. Returns ErrNotFound if the ID is absent or
	// in another tenant. Returns ErrNameConflict if Name conflicts with another
	// non-deleted agent in the tenant. Status is re-derived from the resulting
	// UpstreamURL after the update is applied.
	Update(ctx context.Context, tenantID, id string, fields UpdateFields) (*Agent, error)

	// Delete soft-deletes an agent by setting its status to StatusInactive.
	// Returns ErrNotFound if the ID is absent, already inactive, or in another
	// tenant. The record is preserved for audit; it is excluded from List results.
	Delete(ctx context.Context, tenantID, id string) error
}

// statusFromUpstreamURL derives the correct non-deleted status for an agent
// based on its upstream_url value.
func statusFromUpstreamURL(u *string) Status {
	if u == nil {
		return StatusPending
	}
	return StatusActive
}

// normalizeProtocol maps the empty string (an omitted create field, or a
// pre-migration row with no protocol column value) onto "http", the default
// protocol (spec 0004 decision 3).
func normalizeProtocol(p string) string {
	if p == "" {
		return "http"
	}
	return p
}
