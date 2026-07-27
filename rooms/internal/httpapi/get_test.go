package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/farcaster/rooms/internal/room"
)

func TestHandleGetRoom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(t *testing.T, s *room.Store) string // returns the room ID to request
		lookupAs   string
		wantStatus int
		checkBody  func(t *testing.T, body map[string]any)
	}{
		{
			name: "200 returns goal, state, members, and the replayed transcript",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				r := mustCreateRoom(t, s, "ship the thing")
				member := mustAddMember(t, s, r.ID, "agt_1")
				if _, err := s.AppendMessage(context.Background(), tenantA, r.ID, member.ID, "hello"); err != nil {
					t.Fatalf("AppendMessage: %v", err)
				}
				return r.ID
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				if body["goal"] != "ship the thing" {
					t.Errorf("goal = %v, want %q", body["goal"], "ship the thing")
				}
				if body["state"] != "open" {
					t.Errorf("state = %v, want %q", body["state"], "open")
				}
				members, ok := body["members"].([]any)
				if !ok || len(members) != 1 {
					t.Fatalf("members = %v, want a single member", body["members"])
				}
				transcript, ok := body["transcript"].([]any)
				if !ok || len(transcript) != 1 {
					t.Fatalf("transcript = %v, want a single message", body["transcript"])
				}
				msg, _ := transcript[0].(map[string]any)
				if msg["body"] != "hello" {
					t.Errorf("transcript[0].body = %v, want %q", msg["body"], "hello")
				}
			},
		},
		{
			name: "404 for an unknown room ID",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return "rom_does_not_exist"
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "404 for a room owned by a different tenant, identical to a missing room",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				r := mustCreateRoom(t, s, "goal")
				return r.ID
			},
			lookupAs:   tenantB,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "401 when no tenant is in context",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				r := mustCreateRoom(t, s, "goal")
				return r.ID
			},
			lookupAs:   "",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mux, store := newMux(t)
			roomID := tt.setup(t, store)

			req := requestAs(t, http.MethodGet, "/v1/rooms/"+roomID, nil, tt.lookupAs)
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

// TestHandleGetRoom_CrossTenantBodyMatchesMissing verifies that a cross-tenant
// 404 body is byte-identical to a genuinely-missing room's 404 body, so a
// caller cannot distinguish "not yours" from "does not exist" (AGENTS.md
// invariant #2).
func TestHandleGetRoom_CrossTenantBodyMatchesMissing(t *testing.T) {
	t.Parallel()

	mux, store := newMux(t)
	r := mustCreateRoom(t, store, "goal")

	crossTenantReq := requestAs(t, http.MethodGet, "/v1/rooms/"+r.ID, nil, tenantB)
	crossTenantRec := httptest.NewRecorder()
	mux.ServeHTTP(crossTenantRec, crossTenantReq)

	missingReq := requestAs(t, http.MethodGet, "/v1/rooms/rom_does_not_exist", nil, tenantB)
	missingRec := httptest.NewRecorder()
	mux.ServeHTTP(missingRec, missingReq)

	if crossTenantRec.Code != missingRec.Code {
		t.Fatalf("cross-tenant status = %d, missing status = %d, want equal", crossTenantRec.Code, missingRec.Code)
	}
	if crossTenantRec.Body.String() != missingRec.Body.String() {
		t.Fatalf("cross-tenant body = %q, missing body = %q, want equal", crossTenantRec.Body.String(), missingRec.Body.String())
	}
}
