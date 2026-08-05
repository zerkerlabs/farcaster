package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/gateway/rooms/internal/room"
)

// roomAndMember creates a room under tenantA with the given turn budget (0
// means the store default) and seats one member in it.
func roomAndMember(t *testing.T, s *room.Store, turnBudget int) (roomID, memberID string) {
	t.Helper()
	var r *room.Room
	var err error
	if turnBudget > 0 {
		r, err = s.CreateRoomWithBudget(context.Background(), tenantA, "goal", turnBudget)
	} else {
		r, err = s.CreateRoom(context.Background(), tenantA, "goal")
	}
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	m := mustAddMember(t, s, r.ID, "agt_1")
	return r.ID, m.ID
}

func TestHandlePostMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(t *testing.T, s *room.Store) (roomID string, body any)
		lookupAs   string
		wantStatus int
		checkBody  func(t *testing.T, body map[string]any)
	}{
		{
			name: "201 appends a message and consumes a turn",
			setup: func(t *testing.T, s *room.Store) (string, any) {
				t.Helper()
				roomID, memberID := roomAndMember(t, s, 0)
				return roomID, map[string]any{"member_id": memberID, "body": "hello"}
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				if body["id"] == "" || body["id"] == nil {
					t.Error("id is empty")
				}
				if body["body"] != "hello" {
					t.Errorf("body = %v, want %q", body["body"], "hello")
				}
			},
		},
		{
			name: "400 missing member_id",
			setup: func(t *testing.T, s *room.Store) (string, any) {
				t.Helper()
				roomID, _ := roomAndMember(t, s, 0)
				return roomID, map[string]any{"body": "hello"}
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 missing body",
			setup: func(t *testing.T, s *room.Store) (string, any) {
				t.Helper()
				roomID, memberID := roomAndMember(t, s, 0)
				return roomID, map[string]any{"member_id": memberID}
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 malformed JSON",
			setup: func(t *testing.T, s *room.Store) (string, any) {
				t.Helper()
				roomID, _ := roomAndMember(t, s, 0)
				return roomID, []byte(`{"body": `)
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 member not seated in the room",
			setup: func(t *testing.T, s *room.Store) (string, any) {
				t.Helper()
				roomID, _ := roomAndMember(t, s, 0)
				return roomID, map[string]any{"member_id": "mem_nope", "body": "hello"}
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "404 for an unknown room ID",
			setup: func(t *testing.T, s *room.Store) (string, any) {
				t.Helper()
				return "rom_does_not_exist", map[string]any{"member_id": "mem_1", "body": "hello"}
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "404 for a room owned by a different tenant",
			setup: func(t *testing.T, s *room.Store) (string, any) {
				t.Helper()
				roomID, memberID := roomAndMember(t, s, 0)
				return roomID, map[string]any{"member_id": memberID, "body": "hello"}
			},
			lookupAs:   tenantB,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "409 posting to a terminated room",
			setup: func(t *testing.T, s *room.Store) (string, any) {
				t.Helper()
				roomID, memberID := roomAndMember(t, s, 0)
				if _, err := s.CompleteRoom(context.Background(), tenantA, roomID); err != nil {
					t.Fatalf("CompleteRoom: %v", err)
				}
				return roomID, map[string]any{"member_id": memberID, "body": "too late"}
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusConflict,
		},
		{
			name: "409 exceeding the turn budget",
			setup: func(t *testing.T, s *room.Store) (string, any) {
				t.Helper()
				roomID, memberID := roomAndMember(t, s, 1)
				if _, err := s.AppendMessage(context.Background(), tenantA, roomID, memberID, "spends the only turn"); err != nil {
					t.Fatalf("AppendMessage: %v", err)
				}
				return roomID, map[string]any{"member_id": memberID, "body": "one too many"}
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusConflict,
		},
		{
			name: "401 when no tenant is in context",
			setup: func(t *testing.T, s *room.Store) (string, any) {
				t.Helper()
				roomID, memberID := roomAndMember(t, s, 0)
				return roomID, map[string]any{"member_id": memberID, "body": "hello"}
			},
			lookupAs:   "",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mux, store := newMux(t)
			roomID, body := tt.setup(t, store)

			req := requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages", body, tt.lookupAs)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.checkBody != nil {
				tt.checkBody(t, decodeBody(t, rec))
			}
		})
	}
}

// TestHandlePostMessage_BudgetExhaustionAbandonsRoom verifies the full
// round-trip: exceeding the turn budget over HTTP returns 409, and a
// subsequent GET on the room reads its state back as abandoned.
func TestHandlePostMessage_BudgetExhaustionAbandonsRoom(t *testing.T) {
	t.Parallel()

	mux, store := newMux(t)
	roomID, memberID := roomAndMember(t, store, 1)

	// Spend the only turn.
	spend := requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "body": "spend it"}, tenantA)
	spendRec := httptest.NewRecorder()
	mux.ServeHTTP(spendRec, spend)
	if spendRec.Code != http.StatusCreated {
		t.Fatalf("spend turn: status = %d, want %d; body = %s", spendRec.Code, http.StatusCreated, spendRec.Body.String())
	}

	// Exceed the budget.
	over := requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "body": "one too many"}, tenantA)
	overRec := httptest.NewRecorder()
	mux.ServeHTTP(overRec, over)
	if overRec.Code != http.StatusConflict {
		t.Fatalf("exceed budget: status = %d, want %d; body = %s", overRec.Code, http.StatusConflict, overRec.Body.String())
	}

	get := requestAs(t, http.MethodGet, "/v1/rooms/"+roomID, nil, tenantA)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get room: status = %d, want %d; body = %s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	body := decodeBody(t, getRec)
	if body["state"] != "abandoned" {
		t.Errorf("state = %v, want %q", body["state"], "abandoned")
	}
}
