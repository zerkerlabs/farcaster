package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/farcaster/rooms/internal/room"
)

func TestHandleAddMember(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(t *testing.T, s *room.Store) string // returns the room ID to target
		body       any
		lookupAs   string
		wantStatus int
		checkBody  func(t *testing.T, body map[string]any)
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mux, store := newMux(t)
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
		})
	}
}
