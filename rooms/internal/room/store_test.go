package room_test

import (
	"context"
	"errors"
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

		m, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA)
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

		_, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantB)
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

		_, err := s.AddMember(context.Background(), tenantB, r.ID, "agt_1", tenantB)
		if !errors.Is(err, room.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
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
	if _, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA); err != nil {
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
	member, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA)
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

	member, err := s.AddMember(context.Background(), tenantA, r1.ID, "agt_1", tenantA)
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

	s := room.NewStore()
	r := mustCreateRoom(t, s, tenantA, "goal")
	member, err := s.AddMember(context.Background(), tenantA, r.ID, "agt_1", tenantA)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	const n = 50
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
}
