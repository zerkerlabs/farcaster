package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/auth/authtest"
)

// authedPatchRequest returns a PATCH request to path with body and auth context.
func authedPatchRequest(t *testing.T, path string, body any, tenant, user string) *http.Request {
	t.Helper()
	var b []byte
	if raw, ok := body.([]byte); ok {
		b = raw
	} else {
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(authtest.WithIdentity(req.Context(), tenant, user))
}

// unauthPatchRequest returns a PATCH request with no auth context set.
func unauthPatchRequest(t *testing.T, path string, body any) *http.Request {
	t.Helper()
	var b []byte
	if raw, ok := body.([]byte); ok {
		b = raw
	} else {
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// seedAgent creates an agent in s and returns its assigned ID.
func seedAgent(t *testing.T, s agent.AgentStore, tenantID, name, user string) string {
	t.Helper()
	a := &agent.Agent{Name: name, CreatedBy: user}
	if err := s.Create(context.Background(), tenantID, a); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return a.ID
}

func TestHandleUpdate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(*testing.T, agent.AgentStore) string // returns agent id to update
		body       any
		wantStatus int
		checkBody  func(t *testing.T, body []byte)
		noAuth     bool
		reqTenant  string
		reqUser    string
	}{
		{
			name: "200 partial update — description only",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "my-agent", testUser)
			},
			body:       map[string]any{"description": "updated description"},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["description"] != "updated description" {
					t.Errorf("description = %q, want %q", resp["description"], "updated description")
				}
				// untouched mutable fields remain
				if resp["name"] != "my-agent" {
					t.Errorf("name = %q, want my-agent", resp["name"])
				}
				if resp["id"] == "" {
					t.Error("id is empty")
				}
				// seeded without upstream_url → pending; description-only patch doesn't change status
				if resp["status"] != "pending" {
					t.Errorf("status = %q, want pending", resp["status"])
				}
			},
		},
		{
			name: "200 toggle emit_receipts on",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "receipts-toggle", testUser)
			},
			body:       map[string]any{"emit_receipts": true},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["emit_receipts"] != true {
					t.Errorf("emit_receipts = %v, want true", resp["emit_receipts"])
				}
			},
		},
		{
			name: "200 toggle capture_body on",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "capture-toggle", testUser)
			},
			body:       map[string]any{"capture_body": true},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["capture_body"] != true {
					t.Errorf("capture_body = %v, want true", resp["capture_body"])
				}
			},
		},
		{
			name: "200 update name and tags",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "old-name", testUser)
			},
			body: map[string]any{
				"name": "new-name",
				"tags": []string{"production", "support"},
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["name"] != "new-name" {
					t.Errorf("name = %q, want new-name", resp["name"])
				}
				tags, _ := resp["tags"].([]any)
				if len(tags) != 2 {
					t.Errorf("tags length = %d, want 2", len(tags))
				}
			},
		},
		{
			name: "200 update metadata",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "meta-agent", testUser)
			},
			body:       map[string]any{"metadata": map[string]any{"team": "platform", "version": "2"}},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				meta, _ := resp["metadata"].(map[string]any)
				if meta["team"] != "platform" {
					t.Errorf("metadata.team = %q, want platform", meta["team"])
				}
			},
		},
		{
			// Immutable fields in the body must be silently ignored; only mutable
			// fields take effect (spec 0001 § Update semantics).
			name: "200 immutable fields in body are silently ignored",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "my-agent", testUser)
			},
			body: map[string]any{
				"id":         "fake-id",
				"status":     "inactive",
				"created_by": "attacker",
				"tenant_id":  "other-tenant",
				"name":       "updated-name",
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["name"] != "updated-name" {
					t.Errorf("name = %q, want updated-name", resp["name"])
				}
				// "status": "inactive" in the body must be silently ignored.
				// The agent was seeded without upstream_url → status remains pending.
				if resp["status"] != "pending" {
					t.Errorf("status = %q, want pending (immutable field must be ignored)", resp["status"])
				}
				if resp["created_by"] != testUser {
					t.Errorf("created_by = %q, want %q (immutable field must be ignored)", resp["created_by"], testUser)
				}
			},
		},
		{
			name: "200 set upstream_url transitions pending → active",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "pending-agent", testUser)
			},
			body:       map[string]any{"upstream_url": "https://agent.example.com/invoke"},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["status"] != "active" {
					t.Errorf("status = %q, want active", resp["status"])
				}
				if resp["upstream_url"] != "https://agent.example.com/invoke" {
					t.Errorf("upstream_url = %v, want https://agent.example.com/invoke", resp["upstream_url"])
				}
			},
		},
		{
			// Regression for the **string null-vs-absent footgun: an explicit
			// JSON null must CLEAR upstream_url (reverting status to pending),
			// not silently no-op. This case fails against a plain **string field.
			name: "200 upstream_url:null clears it and reverts to pending",
			setup: func(t *testing.T, s agent.AgentStore) string {
				url := "https://agent.example.com/invoke"
				a := &agent.Agent{Name: "clearable-agent", CreatedBy: testUser, UpstreamURL: &url}
				if err := s.Create(context.Background(), testTenant, a); err != nil {
					t.Fatalf("seed active agent: %v", err)
				}
				return a.ID
			},
			body:       []byte(`{"upstream_url": null}`),
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["upstream_url"] != nil {
					t.Errorf("upstream_url = %v, want nil (cleared)", resp["upstream_url"])
				}
				if resp["status"] != "pending" {
					t.Errorf("status = %q, want pending after clearing upstream_url", resp["status"])
				}
			},
		},
		{
			// Companion to the case above: an ABSENT upstream_url key leaves the
			// field unchanged. Together these prove null != absent.
			name: "200 patch without upstream_url leaves it unchanged",
			setup: func(t *testing.T, s agent.AgentStore) string {
				url := "https://agent.example.com/invoke"
				a := &agent.Agent{Name: "keep-agent", CreatedBy: testUser, UpstreamURL: &url}
				if err := s.Create(context.Background(), testTenant, a); err != nil {
					t.Fatalf("seed active agent: %v", err)
				}
				return a.ID
			},
			body:       map[string]any{"description": "touch something else"},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["upstream_url"] != "https://agent.example.com/invoke" {
					t.Errorf("upstream_url = %v, want unchanged", resp["upstream_url"])
				}
				if resp["status"] != "active" {
					t.Errorf("status = %q, want active (unchanged)", resp["status"])
				}
			},
		},
		{
			// SSRF guard: setting upstream_url to a private/loopback address
			// must be rejected at the boundary (validateUpstreamURL).
			name: "400 on upstream_url targeting a private address",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "ssrf-agent", testUser)
			},
			body:       map[string]any{"upstream_url": "http://169.254.169.254/latest/meta-data/"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 on mcp agent's upstream_url targeting a loopback address",
			setup: func(t *testing.T, s agent.AgentStore) string {
				transport := "streamable_http"
				a := &agent.Agent{
					Name: "mcp-ssrf-agent", CreatedBy: testUser,
					Protocol: "mcp", MCPTransport: &transport,
				}
				if err := s.Create(context.Background(), testTenant, a); err != nil {
					t.Fatalf("seed mcp agent: %v", err)
				}
				return a.ID
			},
			body:       map[string]any{"upstream_url": "http://127.0.0.1/"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "404 for unknown agent id",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return "agt_doesnotexist"
			},
			body:       map[string]any{"description": "something"},
			wantStatus: http.StatusNotFound,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]string
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if resp["error"] == "" {
					t.Error("error field is missing")
				}
			},
		},
		{
			// Spec invariant #2: existence is never confirmed across tenants.
			// An agent owned by tenant-2 must return 404, not 403, for tenant-1.
			name: "404 for agent belonging to a different tenant",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, "tenant-2", "other-agent", "user-2")
			},
			body:       map[string]any{"description": "something"},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "409 on name collision within tenant",
			setup: func(t *testing.T, s agent.AgentStore) string {
				seedAgent(t, s, testTenant, "existing-agent", testUser)
				return seedAgent(t, s, testTenant, "my-agent", testUser)
			},
			body:       map[string]any{"name": "existing-agent"},
			wantStatus: http.StatusConflict,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]string
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if resp["error"] == "" {
					t.Error("error field is missing")
				}
			},
		},
		{
			name: "400 on malformed JSON",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "my-agent", testUser)
			},
			body:       []byte(`{not valid json`),
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 when name is explicitly empty string",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "my-agent", testUser)
			},
			body:       map[string]any{"name": ""},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "200 transition http agent to mcp with streamable_http",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "to-be-mcp", testUser)
			},
			body: map[string]any{
				"protocol":      "mcp",
				"mcp_transport": "streamable_http",
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["protocol"] != "mcp" {
					t.Errorf("protocol = %v, want mcp", resp["protocol"])
				}
				if resp["mcp_transport"] != "streamable_http" {
					t.Errorf("mcp_transport = %v, want streamable_http", resp["mcp_transport"])
				}
			},
		},
		{
			name: "400 transition to mcp without mcp_transport",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "mcp-missing-transport", testUser)
			},
			body:       map[string]any{"protocol": "mcp"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 transition to mcp with stdio transport is rejected",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "mcp-stdio-patch", testUser)
			},
			body: map[string]any{
				"protocol":      "mcp",
				"mcp_transport": "stdio",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			// The agent is already protocol=mcp; a PATCH that only touches
			// mcp_transport must still validate against the resulting record.
			name: "400 setting mcp_transport to sse on an existing mcp agent",
			setup: func(t *testing.T, s agent.AgentStore) string {
				transport := "streamable_http"
				a := &agent.Agent{
					Name: "already-mcp", CreatedBy: testUser,
					Protocol: "mcp", MCPTransport: &transport,
				}
				if err := s.Create(context.Background(), testTenant, a); err != nil {
					t.Fatalf("seed mcp agent: %v", err)
				}
				return a.ID
			},
			body:       map[string]any{"mcp_transport": "sse"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "200 transition mcp agent back to http clears mcp_transport",
			setup: func(t *testing.T, s agent.AgentStore) string {
				transport := "streamable_http"
				a := &agent.Agent{
					Name: "mcp-to-http", CreatedBy: testUser,
					Protocol: "mcp", MCPTransport: &transport,
				}
				if err := s.Create(context.Background(), testTenant, a); err != nil {
					t.Fatalf("seed mcp agent: %v", err)
				}
				return a.ID
			},
			body:       []byte(`{"protocol": "http", "mcp_transport": null}`),
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["protocol"] != "http" {
					t.Errorf("protocol = %v, want http", resp["protocol"])
				}
				if resp["mcp_transport"] != nil {
					t.Errorf("mcp_transport = %v, want nil", resp["mcp_transport"])
				}
			},
		},
		{
			// An http agent may not gain an mcp_transport without also switching
			// to protocol=mcp (spec 0004 decision 3) — the resulting record would
			// be an http agent with a stale transport.
			name: "400 setting mcp_transport on an http agent",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "http-no-transport", testUser)
			},
			body:       map[string]any{"mcp_transport": "streamable_http"},
			wantStatus: http.StatusBadRequest,
		},
		{
			// spec 0005: set route pricing on an existing agent.
			name: "200 set route pricing on an agent",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "to-be-priced", testUser)
			},
			body: map[string]any{
				"pricing": map[string]any{
					"amount": "10000", "asset": "USDC", "network": "base",
					"pay_to": "0x0000000000000000000000000000000000000001",
				},
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				pricing, ok := resp["pricing"].(map[string]any)
				if !ok {
					t.Fatalf("pricing = %v, want object", resp["pricing"])
				}
				if pricing["amount"] != "10000" {
					t.Errorf("pricing.amount = %v, want 10000", pricing["amount"])
				}
			},
		},
		{
			// spec 0005: clearing pricing (explicit null) unprices the agent.
			name: "200 clear pricing with null",
			setup: func(t *testing.T, s agent.AgentStore) string {
				a := &agent.Agent{
					Name: "priced-to-clear", CreatedBy: testUser,
					Pricing: &agent.Pricing{
						Amount: "1", Asset: "USDC", Network: "base",
						PayTo: "0x0000000000000000000000000000000000000001",
					},
				}
				if err := s.Create(context.Background(), testTenant, a); err != nil {
					t.Fatalf("seed priced agent: %v", err)
				}
				return a.ID
			},
			body:       []byte(`{"pricing": null}`),
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["pricing"] != nil {
					t.Errorf("pricing = %v, want nil after clear", resp["pricing"])
				}
			},
		},
		{
			// spec 0005 decision 5: adding per-tool pricing must validate against
			// the resulting record — an http agent (default) cannot take tools.
			name: "400 adding per-tool pricing to an http agent",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "http-no-tools", testUser)
			},
			body: map[string]any{
				"pricing": map[string]any{
					"amount": "1", "asset": "USDC", "network": "base",
					"pay_to": "0x0000000000000000000000000000000000000001",
					"tools":  map[string]any{"search_docs": "50000"},
				},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 pricing with malformed pay_to",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "bad-payto-patch", testUser)
			},
			body: map[string]any{
				"pricing": map[string]any{
					"amount": "1", "asset": "USDC", "network": "base",
					"pay_to": "nope",
				},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "401 when request carries no auth context",
			setup: func(t *testing.T, s agent.AgentStore) string {
				return seedAgent(t, s, testTenant, "my-agent", testUser)
			},
			body:       map[string]any{"description": "something"},
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

			var agentID string
			if tc.setup != nil {
				agentID = tc.setup(t, store)
			}

			path := "/v1/agents/" + agentID

			var req *http.Request
			if tc.noAuth {
				req = unauthPatchRequest(t, path, tc.body)
			} else {
				tenant, user := testTenant, testUser
				if tc.reqTenant != "" {
					tenant = tc.reqTenant
				}
				if tc.reqUser != "" {
					user = tc.reqUser
				}
				req = authedPatchRequest(t, path, tc.body, tenant, user)
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
