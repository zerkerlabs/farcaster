// Package policy defines the per-tenant policy document type, its
// tenant-scoped storage interface, and the deterministic evaluation engine
// that resolves a Policy + RequestContext to a Decision (spec 0009, tickets
// T1/T3). It is the data model and evaluator the authoring API (T2) and
// inline enforcement (T4) build on — no HTTP surface, no Cedar dependency
// (see evaluate.go for the language-choice rationale, Decision 2).
package policy

import (
	"context"
	"errors"
	"time"
)

// Action is the outcome a matched rule (or a policy's default/on_error)
// resolves to (spec 0009 Decision 3).
type Action string

const (
	// ActionAllow forwards the call unchanged.
	ActionAllow Action = "allow"
	// ActionWarn forwards the call and records a warning decision.
	ActionWarn Action = "warn"
	// ActionDeny blocks the call before the x402 gate and the forward.
	ActionDeny Action = "deny"
)

// ErrNotFound is returned by Get when a tenant has no policy document. Absent
// policy means the tenant sees zero behavior change — the surface is strictly
// additive (spec 0009 "No policy configured = no behavior change").
var ErrNotFound = errors.New("policy not found")

// Match is the set of conditions a Rule matches an in-flight call against.
// Every populated field must match for the rule to apply; a nil/empty field
// is not a match condition (matches any value). Tool-level matching applies
// only to protocol=mcp agents (spec 0009 Decision 6); protocol=http agents
// are route-level only.
type Match struct {
	// Agents matches the calling agent's ID. Empty/nil matches any agent.
	Agents []string `json:"agents,omitempty"`

	// Tools matches the parsed mcp_tool (surface-4), supporting a trailing
	// "*" wildcard (e.g. "read_*", per the spec example). Empty/nil matches
	// any tool. Meaningless for protocol=http calls.
	Tools []string `json:"tools,omitempty"`

	// MaxBodyBytes matches when the request body size is greater than or
	// equal to this many bytes. Nil means this condition is not part of the
	// rule.
	MaxBodyBytes *int64 `json:"max_body_bytes,omitempty"`

	// RatePerMin matches when the caller's observed rate exceeds this many
	// requests per minute. Nil means this condition is not part of the rule.
	RatePerMin *int `json:"rate_per_min,omitempty"`
}

// Rule is one ordered entry in a Policy's rule list: an Action to take when
// Match applies to the in-flight call. The evaluation engine (T3) evaluates
// rules in order and applies the first match.
type Rule struct {
	Action Action `json:"action"`
	Match  Match  `json:"match"`

	// Classifier optionally delegates this rule's decision to an operator-
	// configured external classifier webhook — the optional semantic rail
	// (spec 0009 Decision 4, ticket T6). Nil (the default) keeps this rule
	// fully deterministic, part of the "pure-deterministic policies make zero
	// external calls" guarantee. When set and this rule matches, the webhook's
	// verdict — not this rule's own Action — becomes the Decision's Action;
	// see ClassifierHook and ClassifierClient.
	Classifier *ClassifierHook `json:"classifier,omitempty"`
}

// Policy is a tenant's policy document (spec 0009 §Surface): a configurable
// default/on_error posture plus an ordered list of match rules. Tenant-scoped
// — one document per tenant_id (ADR-0005; cross-tenant reads 404, never
// leaking existence).
type Policy struct {
	TenantID string
	// Version increments on every successful Put, starting at 1. Surfaced to
	// the caller on PUT (spec 0009 §Surface example response).
	Version int
	// Default is the action applied when no rule matches. Operator-set, not
	// hardcoded fail-closed (spec 0009 Decision 3).
	Default Action
	// OnError is the action applied when the evaluation engine or store
	// errors. Operator-set, independent of Default.
	OnError   Action
	Rules     []Rule
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PutFields carries the fields a PUT supplies. Unlike settlement.UpdateFields
// and agent.UpdateFields, these are not pointers: spec 0009 defines PUT
// /v1/policy as a wholesale replace of the document (§Surface — "Replace the
// calling tenant's policy document"), not a partial patch, so there is no
// "leave unchanged" state to encode and no double-pointer "clear" state to
// distinguish — every Put sets Default, OnError, and Rules to exactly what is
// given, including replacing Rules with an empty list.
type PutFields struct {
	Default Action
	OnError Action
	Rules   []Rule
}

// PolicyStore persists and retrieves a tenant's Policy document. Every method
// is scoped to the tenantID argument — no implementation may read or mutate a
// record belonging to a different tenant (invariant #2, AGENTS.md; spec 0009
// "Multi-tenant isolation").
type PolicyStore interface {
	// Get fetches the tenant's policy document. Returns ErrNotFound if the
	// tenant has not configured a policy.
	Get(ctx context.Context, tenantID string) (*Policy, error)

	// Put replaces the tenant's policy document with fields, creating it if
	// none exists. Version starts at 1 on first Put and increments on every
	// subsequent Put for the tenant.
	Put(ctx context.Context, tenantID string, fields PutFields) (*Policy, error)
}
