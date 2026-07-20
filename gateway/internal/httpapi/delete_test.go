package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/auth/authtest"
)

// deleteRequest builds a DELETE request to /v1/agents/{id} with auth context set.
func deleteRequest(t *testing.T, id, tenant, user string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/v1/agents/"+id, nil)
	return req.WithContext(authtest.WithIdentity(req.Context(), tenant, user))
}

// deleteRequestNoAuth builds a DELETE request with no auth context.
func deleteRequestNoAuth(t *testing.T, id string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodDelete, "/v1/agents/"+id, nil)
}

func TestHandleDelete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		id         string
		reqTenant  string
		reqUser    string
		noAuth     bool
		setup      func(agent.AgentStore) string // returns the created agent ID when needed
		wantStatus int
		// checkStore verifies agent store state after the request.
		checkStore func(t *testing.T, store agent.AgentStore, id string)
	}{
		{
			name:      "204 soft-deletes an active agent",
			reqTenant: testTenant,
			reqUser:   testUser,
			setup: func(s agent.AgentStore) string {
				a := &agent.Agent{Name: "to-delete", CreatedBy: testUser}
				if err := s.Create(context.Background(), testTenant, a); err != nil {
					panic(err)
				}
				return a.ID
			},
			wantStatus: http.StatusNoContent,
			checkStore: func(t *testing.T, s agent.AgentStore, id string) {
				t.Helper()
				// Get still returns the record (preserved for audit) but with StatusInactive.
				got, err := s.Get(context.Background(), testTenant, id)
				if err != nil {
					t.Fatalf("Get after delete: %v", err)
				}
				if got.Status != agent.StatusInactive {
					t.Errorf("Get after delete: status = %q, want %q", got.Status, agent.StatusInactive)
				}
				// List must not include the soft-deleted agent.
				agents, _, err := s.List(context.Background(), testTenant, 1, 50, "")
				if err != nil {
					t.Fatalf("List after delete: %v", err)
				}
				for _, a := range agents {
					if a.ID == id {
						t.Errorf("List after delete: agent %s still present in active list", id)
					}
				}
			},
		},
		{
			name:       "404 for unknown agent ID",
			id:         "agt_doesnotexist",
			reqTenant:  testTenant,
			reqUser:    testUser,
			wantStatus: http.StatusNotFound,
		},
		{
			name:      "404 for agent owned by a different tenant",
			reqTenant: "tenant-2",
			reqUser:   "user-2",
			setup: func(s agent.AgentStore) string {
				a := &agent.Agent{Name: "other-tenant-agent", CreatedBy: testUser}
				if err := s.Create(context.Background(), testTenant, a); err != nil {
					panic(err)
				}
				return a.ID
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "401 when no auth context is present",
			id:         "agt_doesnotexist",
			noAuth:     true,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var store agent.AgentStore
			h := newHandler(t, func(s agent.AgentStore) { store = s })
			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			id := tc.id
			if tc.setup != nil {
				id = tc.setup(store)
			}

			var req *http.Request
			if tc.noAuth {
				req = deleteRequestNoAuth(t, id)
			} else {
				tenant, user := tc.reqTenant, tc.reqUser
				req = deleteRequest(t, id, tenant, user)
			}

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s",
					rec.Code, tc.wantStatus, rec.Body.String())
			}

			if tc.checkStore != nil {
				tc.checkStore(t, store, id)
			}
		})
	}
}
