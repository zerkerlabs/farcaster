package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/receipt"
	"github.com/zerkerlabs/farcaster/rooms/internal/room"
)

// mustEmit emits one receipt directly through em for roomID under tenantID,
// bypassing the message-delivery path — these tests only care what
// GET .../receipts returns for a given set of receipts, not how they came to
// be emitted.
func mustEmit(t *testing.T, em *receipt.Fake, roomID, tenantID, outcome, failureClass string) {
	t.Helper()
	if err := em.Emit(context.Background(), receipt.Receipt{
		RoomID:       roomID,
		TenantID:     tenantID,
		FromMemberID: "mem_from",
		ToMemberID:   "mem_to",
		ToAgentID:    "agt_to",
		Outcome:      outcome,
		FailureClass: failureClass,
		OccurredAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
}

func TestHandleGetReceipts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(t *testing.T, s *room.Store, em *receipt.Fake) string // returns the room ID to request
		lookupAs   string
		wantStatus int
		checkBody  func(t *testing.T, body map[string]any)
	}{
		{
			name: "200 returns the room's receipts in emission order",
			setup: func(t *testing.T, s *room.Store, em *receipt.Fake) string {
				t.Helper()
				r := mustCreateRoom(t, s, "ship the thing")
				other := mustCreateRoom(t, s, "a different room")
				mustEmit(t, em, r.ID, tenantA, receipt.OutcomeSucceeded, "")
				mustEmit(t, em, r.ID, tenantA, receipt.OutcomeFailed, "upstream_failure")
				mustEmit(t, em, other.ID, tenantA, receipt.OutcomeSucceeded, "")
				return r.ID
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				receipts, ok := body["receipts"].([]any)
				if !ok || len(receipts) != 2 {
					t.Fatalf("receipts = %v, want exactly 2 (another room's receipt must not appear)", body["receipts"])
				}
				first, _ := receipts[0].(map[string]any)
				second, _ := receipts[1].(map[string]any)
				if first["outcome"] != "succeeded" {
					t.Errorf("receipts[0].outcome = %v, want %q", first["outcome"], "succeeded")
				}
				if second["outcome"] != "failed" {
					t.Errorf("receipts[1].outcome = %v, want %q", second["outcome"], "failed")
				}
				if second["failure_class"] != "upstream_failure" {
					t.Errorf("receipts[1].failure_class = %v, want %q", second["failure_class"], "upstream_failure")
				}
			},
		},
		{
			name: "200 excludes a receipt emitted under another tenant for the same room ID",
			setup: func(t *testing.T, s *room.Store, em *receipt.Fake) string {
				t.Helper()
				r := mustCreateRoom(t, s, "goal")
				mustEmit(t, em, r.ID, tenantA, receipt.OutcomeSucceeded, "")
				// No caller ever legitimately emits a receipt for tenantA's room
				// under tenantB — this stands in for the seam itself never
				// leaking another tenant's receipt across the same room ID,
				// belt-and-braces alongside the room-existence check above.
				mustEmit(t, em, r.ID, tenantB, receipt.OutcomeSucceeded, "")
				return r.ID
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				receipts, ok := body["receipts"].([]any)
				if !ok || len(receipts) != 1 {
					t.Fatalf("receipts = %v, want exactly 1 (tenant-b's receipt must not leak)", body["receipts"])
				}
			},
		},
		{
			name: "200 with an empty collection for a room with no receipts",
			setup: func(t *testing.T, s *room.Store, em *receipt.Fake) string {
				t.Helper()
				r := mustCreateRoom(t, s, "goal")
				return r.ID
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				receipts, ok := body["receipts"].([]any)
				if !ok || len(receipts) != 0 {
					t.Errorf("receipts = %v, want an empty collection, not absent or null", body["receipts"])
				}
			},
		},
		{
			name: "404 for an unknown room ID",
			setup: func(t *testing.T, s *room.Store, em *receipt.Fake) string {
				t.Helper()
				return "rom_does_not_exist"
			},
			lookupAs:   tenantA,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "404 for a room owned by a different tenant, identical to a missing room",
			setup: func(t *testing.T, s *room.Store, em *receipt.Fake) string {
				t.Helper()
				r := mustCreateRoom(t, s, "goal")
				mustEmit(t, em, r.ID, tenantA, receipt.OutcomeSucceeded, "")
				return r.ID
			},
			lookupAs:   tenantB,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "401 when no tenant is in context",
			setup: func(t *testing.T, s *room.Store, em *receipt.Fake) string {
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

			em := receipt.NewFake()
			mux, store, _ := newMuxWithReceipts(t, nil, unreachableGateway{t}, em)
			roomID := tt.setup(t, store, em)

			req := requestAs(t, http.MethodGet, "/v1/rooms/"+roomID+"/receipts", nil, tt.lookupAs)
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

// TestHandleGetReceipts_CrossTenantBodyMatchesMissing verifies that a
// cross-tenant 404 body is byte-identical to a genuinely-missing room's 404
// body, so a caller cannot distinguish "not yours" from "does not exist"
// (AGENTS.md invariant #2) — and that a receipt belonging to another
// tenant's room is never readable in the process.
func TestHandleGetReceipts_CrossTenantBodyMatchesMissing(t *testing.T) {
	t.Parallel()

	em := receipt.NewFake()
	mux, store, _ := newMuxWithReceipts(t, nil, unreachableGateway{t}, em)
	r := mustCreateRoom(t, store, "goal")
	mustEmit(t, em, r.ID, tenantA, receipt.OutcomeSucceeded, "")

	crossTenantReq := requestAs(t, http.MethodGet, "/v1/rooms/"+r.ID+"/receipts", nil, tenantB)
	crossTenantRec := httptest.NewRecorder()
	mux.ServeHTTP(crossTenantRec, crossTenantReq)

	missingReq := requestAs(t, http.MethodGet, "/v1/rooms/rom_does_not_exist/receipts", nil, tenantB)
	missingRec := httptest.NewRecorder()
	mux.ServeHTTP(missingRec, missingReq)

	if crossTenantRec.Code != missingRec.Code {
		t.Fatalf("cross-tenant status = %d, missing status = %d, want equal", crossTenantRec.Code, missingRec.Code)
	}
	if crossTenantRec.Body.String() != missingRec.Body.String() {
		t.Fatalf("cross-tenant body = %q, missing body = %q, want equal", crossTenantRec.Body.String(), missingRec.Body.String())
	}
}
