package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/farcaster/rooms/internal/room"
)

func TestHandleCompleteRoom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(t *testing.T, s *room.Store) string // returns the room ID to complete
		lookupAs   string
		wantStatus int
	}{
		{
			name: "200 completes an open room",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return mustCreateRoom(t, s, "goal").ID
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusOK,
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
				return mustCreateRoom(t, s, "goal").ID
			},
			lookupAs:   tenantB,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "409 for a room already completed",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				r := mustCreateRoom(t, s, "goal")
				if _, err := s.CompleteRoom(context.Background(), tenantA, r.ID); err != nil {
					t.Fatalf("CompleteRoom: %v", err)
				}
				return r.ID
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusConflict,
		},
		{
			name: "409 for a room already abandoned",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				r, err := s.CreateRoomWithBudget(context.Background(), tenantA, "goal", 1)
				if err != nil {
					t.Fatalf("CreateRoomWithBudget: %v", err)
				}
				member := mustAddMember(t, s, r.ID, "agt_1")
				if _, err := s.AppendMessage(context.Background(), tenantA, r.ID, member.ID, "spend the only turn"); err != nil {
					t.Fatalf("AppendMessage: %v", err)
				}
				// The budget is now spent; one more post exceeds it and abandons
				// the room rather than letting it stay open.
				if _, err := s.AppendMessage(context.Background(), tenantA, r.ID, member.ID, "over budget"); err == nil {
					t.Fatal("AppendMessage: want ErrTurnBudgetExceeded, got nil")
				}
				return r.ID
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusConflict,
		},
		{
			name: "401 when no tenant is in context",
			setup: func(t *testing.T, s *room.Store) string {
				t.Helper()
				return mustCreateRoom(t, s, "goal").ID
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

			req := requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/complete", nil, tt.lookupAs)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestHandleCompleteRoom_ReadBackReportsCompleted verifies the room reports
// StateCompleted when read back over the API after being completed over the
// API — the whole point of the route: the success path of the room lifecycle
// becomes observable, not just reachable from the store's own tests.
func TestHandleCompleteRoom_ReadBackReportsCompleted(t *testing.T) {
	t.Parallel()

	mux, store := newMux(t)
	r := mustCreateRoom(t, store, "goal")

	completeReq := requestAs(t, http.MethodPost, "/v1/rooms/"+r.ID+"/complete", nil, tenantA)
	completeRec := httptest.NewRecorder()
	mux.ServeHTTP(completeRec, completeReq)
	if completeRec.Code != http.StatusOK {
		t.Fatalf("complete status = %d, want %d; body = %s", completeRec.Code, http.StatusOK, completeRec.Body.String())
	}
	if body := decodeBody(t, completeRec); body["state"] != "completed" {
		t.Errorf("complete response state = %v, want %q", body["state"], "completed")
	}

	getReq := requestAs(t, http.MethodGet, "/v1/rooms/"+r.ID, nil, tenantA)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d; body = %s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	if body := decodeBody(t, getRec); body["state"] != "completed" {
		t.Errorf("get response state = %v, want %q", body["state"], "completed")
	}
}

// TestHandleCompleteRoom_CrossTenantBodyMatchesMissing verifies that a
// cross-tenant 404 body is byte-identical to a genuinely-missing room's 404
// body, so a caller cannot distinguish "not yours" from "does not exist"
// (AGENTS.md invariant #2).
func TestHandleCompleteRoom_CrossTenantBodyMatchesMissing(t *testing.T) {
	t.Parallel()

	mux, store := newMux(t)
	r := mustCreateRoom(t, store, "goal")

	crossTenantReq := requestAs(t, http.MethodPost, "/v1/rooms/"+r.ID+"/complete", nil, tenantB)
	crossTenantRec := httptest.NewRecorder()
	mux.ServeHTTP(crossTenantRec, crossTenantReq)

	missingReq := requestAs(t, http.MethodPost, "/v1/rooms/rom_does_not_exist/complete", nil, tenantB)
	missingRec := httptest.NewRecorder()
	mux.ServeHTTP(missingRec, missingReq)

	if crossTenantRec.Code != missingRec.Code {
		t.Fatalf("cross-tenant status = %d, missing status = %d, want equal", crossTenantRec.Code, missingRec.Code)
	}
	if crossTenantRec.Body.String() != missingRec.Body.String() {
		t.Fatalf("cross-tenant body = %q, missing body = %q, want equal", crossTenantRec.Body.String(), missingRec.Body.String())
	}
}

// TestHandleCompleteRoom_AlreadyTerminalDistinctFromMissing verifies that,
// unlike the cross-tenant case, a caller that owns the room learns it was
// already terminal (409) rather than being told it doesn't exist (404) — the
// two must be distinguishable to the owning tenant even though they are
// deliberately indistinguishable to a different one.
func TestHandleCompleteRoom_AlreadyTerminalDistinctFromMissing(t *testing.T) {
	t.Parallel()

	mux, store := newMux(t)
	r := mustCreateRoom(t, store, "goal")
	if _, err := store.CompleteRoom(context.Background(), tenantA, r.ID); err != nil {
		t.Fatalf("CompleteRoom: %v", err)
	}

	alreadyTerminalReq := requestAs(t, http.MethodPost, "/v1/rooms/"+r.ID+"/complete", nil, tenantA)
	alreadyTerminalRec := httptest.NewRecorder()
	mux.ServeHTTP(alreadyTerminalRec, alreadyTerminalReq)
	if alreadyTerminalRec.Code != http.StatusConflict {
		t.Fatalf("already-terminal status = %d, want %d", alreadyTerminalRec.Code, http.StatusConflict)
	}

	missingReq := requestAs(t, http.MethodPost, "/v1/rooms/rom_does_not_exist/complete", nil, tenantA)
	missingRec := httptest.NewRecorder()
	mux.ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want %d", missingRec.Code, http.StatusNotFound)
	}
}
