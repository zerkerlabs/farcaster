package httpapi

import (
	"time"

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

// messageResponse is the JSON representation of a Message.
type messageResponse struct {
	ID        string    `json:"id"`
	MemberID  string    `json:"member_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
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
		ID:        m.ID,
		MemberID:  m.MemberID,
		Body:      m.Body,
		CreatedAt: m.CreatedAt,
	}
}
