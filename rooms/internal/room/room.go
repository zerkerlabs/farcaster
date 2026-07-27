// Package room defines the Room domain model — Room, Member, Message, and
// Event — and a tenant-scoped in-memory store for them.
//
// A room is a durable, membership-scoped space where two or more agents cowork
// a single task. Rooms are single-tenant: every member of a room belongs to the
// same tenant as the room. Cross-tenant rooms are out of scope (a larger
// identity/consent problem), so every store method scopes reads and mutations
// to a tenant and never lets a caller distinguish "belongs to another tenant"
// from "does not exist" (AGENTS.md invariant #2).
//
// A room's history is an append-only event log: every membership change,
// message, and lifecycle transition is appended as an Event, in order. The
// room's transcript is a replay of that log rather than a second structure
// kept in sync with it. A room also carries a turn budget: posting a message
// consumes one turn, and exceeding the budget abandons the room rather than
// letting agents loop indefinitely.
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

// ErrRoomTerminated is returned when a caller tries to append to a room that
// has already reached a terminal state (StateCompleted or StateAbandoned).
var ErrRoomTerminated = errors.New("room is terminated")

// ErrTurnBudgetExceeded is returned when posting a message would consume more
// turns than a room's budget allows. The message is rejected and the room
// transitions to StateAbandoned as part of the same call.
var ErrTurnBudgetExceeded = errors.New("room turn budget exceeded")

// DefaultTurnBudget is the turn budget assigned to a room created without an
// explicit one. It bounds how many message-posting turns a room's members may
// spend before the room is abandoned rather than left to loop indefinitely.
const DefaultTurnBudget = 50

// Room is the canonical record for a room.
type Room struct {
	ID         string
	TenantID   string
	Goal       string
	State      State
	CreatedAt  time.Time
	Members    []*Member
	TurnBudget int
	Events     []*Event
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

// EventKind identifies the kind of a room Event.
type EventKind string

const (
	// EventMemberJoined records that an agent was seated in the room.
	EventMemberJoined EventKind = "member_joined"
	// EventMessagePosted records a member's message.
	EventMessagePosted EventKind = "message_posted"
	// EventTaskStateChanged records a transition in the room's shared task
	// state. Reserved for the shared task-state feature; no store method
	// emits it yet.
	EventTaskStateChanged EventKind = "task_state_changed"
	// EventRoomTerminated records the room reaching a terminal state.
	EventRoomTerminated EventKind = "room_terminated"
)

// Event is one entry in a room's append-only log. Every membership change,
// message, and lifecycle transition is appended as an Event, in the order it
// happened; a room's transcript is a replay of this log rather than a second
// structure kept in sync with it.
type Event struct {
	Sequence  int // contiguous, starting at 1, per room
	Kind      EventKind
	Timestamp time.Time
	Payload   any
}

// MemberJoinedPayload is the Payload of an EventMemberJoined event.
type MemberJoinedPayload struct {
	Member *Member
}

// MessagePostedPayload is the Payload of an EventMessagePosted event.
type MessagePostedPayload struct {
	Message *Message
}

// RoomTerminatedPayload is the Payload of an EventRoomTerminated event,
// recording which terminal state the room reached.
type RoomTerminatedPayload struct {
	State State
}
