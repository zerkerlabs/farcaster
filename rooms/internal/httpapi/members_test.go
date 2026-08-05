package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/gateway/rooms/internal/memory"
	"github.com/zerkerlabs/gateway/rooms/internal/room"
)

// erroringMemoryStore is a memory.Store whose Read always fails, used to
// exercise onboarding's fail-closed path: a memory backend error must refuse
// the join rather than seat a member with an empty context.
type erroringMemoryStore struct{}

func (erroringMemoryStore) Read(ctx context.Context, scope memory.Scope) ([]memory.Entry, error) {
	return nil, errors.New("memory backend unavailable")
}

func (erroringMemoryStore) Append(ctx context.Context, scope memory.Scope, content string) (memory.Entry, error) {
	return memory.Entry{}, errors.New("memory backend unavailable")
}

func TestHandleAddMember(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(t *testing.T, s *room.Store) string // returns the room ID to target
		memStore   memory.Store                             // nil uses a fresh memory.Fake
		body       any
		lookupAs   string
		wantStatus int
		checkBody  func(t *testing.T, body map[string]any)
		afterCheck func(t *testing.T, s *room.Store, roomID string)
	}{
		{
			name: "201 seats an agent",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return mustCreateRoom(t, s, "goal").ID
			},
			body:       map[string]any{"agent_id": "agt_1"},
			lookupAs:   tenantA,
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				if body["id"] == "" || body["id"] == nil {
					t.Error("id is empty")
				}
				if body["agent_id"] != "agt_1" {
					t.Errorf("agent_id = %v, want %q", body["agent_id"], "agt_1")
				}
				if body["joined_at"] == "" || body["joined_at"] == nil {
					t.Error("joined_at is empty")
				}
			},
		},
		{
			name: "400 missing agent_id",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return mustCreateRoom(t, s, "goal").ID
			},
			body:       map[string]any{},
			lookupAs:   tenantA,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 malformed JSON",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return mustCreateRoom(t, s, "goal").ID
			},
			body:       []byte(`{"agent_id": `),
			lookupAs:   tenantA,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "404 for an unknown room ID",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return "rom_does_not_exist"
			},
			body:       map[string]any{"agent_id": "agt_1"},
			lookupAs:   tenantA,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "404 for a room owned by a different tenant",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return mustCreateRoom(t, s, "goal").ID
			},
			body:       map[string]any{"agent_id": "agt_1"},
			lookupAs:   tenantB,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "409 for a terminated room",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				r := mustCreateRoom(t, s, "goal")
				if _, err := s.CompleteRoom(context.Background(), tenantA, r.ID); err != nil {
					t.Fatalf("CompleteRoom: %v", err)
				}
				return r.ID
			},
			body:       map[string]any{"agent_id": "agt_1"},
			lookupAs:   tenantA,
			wantStatus: http.StatusConflict,
		},
		{
			name: "401 when no tenant is in context",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return mustCreateRoom(t, s, "goal").ID
			},
			body:       map[string]any{"agent_id": "agt_1"},
			lookupAs:   "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "201 onboards a member from memory entries plus documents",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return mustCreateRoom(t, s, "goal").ID
			},
			memStore: func() memory.Store {
				f := memory.NewFake()
				scope := memory.Scope{TenantID: tenantA, AgentID: "agt_1"}
				if _, err := f.Append(context.Background(), scope, "prior fact 1"); err != nil {
					panic(err)
				}
				if _, err := f.Append(context.Background(), scope, "prior fact 2"); err != nil {
					panic(err)
				}
				return f
			}(),
			body:       map[string]any{"agent_id": "agt_1", "documents": []string{"doc A", "doc B"}},
			lookupAs:   tenantA,
			wantStatus: http.StatusCreated,
			afterCheck: func(t *testing.T, s *room.Store, roomID string) {
				t.Helper()
				got, err := s.GetRoom(context.Background(), tenantA, roomID)
				if err != nil {
					t.Fatalf("GetRoom: %v", err)
				}
				if len(got.Members) != 1 {
					t.Fatalf("Members = %v, want a single member", got.Members)
				}
				want := []string{"prior fact 1", "prior fact 2", "doc A", "doc B"}
				got0 := got.Members[0].StartingContext
				if len(got0) != len(want) {
					t.Fatalf("StartingContext = %v, want %v", got0, want)
				}
				for i := range want {
					if got0[i] != want[i] {
						t.Errorf("StartingContext[%d] = %q, want %q", i, got0[i], want[i])
					}
				}
			},
		},
		{
			name: "201 seats a member with zero documents",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return mustCreateRoom(t, s, "goal").ID
			},
			body:       map[string]any{"agent_id": "agt_1"},
			lookupAs:   tenantA,
			wantStatus: http.StatusCreated,
			afterCheck: func(t *testing.T, s *room.Store, roomID string) {
				t.Helper()
				got, err := s.GetRoom(context.Background(), tenantA, roomID)
				if err != nil {
					t.Fatalf("GetRoom: %v", err)
				}
				if len(got.Members) != 1 {
					t.Fatalf("Members = %v, want a single member", got.Members)
				}
				if len(got.Members[0].StartingContext) != 0 {
					t.Errorf("StartingContext = %v, want empty", got.Members[0].StartingContext)
				}
			},
		},
		{
			name: "500 refuses the join when the memory read fails; no member is added",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return mustCreateRoom(t, s, "goal").ID
			},
			memStore:   erroringMemoryStore{},
			body:       map[string]any{"agent_id": "agt_1"},
			lookupAs:   tenantA,
			wantStatus: http.StatusInternalServerError,
			afterCheck: func(t *testing.T, s *room.Store, roomID string) {
				t.Helper()
				got, err := s.GetRoom(context.Background(), tenantA, roomID)
				if err != nil {
					t.Fatalf("GetRoom: %v", err)
				}
				if len(got.Members) != 0 {
					t.Errorf("Members = %v, want empty after a refused join", got.Members)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			memStore := tt.memStore
			if memStore == nil {
				memStore = memory.NewFake()
			}
			mux, store := newMuxWithMemory(t, memStore)
			roomID := tt.setup(t, store)

			req := requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/members", tt.body, tt.lookupAs)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.checkBody != nil {
				tt.checkBody(t, decodeBody(t, rec))
			}
			if tt.afterCheck != nil {
				tt.afterCheck(t, store, roomID)
			}
		})
	}
}
