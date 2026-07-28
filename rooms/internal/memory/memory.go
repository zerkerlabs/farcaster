// Package memory defines the narrow interface Rooms needs against an
// external per-agent memory backend: read the entries recorded for a scope,
// and append a new one. That is all Rooms consumes — this package does not
// design a memory product; there is no query language, no ranking, and no
// embeddings here.
//
// The real backend does not exist yet. This package therefore ships an
// in-memory Fake behind the same Store interface so Rooms can be built and
// tested today; a real client lands later without changing the interface
// (gateway/internal/receipt.Emitter is the same pattern for an external
// system with no client yet).
package memory

import (
	"context"
	"time"
)

// Scope identifies whose memory is being read or appended to: one agent
// within one tenant. Rooms is single-tenant (AGENTS.md invariant #2), so a
// scope is never resolved across tenants — an implementation must never let
// one tenant's or one agent's entries leak into another's read.
type Scope struct {
	TenantID string
	AgentID  string
}

// Entry is one item recorded in an agent's memory scope. It is deliberately
// minimal: Rooms reads and appends entries verbatim, and never interprets,
// ranks, or embeds their content.
type Entry struct {
	Content   string
	CreatedAt time.Time
}

// Store is the seam Rooms needs against an external agent-memory backend.
// Implementations must be safe for concurrent use.
type Store interface {
	// Read returns every entry recorded for scope, oldest first. A scope with
	// no entries yet returns an empty slice and a nil error — never an error.
	Read(ctx context.Context, scope Scope) ([]Entry, error)

	// Append records content as a new entry under scope, returning the entry
	// with its server-assigned CreatedAt.
	Append(ctx context.Context, scope Scope, content string) (Entry, error)
}
