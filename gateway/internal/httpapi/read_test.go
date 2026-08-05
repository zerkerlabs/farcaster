package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/gateway/gateway/internal/httpapi"
)

// authedGetRequest returns a GET request with the auth context pre-populated.
func authedGetRequest(t *testing.T, path, tenant, user string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	return req.WithContext(authtest.WithIdentity(req.Context(), tenant, user))
}

// unauthGetRequest returns a GET request with no auth context.
func unauthGetRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, path, nil)
}

// newHandlerForRead creates a store, runs setup on it, then returns both the
// handler and the store so tests can extract server-assigned IDs after seeding.
func newHandlerForRead(t *testing.T) (*httpapi.Handler, agent.AgentStore) {
	t.Helper()
	store := agent.NewMemoryStore()
	return httpapi.NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))), store
}

func TestHandleList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		setup      func(agent.AgentStore)
		noAuth     bool
		reqTenant  string
		reqUser    string
		wantStatus int
		checkBody  func(t *testing.T, body []byte)
	}{
		{
			name:       "200 empty list when tenant has no agents",
			path:       "/v1/agents",
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp struct {
					Agents  []any `json:"agents"`
					Page    int   `json:"page"`
					PerPage int   `json:"per_page"`
					Total   int   `json:"total"`
				}
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if len(resp.Agents) != 0 {
					t.Errorf("agents length = %d, want 0", len(resp.Agents))
				}
				if resp.Total != 0 {
					t.Errorf("total = %d, want 0", resp.Total)
				}
				if resp.Page != 1 {
					t.Errorf("page = %d, want 1 (default)", resp.Page)
				}
				if resp.PerPage != 50 {
					t.Errorf("per_page = %d, want 50 (default)", resp.PerPage)
				}
			},
		},
		{
			name: "200 returns caller's non-deleted agents (active and pending) with total",
			path: "/v1/agents",
			setup: func(s agent.AgentStore) {
				for _, name := range []string{"alpha", "beta"} {
					if err := s.Create(context.Background(), testTenant, &agent.Agent{
						Name:      name,
						CreatedBy: testUser,
					}); err != nil {
						panic(err)
					}
				}
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp struct {
					Agents []map[string]any `json:"agents"`
					Total  int              `json:"total"`
				}
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp.Total != 2 {
					t.Errorf("total = %d, want 2", resp.Total)
				}
				if len(resp.Agents) != 2 {
					t.Errorf("agents length = %d, want 2", len(resp.Agents))
				}
				// Each agent record must carry the required fields.
				// Seeded without upstream_url → status is pending.
				for _, a := range resp.Agents {
					if a["id"] == "" {
						t.Error("agent missing id")
					}
					if a["status"] != "pending" {
						t.Errorf("agent status = %q, want pending", a["status"])
					}
				}
			},
		},
		{
			name: "200 excludes other tenant's agents (cross-tenant isolation)",
			path: "/v1/agents",
			setup: func(s agent.AgentStore) {
				if err := s.Create(context.Background(), "tenant-other", &agent.Agent{
					Name:      "foreign-agent",
					CreatedBy: "user-other",
				}); err != nil {
					panic(err)
				}
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp struct {
					Agents []any `json:"agents"`
					Total  int   `json:"total"`
				}
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp.Total != 0 {
					t.Errorf("total = %d, want 0 (cross-tenant leak)", resp.Total)
				}
				if len(resp.Agents) != 0 {
					t.Errorf("agents length = %d, want 0 (cross-tenant leak)", len(resp.Agents))
				}
			},
		},
		{
			name: "pagination: per_page limits result count, total reflects full set",
			path: "/v1/agents?page=1&per_page=2",
			setup: func(s agent.AgentStore) {
				for _, name := range []string{"a-agent", "b-agent", "c-agent"} {
					if err := s.Create(context.Background(), testTenant, &agent.Agent{
						Name:      name,
						CreatedBy: testUser,
					}); err != nil {
						panic(err)
					}
				}
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp struct {
					Agents  []any `json:"agents"`
					Page    int   `json:"page"`
					PerPage int   `json:"per_page"`
					Total   int   `json:"total"`
				}
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if len(resp.Agents) != 2 {
					t.Errorf("agents length = %d, want 2", len(resp.Agents))
				}
				if resp.Total != 3 {
					t.Errorf("total = %d, want 3", resp.Total)
				}
				if resp.Page != 1 {
					t.Errorf("page = %d, want 1", resp.Page)
				}
				if resp.PerPage != 2 {
					t.Errorf("per_page = %d, want 2", resp.PerPage)
				}
			},
		},
		{
			name: "pagination: page 2 returns next slice",
			path: "/v1/agents?page=2&per_page=2",
			setup: func(s agent.AgentStore) {
				for _, name := range []string{"a-agent", "b-agent", "c-agent"} {
					if err := s.Create(context.Background(), testTenant, &agent.Agent{
						Name:      name,
						CreatedBy: testUser,
					}); err != nil {
						panic(err)
					}
				}
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp struct {
					Agents []any `json:"agents"`
					Page   int   `json:"page"`
					Total  int   `json:"total"`
				}
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if len(resp.Agents) != 1 {
					t.Errorf("agents length = %d, want 1 (last page)", len(resp.Agents))
				}
				if resp.Total != 3 {
					t.Errorf("total = %d, want 3", resp.Total)
				}
				if resp.Page != 2 {
					t.Errorf("page = %d, want 2", resp.Page)
				}
			},
		},
		{
			name: "pagination: page beyond last returns empty agents with correct total",
			path: "/v1/agents?page=99&per_page=50",
			setup: func(s agent.AgentStore) {
				if err := s.Create(context.Background(), testTenant, &agent.Agent{
					Name:      "lone-agent",
					CreatedBy: testUser,
				}); err != nil {
					panic(err)
				}
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp struct {
					Agents []any `json:"agents"`
					Total  int   `json:"total"`
				}
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if len(resp.Agents) != 0 {
					t.Errorf("agents length = %d, want 0", len(resp.Agents))
				}
				if resp.Total != 1 {
					t.Errorf("total = %d, want 1", resp.Total)
				}
			},
		},
		{
			name:       "401 when no auth context",
			path:       "/v1/agents",
			noAuth:     true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "protocol=mcp returns only MCP agents",
			path: "/v1/agents?protocol=mcp",
			setup: func(s agent.AgentStore) {
				if err := s.Create(context.Background(), testTenant, &agent.Agent{
					Name: "http-agent", CreatedBy: testUser,
				}); err != nil {
					panic(err)
				}
				if err := s.Create(context.Background(), testTenant, &agent.Agent{
					Name: "mcp-agent", CreatedBy: testUser, Protocol: "mcp",
				}); err != nil {
					panic(err)
				}
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp struct {
					Agents []map[string]any `json:"agents"`
					Total  int              `json:"total"`
				}
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp.Total != 1 || len(resp.Agents) != 1 {
					t.Fatalf("total=%d len(agents)=%d, want 1/1", resp.Total, len(resp.Agents))
				}
				if resp.Agents[0]["name"] != "mcp-agent" {
					t.Errorf("agent name = %q, want mcp-agent", resp.Agents[0]["name"])
				}
			},
		},
		{
			name: "protocol=http returns only HTTP agents",
			path: "/v1/agents?protocol=http",
			setup: func(s agent.AgentStore) {
				if err := s.Create(context.Background(), testTenant, &agent.Agent{
					Name: "http-agent", CreatedBy: testUser,
				}); err != nil {
					panic(err)
				}
				if err := s.Create(context.Background(), testTenant, &agent.Agent{
					Name: "mcp-agent", CreatedBy: testUser, Protocol: "mcp",
				}); err != nil {
					panic(err)
				}
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp struct {
					Agents []map[string]any `json:"agents"`
					Total  int              `json:"total"`
				}
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp.Total != 1 || len(resp.Agents) != 1 {
					t.Fatalf("total=%d len(agents)=%d, want 1/1", resp.Total, len(resp.Agents))
				}
				if resp.Agents[0]["name"] != "http-agent" {
					t.Errorf("agent name = %q, want http-agent", resp.Agents[0]["name"])
				}
			},
		},
		{
			name: "protocol omitted returns all agents regardless of protocol",
			path: "/v1/agents",
			setup: func(s agent.AgentStore) {
				if err := s.Create(context.Background(), testTenant, &agent.Agent{
					Name: "http-agent", CreatedBy: testUser,
				}); err != nil {
					panic(err)
				}
				if err := s.Create(context.Background(), testTenant, &agent.Agent{
					Name: "mcp-agent", CreatedBy: testUser, Protocol: "mcp",
				}); err != nil {
					panic(err)
				}
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp struct {
					Total int `json:"total"`
				}
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp.Total != 2 {
					t.Errorf("total = %d, want 2", resp.Total)
				}
			},
		},
		{
			name:       "400 for invalid protocol value",
			path:       "/v1/agents?protocol=grpc",
			wantStatus: http.StatusBadRequest,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]string
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp["error"] == "" {
					t.Error("error field is missing or empty")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHandler(t, tc.setup)
			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			var req *http.Request
			if tc.noAuth {
				req = unauthGetRequest(t, tc.path)
			} else {
				tenant, user := testTenant, testUser
				if tc.reqTenant != "" {
					tenant = tc.reqTenant
				}
				if tc.reqUser != "" {
					user = tc.reqUser
				}
				req = authedGetRequest(t, tc.path, tenant, user)
			}

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s",
					rec.Code, tc.wantStatus, rec.Body.String())
			}

			if tc.checkBody != nil {
				tc.checkBody(t, rec.Body.Bytes())
			}
		})
	}
}

func TestHandleGet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// buildPath seeds agents into the store and returns the request path.
		buildPath  func(t *testing.T, store agent.AgentStore) string
		noAuth     bool
		reqTenant  string
		reqUser    string
		wantStatus int
		checkBody  func(t *testing.T, body []byte)
	}{
		{
			name: "200 returns full agent record",
			buildPath: func(t *testing.T, s agent.AgentStore) string {
				t.Helper()
				a := &agent.Agent{Name: "fetch-me", CreatedBy: testUser}
				if err := s.Create(context.Background(), testTenant, a); err != nil {
					t.Fatal(err)
				}
				return "/v1/agents/" + a.ID
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp["id"] == "" {
					t.Error("id is empty")
				}
				if resp["name"] != "fetch-me" {
					t.Errorf("name = %q, want %q", resp["name"], "fetch-me")
				}
				// No upstream_url → status is pending.
				if resp["status"] != "pending" {
					t.Errorf("status = %q, want pending", resp["status"])
				}
				if resp["created_by"] != testUser {
					t.Errorf("created_by = %q, want %q", resp["created_by"], testUser)
				}
			},
		},
		{
			name: "404 for completely unknown agent ID",
			buildPath: func(t *testing.T, s agent.AgentStore) string {
				t.Helper()
				return "/v1/agents/agt_doesnotexist"
			},
			wantStatus: http.StatusNotFound,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]string
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp["error"] == "" {
					t.Error("error field is missing or empty")
				}
			},
		},
		{
			// Spec 0001 / invariant #2: existence must not be confirmed across tenants.
			// A valid agent owned by another tenant must return 404, not 403.
			name: "404 not 403 for agent owned by another tenant",
			buildPath: func(t *testing.T, s agent.AgentStore) string {
				t.Helper()
				a := &agent.Agent{Name: "foreign-agent", CreatedBy: "user-other"}
				if err := s.Create(context.Background(), "tenant-other", a); err != nil {
					t.Fatal(err)
				}
				return "/v1/agents/" + a.ID
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "401 when no auth context",
			buildPath: func(t *testing.T, s agent.AgentStore) string {
				t.Helper()
				return "/v1/agents/agt_anyid"
			},
			noAuth:     true,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, store := newHandlerForRead(t)
			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			path := tc.buildPath(t, store)

			var req *http.Request
			if tc.noAuth {
				req = unauthGetRequest(t, path)
			} else {
				tenant, user := testTenant, testUser
				if tc.reqTenant != "" {
					tenant = tc.reqTenant
				}
				if tc.reqUser != "" {
					user = tc.reqUser
				}
				req = authedGetRequest(t, path, tenant, user)
			}

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s",
					rec.Code, tc.wantStatus, rec.Body.String())
			}

			if tc.checkBody != nil {
				tc.checkBody(t, rec.Body.Bytes())
			}
		})
	}
}
