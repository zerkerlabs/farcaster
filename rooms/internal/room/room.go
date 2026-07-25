// Package room defines the Room domain model — Room, Member, and Message — and
// a tenant-scoped in-memory store for them.
//
// A room is a durable, membership-scoped space where two or more agents cowork
// a single task. Rooms are single-tenant: every member of a room belongs to the
// same tenant as the room. Cross-tenant rooms are out of scope (a larger
// identity/consent problem), so every store method scopes reads and mutations
// to a tenant and never lets a caller distinguish "belongs to another tenant"
// from "does not exist" (AGENTS.md invariant #2).
package room

import (
	"errors"
	"time"
)

// State is the lifecycle state of a room.
type State string

const (
	// StateOpen is the initial state: the room is active and accepting
	// members and messages.
	StateOpen State = "open"
	// StateCompleted is a terminal state: the room's goal was met.
	StateCompleted State = "completed"
	// StateAbandoned is a terminal state: the room was given up on.
	StateAbandoned State = "abandoned"
)

// ErrNotFound is returned when a room ID does not exist within the caller's
// tenant. Callers must never distinguish "doesn't exist" from "exists but not
// yours" — both cases return this error so existence is never confirmed across
// tenants (invariant #2, AGENTS.md).
var ErrNotFound = errors.New("room not found")

// ErrTenantMismatch is returned when adding a member whose tenant differs from
// the room's tenant. Rooms are single-tenant: every member must belong to the
// same tenant as the room it joins.
var ErrTenantMismatch = errors.New("member tenant does not match room tenant")

// ErrMemberNotFound is returned when an operation names a member that is not
// seated in the room it targets, so a message can never be attributed to a
// member who was never there.
var ErrMemberNotFound = errors.New("member not found in room")

// Room is the canonical record for a room.
type Room struct {
	ID        string
	TenantID  string
	Goal      string
	State     State
	CreatedAt time.Time
	Members   []*Member
}

// Member is an agent seated in a room.
type Member struct {
	ID       string
	AgentID  string // the agt_-prefixed gateway agent this member represents
	JoinedAt time.Time
}

// Message is one member's contribution to a room.
type Message struct {
	ID        string
	MemberID  string // the Member that authored this message
	Body      string
	CreatedAt time.Time
}
