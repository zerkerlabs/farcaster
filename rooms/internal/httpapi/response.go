package httpapi

import (
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/receipt"
	"github.com/zerkerlabs/farcaster/rooms/internal/room"
)

// roomResponse is the JSON representation of a Room, including its transcript
// replayed from the event log (GET /v1/rooms/{rom_id} returns this in full;
// POST /v1/rooms returns it with no members and an empty transcript).
type roomResponse struct {
	ID         string            `json:"id"`
	Goal       string            `json:"goal"`
	State      string            `json:"state"`
	TurnBudget int               `json:"turn_budget"`
	CreatedAt  time.Time         `json:"created_at"`
	Members    []memberResponse  `json:"members"`
	Transcript []messageResponse `json:"transcript"`
}

// memberResponse is the JSON representation of a Member.
type memberResponse struct {
	ID       string    `json:"id"`
	AgentID  string    `json:"agent_id"`
	JoinedAt time.Time `json:"joined_at"`
}

// messageResponse is the JSON representation of a Message. ToMemberID is
// omitted for a message posted to the room at large, so its presence is what
// marks a message as addressed — and delivered as a proxied call to that
// member's agent.
type messageResponse struct {
	ID         string    `json:"id"`
	MemberID   string    `json:"member_id"`
	ToMemberID string    `json:"to_member_id,omitempty"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
}

// toRoomResponse maps a *room.Room and its replayed transcript to their JSON
// representation.
func toRoomResponse(r *room.Room, transcript []*room.Message) roomResponse {
	members := make([]memberResponse, len(r.Members))
	for i, m := range r.Members {
		members[i] = toMemberResponse(m)
	}
	msgs := make([]messageResponse, len(transcript))
	for i, m := range transcript {
		msgs[i] = toMessageResponse(m)
	}
	return roomResponse{
		ID:         r.ID,
		Goal:       r.Goal,
		State:      string(r.State),
		TurnBudget: r.TurnBudget,
		CreatedAt:  r.CreatedAt,
		Members:    members,
		Transcript: msgs,
	}
}

func toMemberResponse(m *room.Member) memberResponse {
	return memberResponse{
		ID:       m.ID,
		AgentID:  m.AgentID,
		JoinedAt: m.JoinedAt,
	}
}

func toMessageResponse(m *room.Message) messageResponse {
	return messageResponse{
		ID:         m.ID,
		MemberID:   m.MemberID,
		ToMemberID: m.ToMemberID,
		Body:       m.Body,
		CreatedAt:  m.CreatedAt,
	}
}

// receiptsResponse is the JSON representation of
// GET /v1/rooms/{rom_id}/receipts: the receipts recorded for one room, oldest
// first — the same order receipt.Emitter.ListByRoom returns them in.
type receiptsResponse struct {
	Receipts []receiptResponse `json:"receipts"`
}

// receiptResponse is the JSON representation of a receipt.Receipt.
// TenantID is omitted: the route is tenant-scoped, so it is always the
// caller's own and carries nothing a response needs to repeat. Nothing here
// is a credential, a token, or message body content — the same rule receipt
// emission already follows.
type receiptResponse struct {
	FromMemberID string    `json:"from_member_id"`
	ToMemberID   string    `json:"to_member_id"`
	ToAgentID    string    `json:"to_agent_id"`
	InvocationID string    `json:"invocation_id,omitempty"`
	Outcome      string    `json:"outcome"`
	FailureClass string    `json:"failure_class,omitempty"`
	OccurredAt   time.Time `json:"occurred_at"`
}

// toReceiptsResponse maps a room's receipts, in the order they are given, to
// their JSON representation. It always returns a non-nil slice — even for a
// room with no receipts — so the response encodes as an empty array, never
// null.
func toReceiptsResponse(receipts []receipt.Receipt) receiptsResponse {
	out := make([]receiptResponse, len(receipts))
	for i, r := range receipts {
		out[i] = receiptResponse{
			FromMemberID: r.FromMemberID,
			ToMemberID:   r.ToMemberID,
			ToAgentID:    r.ToAgentID,
			InvocationID: r.InvocationID,
			Outcome:      r.Outcome,
			FailureClass: r.FailureClass,
			OccurredAt:   r.OccurredAt,
		}
	}
	return receiptsResponse{Receipts: out}
}
