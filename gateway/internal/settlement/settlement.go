// Package settlement provides the tenant-scoped settlement configuration
// domain type and its storage interfaces (spec 0006, ticket T1).
//
// A tenant's Config points the gateway at an x402 facilitator: the facilitator
// URL plus a credential_ref for facilitator auth. The gateway holds no key and
// is never in the money flow (spec 0006 Decision 1) — this package covers only
// the config resource; the facilitator client and settle-then-forward
// orchestration are later tickets (T2, T4).
package settlement

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors.
var (
	// ErrNotFound is returned by Get when a tenant has not configured
	// settlement. Absent config means priced routes stay gate-only
	// (surface-5 behavior); settlement is opt-in (spec 0006 Decision 1).
	ErrNotFound = errors.New("settlement config not found")

	// ErrIncomplete is returned by Upsert when no config exists yet for the
	// tenant and fields does not supply both FacilitatorURL and
	// FacilitatorCredentialRef — the first configure call must set both.
	ErrIncomplete = errors.New("facilitator_url and facilitator_credential_ref are both required to configure settlement")
)

// Config is a tenant's settlement configuration: the facilitator URL and a
// credential_ref for facilitator auth (spec 0006 Decision 1). Facilitator auth
// is always a reference into the envelope-encrypted credential store
// (internal/credential); the bare value is never stored here (invariant #4,
// AGENTS.md).
type Config struct {
	TenantID                 string
	FacilitatorURL           string
	FacilitatorCredentialRef string
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// UpdateFields carries the mutable fields for a PATCH operation. A nil pointer
// means "leave this field unchanged". Neither field is nullable — there is no
// "clear" state; a tenant swaps to a different facilitator/credential rather
// than reverting to unconfigured.
type UpdateFields struct {
	FacilitatorURL           *string
	FacilitatorCredentialRef *string
}

// Store persists and retrieves the tenant's settlement Config. Every method is
// scoped to tenantID — implementations must never read or mutate a record
// belonging to a different tenant (invariant #2, AGENTS.md).
type Store interface {
	// Get fetches the tenant's settlement config. Returns ErrNotFound if the
	// tenant has not configured settlement.
	Get(ctx context.Context, tenantID string) (*Config, error)

	// Upsert creates the tenant's settlement config if none exists, or applies
	// non-nil fields from UpdateFields to the existing one. Returns
	// ErrIncomplete if no config exists yet and the resulting record would
	// have an empty FacilitatorURL or FacilitatorCredentialRef.
	Upsert(ctx context.Context, tenantID string, fields UpdateFields) (*Config, error)
}
