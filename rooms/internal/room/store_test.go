package room_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zerkerlabs/farcaster/rooms/internal/room"
)

const (
	tenantA = "tenant-alpha"
	tenantB = "tenant-beta"
)

func mustCreateRoom(t *testing.T, s *room.Store, tenantID, goal string) *room.Room {
	t.Helper()
	r, err := s.CreateRoom(context.Background(), tenantID, goal)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	return r
}

// ------------------------------------------------------------- CreateRoom ---

func TestCreateRoom_AssignsServerFields(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	r := mustCreateRoom(t, s, tenantA, "ship the thing")

	if !strings.HasPrefix(r.ID, "rom_") {
		t.Errorf("ID %q does not start with rom_", r.ID)
	}
	if r.TenantID != tenantA {
		t.Errorf("TenantID = %q, want %q", r.TenantID, tenantA)
	}
	if r.Goal != "ship the thing" {
		t.Errorf("Goal = %q, want %q", r.Goal, "ship the thing")
	}
	if r.State != room.StateOpen {
		t.Errorf("State = %q, want %q", r.State, room.StateOpen)
	}
	if r.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if len(r.Members) != 0 {
		t.Errorf("Members = %v, want empty", r.Members)
	}
}

// ---------------------------------------------------------------- GetRoom ---

func TestGetRoom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		lookupAs  string
		wantFound bool
	}{
		{"round-trip in owning tenant", tenantA, true},
		{"cross-tenant read returns not-found", tenantB, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := room.NewStore()
			created := mustCreateRoom(t, s, tenantA, "goal")

			got, err := s.GetRoom(context.Background(), tt.lookupAs, created.ID)
			if tt.wantFound {
				if err != nil {
					t.Fatalf("GetRoom: %v", err)
				}
				if got.ID != created.ID || got.Goal != created.Goal {
					t.Errorf("got %+v, want fields matching %+v", got, created)
				}
				return
			}
			if !errors.Is(err, room.ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
			if got != nil {
				t.Errorf("got = %+v, want nil", got)
			}
		})
	}
}

func TestGetRoom_UnknownID(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	_, err := s.GetRoom(context.Background(), tenantA, "rom_nonexistent")
	if !errors.Is(err, room.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// -------------------------------------------------------------- ListRooms ---

func TestListRooms_CrossTenantIsolation(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	a1 := mustCreateRoom(t, s, tenantA, "a1")
	a2 := mustCreateRoom(t, s, tenantA, "a2")
	mustCreateRoom(t, s, tenantB, "b1")

	got, err := s.ListRooms(context.Background(), tenantA)
	if err != nil {
		t.Fatalf("ListRooms: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}

	ids := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !ids[a1.ID] || !ids[a2.ID] {
		t.Errorf("got = %v, want ids %q and %q", got, a1.ID, a2.ID)
	}
}

func TestListRooms_Empty(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	got, err := s.ListRooms(context.Background(), tenantA)
	if err != nil {
		t.Fatalf("ListRooms: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

// -------------------------------------------------------------- AddMember ---

func TestAddMember(t *testing.T) {
	t.Parallel()

	t.Run("seats a same-tenant agent", func(t *testing.T) {
		t.Parallel()

		s := room.NewStore()
		r := mustCreateRoom(t, s, tenantA, "goal")

		m, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA, nil)
		if err != nil {
			t.Fatalf("AddMember: %v", err)
		}
		if !strings.HasPrefix(m.ID, "mem_") {
			t.Errorf("ID %q does not start with mem_", m.ID)
		}
		if m.AgentID != "agt_1" {
			t.Errorf("AgentID = %q, want %q", m.AgentID, "agt_1")
		}
		if m.JoinedAt.IsZero() {
			t.Error("JoinedAt is zero")
		}

		got, err := s.GetRoom(context.Background(), tenantA, r.ID)
		if err != nil {
			t.Fatalf("GetRoom: %v", err)
		}
		if len(got.Members) != 1 || got.Members[0].ID != m.ID {
			t.Errorf("Members = %v, want [%s]", got.Members, m.ID)
		}
	})

	t.Run("cross-tenant member add is rejected", func(t *testing.T) {
		t.Parallel()

		s := room.NewStore()
		r := mustCreateRoom(t, s, tenantA, "goal")

		_, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantB, nil)
		if !errors.Is(err, room.ErrTenantMismatch) {
			t.Errorf("err = %v, want ErrTenantMismatch", err)
		}

		got, err := s.GetRoom(context.Background(), tenantA, r.ID)
		if err != nil {
			t.Fatalf("GetRoom: %v", err)
		}
		if len(got.Members) != 0 {
			t.Errorf("Members = %v, want empty after rejected add", got.Members)
		}
	})

	t.Run("room in another tenant is not found", func(t *testing.T) {
		t.Parallel()

		s := room.NewStore()
		r := mustCreateRoom(t, s, tenantA, "goal")

		_, err := s.AddMember(context.Background(), tenantB, r.ID, "agt_1", tenantB, nil)
		if !errors.Is(err, room.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("carries the caller-supplied starting context", func(t *testing.T) {
		t.Parallel()

		s := room.NewStore()
		r := mustCreateRoom(t, s, tenantA, "goal")
		startingContext := []string{"memory entry", "onboarding doc"}

		m, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA, startingContext)
		if err != nil {
			t.Fatalf("AddMember: %v", err)
		}
		if !slices.Equal(m.StartingContext, startingContext) {
			t.Errorf("StartingContext = %v, want %v", m.StartingContext, startingContext)
		}

		// Mutating the caller's slice after the call must not leak into the
		// store (same isolation guarantee as TestReturnedValuesAreIsolated).
		startingContext[0] = "mutated"
		got, err := s.GetRoom(context.Background(), tenantA, r.ID)
		if err != nil {
			t.Fatalf("GetRoom: %v", err)
		}
		if got.Members[0].StartingContext[0] != "memory entry" {
			t.Errorf("stored StartingContext[0] = %q, want %q (caller mutation leaked into store)", got.Members[0].StartingContext[0], "memory entry")
		}
	})
}

// -------------------------------------------------------------- clonation ---

// TestReturnedValuesAreIsolated verifies that mutating a returned *Room does
// not corrupt the stored record.
func TestReturnedValuesAreIsolated(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	r := mustCreateRoom(t, s, tenantA, "goal")
	if _, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	got, err := s.GetRoom(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("GetRoom: %v", err)
	}
	got.Members[0].AgentID = "mutated"

	again, err := s.GetRoom(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("GetRoom again: %v", err)
	}
	if again.Members[0].AgentID != "agt_1" {
		t.Errorf("stored AgentID = %q, want %q (caller mutation leaked into store)", again.Members[0].AgentID, "agt_1")
	}
}

// ----------------------------------------------------------- AppendMessage ---

func TestAppendMessage_RoundTrip(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	r := mustCreateRoom(t, s, tenantA, "goal")
	member, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA, nil)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	msg, err := s.AppendMessage(context.Background(), tenantA, r.ID, member.ID, "hello")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if !strings.HasPrefix(msg.ID, "msg_") {
		t.Errorf("ID %q does not start with msg_", msg.ID)
	}
	if msg.MemberID != member.ID {
		t.Errorf("MemberID = %q, want %q", msg.MemberID, member.ID)
	}
	if msg.Body != "hello" {
		t.Errorf("Body = %q, want %q", msg.Body, "hello")
	}

	msgs, err := s.Messages(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != msg.ID {
		t.Errorf("Messages = %v, want [%s]", msgs, msg.ID)
	}
}

func TestAppendMessage_CrossTenantBlocked(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	r := mustCreateRoom(t, s, tenantA, "goal")

	_, err := s.AppendMessage(context.Background(), tenantB, r.ID, "mem_1", "hello")
	if !errors.Is(err, room.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A message must be attributable to a member actually seated in the room —
// otherwise a caller could persist one pointing at a member that never
// existed, or one belonging to a different room.
func TestAppendMessage_UnknownMemberRejected(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	r := mustCreateRoom(t, s, tenantA, "goal")

	if _, err := s.AppendMessage(context.Background(), tenantA, r.ID, "mem_nope", "hello"); !errors.Is(err, room.ErrMemberNotFound) {
		t.Errorf("err = %v, want ErrMemberNotFound", err)
	}
}

// A member seated in one room may not author messages in another, even when
// both rooms belong to the same tenant.
func TestAppendMessage_MemberFromAnotherRoomRejected(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	r1 := mustCreateRoom(t, s, tenantA, "first")
	r2 := mustCreateRoom(t, s, tenantA, "second")

	member, err := s.AddMember(context.Background(), tenantA, r1.ID, "agt_1", tenantA, nil)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	if _, err := s.AppendMessage(context.Background(), tenantA, r2.ID, member.ID, "hello"); !errors.Is(err, room.ErrMemberNotFound) {
		t.Errorf("err = %v, want ErrMemberNotFound", err)
	}
}

// TestAppendMessage_ConcurrentDoNotRace exercises the store's append path from
// many goroutines at once; run with -race (make check's test target does).
func TestAppendMessage_ConcurrentDoNotRace(t *testing.T) {
	t.Parallel()

	const n = 50
	s := room.NewStore()
	r, err := s.CreateRoomWithBudget(context.Background(), tenantA, "goal", n)
	if err != nil {
		t.Fatalf("CreateRoomWithBudget: %v", err)
	}
	member, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA, nil)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			if _, err := s.AppendMessage(context.Background(), tenantA, r.ID, member.ID, "msg"); err != nil {
				t.Errorf("AppendMessage %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	msgs, err := s.Messages(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != n {
		t.Errorf("len(msgs) = %d, want %d", len(msgs), n)
	}

	ids := make(map[string]bool, n)
	for _, m := range msgs {
		if ids[m.ID] {
			t.Fatalf("duplicate message ID: %q", m.ID)
		}
		ids[m.ID] = true
	}

	// Sequence numbers assigned under concurrent append must still form a
	// valid contiguous run starting at 1 — the store's single write lock
	// serializes appends even though the callers race.
	events, err := s.Events(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	seqs := make(map[int]bool, len(events))
	for _, ev := range events {
		if seqs[ev.Sequence] {
			t.Fatalf("duplicate sequence number: %d", ev.Sequence)
		}
		seqs[ev.Sequence] = true
	}
	// 1 member-joined event + n message-posted events.
	want := n + 1
	if len(events) != want {
		t.Fatalf("len(events) = %d, want %d", len(events), want)
	}
	for i := 1; i <= want; i++ {
		if !seqs[i] {
			t.Errorf("missing sequence number %d", i)
		}
	}
}

// --------------------------------------------------------------- Events ---

// TestEvents_SequenceNumbering verifies that every kind of append —
// membership, messages, and terminal transitions — shares one contiguous,
// per-room sequence starting at 1.
func TestEvents_SequenceNumbering(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	r := mustCreateRoom(t, s, tenantA, "goal")
	member, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA, nil)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if _, err := s.AppendMessage(context.Background(), tenantA, r.ID, member.ID, "hello"); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := s.AppendMessage(context.Background(), tenantA, r.ID, member.ID, "world"); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := s.CompleteRoom(context.Background(), tenantA, r.ID); err != nil {
		t.Fatalf("CompleteRoom: %v", err)
	}

	events, err := s.Events(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	wantKinds := []room.EventKind{
		room.EventMemberJoined,
		room.EventMessagePosted,
		room.EventMessagePosted,
		room.EventRoomTerminated,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("len(events) = %d, want %d", len(events), len(wantKinds))
	}
	for i, ev := range events {
		wantSeq := i + 1
		if ev.Sequence != wantSeq {
			t.Errorf("events[%d].Sequence = %d, want %d", i, ev.Sequence, wantSeq)
		}
		if ev.Kind != wantKinds[i] {
			t.Errorf("events[%d].Kind = %q, want %q", i, ev.Kind, wantKinds[i])
		}
		if ev.Timestamp.IsZero() {
			t.Errorf("events[%d].Timestamp is zero", i)
		}
	}
}

// ------------------------------------------------------------ transcript ---

// TestMessages_ReplaysTranscriptFromEvents verifies that a room's transcript
// is derived by replaying its event log, in order, skipping non-message
// events such as the member join and the terminal transition.
func TestMessages_ReplaysTranscriptFromEvents(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	r := mustCreateRoom(t, s, tenantA, "goal")
	member, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA, nil)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	var want []string
	for _, body := range []string{"first", "second", "third"} {
		msg, err := s.AppendMessage(context.Background(), tenantA, r.ID, member.ID, body)
		if err != nil {
			t.Fatalf("AppendMessage(%q): %v", body, err)
		}
		want = append(want, msg.ID)
	}
	if _, err := s.CompleteRoom(context.Background(), tenantA, r.ID); err != nil {
		t.Fatalf("CompleteRoom: %v", err)
	}

	msgs, err := s.Messages(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != len(want) {
		t.Fatalf("len(msgs) = %d, want %d", len(msgs), len(want))
	}
	for i, m := range msgs {
		if m.ID != want[i] {
			t.Errorf("msgs[%d].ID = %q, want %q", i, m.ID, want[i])
		}
		if m.Body != []string{"first", "second", "third"}[i] {
			t.Errorf("msgs[%d].Body = %q, want %q", i, m.Body, []string{"first", "second", "third"}[i])
		}
	}
}

// ------------------------------------------------------------ turn budget ---

// TestAppendMessage_TurnBudgetExhaustionAbandonsRoom verifies that exceeding
// a room's turn budget rejects the over-budget message and abandons the room,
// recording the transition as an event.
func TestAppendMessage_TurnBudgetExhaustionAbandonsRoom(t *testing.T) {
	t.Parallel()

	const budget = 2
	s := room.NewStore()
	r, err := s.CreateRoomWithBudget(context.Background(), tenantA, "goal", budget)
	if err != nil {
		t.Fatalf("CreateRoomWithBudget: %v", err)
	}
	member, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA, nil)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	for i := range budget {
		if _, err := s.AppendMessage(context.Background(), tenantA, r.ID, member.ID, "msg"); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
	}

	got, err := s.GetRoom(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("GetRoom: %v", err)
	}
	if got.State != room.StateOpen {
		t.Fatalf("State = %q, want %q before exceeding budget", got.State, room.StateOpen)
	}

	_, err = s.AppendMessage(context.Background(), tenantA, r.ID, member.ID, "one too many")
	if !errors.Is(err, room.ErrTurnBudgetExceeded) {
		t.Fatalf("err = %v, want ErrTurnBudgetExceeded", err)
	}

	got, err = s.GetRoom(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("GetRoom: %v", err)
	}
	if got.State != room.StateAbandoned {
		t.Fatalf("State = %q, want %q after exceeding budget", got.State, room.StateAbandoned)
	}

	msgs, err := s.Messages(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != budget {
		t.Fatalf("len(msgs) = %d, want %d (over-budget message must not be recorded)", len(msgs), budget)
	}

	events, err := s.Events(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	last := events[len(events)-1]
	if last.Kind != room.EventRoomTerminated {
		t.Fatalf("last event kind = %q, want %q", last.Kind, room.EventRoomTerminated)
	}
	payload, ok := last.Payload.(room.RoomTerminatedPayload)
	if !ok {
		t.Fatalf("last event payload = %T, want room.RoomTerminatedPayload", last.Payload)
	}
	if payload.State != room.StateAbandoned {
		t.Errorf("terminated payload state = %q, want %q", payload.State, room.StateAbandoned)
	}
}

func TestCreateRoomWithBudget_RejectsNonPositive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		budget int
	}{
		{"zero", 0},
		{"negative", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := room.NewStore()
			_, err := s.CreateRoomWithBudget(context.Background(), tenantA, "goal", tt.budget)
			if err == nil {
				t.Fatal("err = nil, want error for non-positive turn budget")
			}
		})
	}
}

// -------------------------------------------------------------- terminal ---

func TestCompleteRoom(t *testing.T) {
	t.Parallel()

	s := room.NewStore()
	r := mustCreateRoom(t, s, tenantA, "goal")

	got, err := s.CompleteRoom(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("CompleteRoom: %v", err)
	}
	if got.State != room.StateCompleted {
		t.Errorf("State = %q, want %q", got.State, room.StateCompleted)
	}

	events, err := s.Events(context.Background(), tenantA, r.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].Kind != room.EventRoomTerminated {
		t.Fatalf("events = %+v, want a single EventRoomTerminated", events)
	}
}

// TestTerminatedRoomRejectsAppends is table-driven over both terminal states
// and every append path: appending anything to a terminated room is an
// error, regardless of how it became terminal.
func TestTerminatedRoomRejectsAppends(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		terminate func(t *testing.T, s *room.Store, roomID string)
	}{
		{
			name: "completed",
			terminate: func(t *testing.T, s *room.Store, roomID string) {
				t.Helper()
				if _, err := s.CompleteRoom(context.Background(), tenantA, roomID); err != nil {
					t.Fatalf("CompleteRoom: %v", err)
				}
			},
		},
		{
			name: "abandoned via turn budget exhaustion",
			terminate: func(t *testing.T, s *room.Store, roomID string) {
				t.Helper()
				member, err := s.AddMember(context.Background(), tenantA, roomID, "agt_budget_burner", tenantA, nil)
				if err != nil {
					t.Fatalf("AddMember: %v", err)
				}
				// Budget is 1: the first message spends the only turn, the
				// second exceeds it and abandons the room.
				if _, err := s.AppendMessage(context.Background(), tenantA, roomID, member.ID, "spend the turn"); err != nil {
					t.Fatalf("AppendMessage (spend the turn): %v", err)
				}
				if _, err := s.AppendMessage(context.Background(), tenantA, roomID, member.ID, "burn the budget"); !errors.Is(err, room.ErrTurnBudgetExceeded) {
					t.Fatalf("AppendMessage (burn the budget): err = %v, want ErrTurnBudgetExceeded", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := room.NewStore()
			r, err := s.CreateRoomWithBudget(context.Background(), tenantA, "goal", 1)
			if err != nil {
				t.Fatalf("CreateRoomWithBudget: %v", err)
			}
			tt.terminate(t, s, r.ID)

			member, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_late", tenantA, nil)
			if err == nil {
				t.Errorf("AddMember on terminated room: got member %+v, want ErrRoomTerminated", member)
			} else if !errors.Is(err, room.ErrRoomTerminated) {
				t.Errorf("AddMember err = %v, want ErrRoomTerminated", err)
			}

			_, err = s.AppendMessage(context.Background(), tenantA, r.ID, "mem_whoever", "too late")
			if !errors.Is(err, room.ErrRoomTerminated) {
				t.Errorf("AppendMessage err = %v, want ErrRoomTerminated", err)
			}

			_, err = s.CompleteRoom(context.Background(), tenantA, r.ID)
			if !errors.Is(err, room.ErrRoomTerminated) {
				t.Errorf("CompleteRoom err = %v, want ErrRoomTerminated", err)
			}
		})
	}
}
