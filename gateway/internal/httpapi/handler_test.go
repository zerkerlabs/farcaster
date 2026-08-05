package httpapi_test

import (
	"bytes"
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

const (
	testTenant = "tenant-1"
	testUser   = "user-1"
)

// newHandler returns a Handler wired to a fresh MemoryStore, optionally
// pre-populated by setup.
func newHandler(t *testing.T, setup func(agent.AgentStore)) *httpapi.Handler {
	t.Helper()
	store := agent.NewMemoryStore()
	if setup != nil {
		setup(store)
	}
	return httpapi.NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// authedRequestAs returns a POST request to path with body, with the auth
// context pre-populated for the given tenant and user (as if the auth
// middleware has already run).
func authedRequestAs(t *testing.T, path string, body any, tenant, user string) *http.Request {
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
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(authtest.WithIdentity(req.Context(), tenant, user))
}

// unauthRequest returns a POST request with no auth context set.
func unauthRequest(t *testing.T, path string, body any) *http.Request {
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
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestHandleCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       any // map[string]any for valid JSON, []byte for raw
		setup      func(agent.AgentStore)
		wantStatus int
		checkBody  func(t *testing.T, body []byte)
		// noAuth sends the request without an auth context (no middleware ran).
		noAuth bool
		// reqTenant/reqUser override the authenticated identity (ignored when noAuth).
		reqTenant string
		reqUser   string
	}{
		{
			name:       "201 with minimal valid body — no upstream_url → pending",
			body:       map[string]any{"name": "my-agent"},
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["id"] == "" {
					t.Error("id is empty")
				}
				// No upstream_url supplied → status must be pending (spec 0002).
				if resp["status"] != "pending" {
					t.Errorf("status = %q, want %q", resp["status"], "pending")
				}
				if resp["name"] != "my-agent" {
					t.Errorf("name = %q, want %q", resp["name"], "my-agent")
				}
				if resp["created_by"] != testUser {
					t.Errorf("created_by = %q, want %q", resp["created_by"], testUser)
				}
				if _, ok := resp["upstream_url"]; !ok {
					t.Error("upstream_url field missing from response")
				}
			},
		},
		{
			name: "201 with upstream_url → active status",
			body: map[string]any{
				"name":         "routable-agent",
				"upstream_url": "https://agent.example.com/invoke",
			},
			wantStatus: http.StatusCreated,
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
			name: "201 with emit_receipts true is reflected in the response",
			body: map[string]any{
				"name":          "receipts-agent",
				"emit_receipts": true,
			},
			wantStatus: http.StatusCreated,
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
			name:       "201 defaults emit_receipts to false when omitted",
			body:       map[string]any{"name": "default-receipts-agent"},
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["emit_receipts"] != false {
					t.Errorf("emit_receipts = %v, want false", resp["emit_receipts"])
				}
			},
		},
		{
			name: "201 with capture_body true is reflected in the response",
			body: map[string]any{
				"name":         "capture-agent",
				"capture_body": true,
			},
			wantStatus: http.StatusCreated,
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
			name:       "201 defaults capture_body to false when omitted",
			body:       map[string]any{"name": "default-capture-agent"},
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["capture_body"] != false {
					t.Errorf("capture_body = %v, want false", resp["capture_body"])
				}
			},
		},
		{
			name: "201 with all optional fields",
			body: map[string]any{
				"name":        "full-agent",
				"description": "A fully-specified agent",
				"tags":        []string{"production", "text"},
				"metadata":    map[string]any{"team": "platform"},
			},
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["description"] != "A fully-specified agent" {
					t.Errorf("description = %q", resp["description"])
				}
				tags, _ := resp["tags"].([]any)
				if len(tags) != 2 {
					t.Errorf("tags length = %d, want 2", len(tags))
				}
				if resp["id"] == "" {
					t.Error("id is empty")
				}
			},
		},
		{
			name: "409 on duplicate name within tenant",
			body: map[string]any{"name": "existing-agent"},
			setup: func(s agent.AgentStore) {
				if err := s.Create(context.Background(), testTenant, &agent.Agent{
					Name:      "existing-agent",
					CreatedBy: testUser,
				}); err != nil {
					panic(err)
				}
			},
			wantStatus: http.StatusConflict,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]string
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if resp["error"] == "" {
					t.Error("error field is missing or empty")
				}
			},
		},
		{
			// SSRF guard: an upstream_url targeting a private/loopback/link-local
			// address must be rejected at creation (validateUpstreamURL).
			name: "400 on upstream_url targeting a private address",
			body: map[string]any{
				"name":         "ssrf-agent",
				"upstream_url": "http://127.0.0.1:8080/",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 when name is absent",
			body:       map[string]any{"description": "no name here"},
			wantStatus: http.StatusBadRequest,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]string
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if resp["error"] == "" {
					t.Error("error field is missing or empty")
				}
			},
		},
		{
			name:       "400 when name is empty string",
			body:       map[string]any{"name": ""},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 on malformed JSON",
			body:       []byte(`{not valid json`),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 on empty body",
			body:       []byte(``),
			wantStatus: http.StatusBadRequest,
		},
		{
			// Spec: "All endpoints require a valid OAuth token; unauthenticated requests get 401."
			// Handler defense-in-depth: reject when no identity is in context.
			name:       "401 when request carries no auth context",
			body:       map[string]any{"name": "some-agent"},
			noAuth:     true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			// spec 0004 decision 3: absent protocol defaults to "http".
			name:       "201 with no protocol given defaults to http",
			body:       map[string]any{"name": "default-protocol-agent"},
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["protocol"] != "http" {
					t.Errorf("protocol = %v, want %q", resp["protocol"], "http")
				}
				if resp["mcp_transport"] != nil {
					t.Errorf("mcp_transport = %v, want nil", resp["mcp_transport"])
				}
			},
		},
		{
			name: "201 valid mcp agent with streamable_http transport",
			body: map[string]any{
				"name":                 "mcp-agent",
				"protocol":             "mcp",
				"mcp_transport":        "streamable_http",
				"mcp_protocol_version": "2025-06-18",
				"upstream_url":         "https://mcp.example.com/",
			},
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["protocol"] != "mcp" {
					t.Errorf("protocol = %v, want %q", resp["protocol"], "mcp")
				}
				if resp["mcp_transport"] != "streamable_http" {
					t.Errorf("mcp_transport = %v, want streamable_http", resp["mcp_transport"])
				}
				if resp["mcp_protocol_version"] != "2025-06-18" {
					t.Errorf("mcp_protocol_version = %v, want 2025-06-18", resp["mcp_protocol_version"])
				}
			},
		},
		{
			name:       "400 mcp agent missing mcp_transport",
			body:       map[string]any{"name": "mcp-no-transport", "protocol": "mcp"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 mcp agent with stdio transport is rejected",
			body: map[string]any{
				"name":          "mcp-stdio-agent",
				"protocol":      "mcp",
				"mcp_transport": "stdio",
			},
			wantStatus: http.StatusBadRequest,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]string
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if resp["error"] == "" {
					t.Error("error field is missing or empty")
				}
			},
		},
		{
			name: "400 mcp agent with sse transport is rejected",
			body: map[string]any{
				"name":          "mcp-sse-agent",
				"protocol":      "mcp",
				"mcp_transport": "sse",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			// spec 0004 decision 10: validateUpstreamURL applies identically to a
			// Protocol=mcp agent's upstream_url — an MCP upstream is just a URL,
			// and there is no protocol-specific bypass of the SSRF write-time guard.
			name: "400 mcp agent with loopback upstream_url is rejected (SSRF)",
			body: map[string]any{
				"name":          "mcp-loopback-agent",
				"protocol":      "mcp",
				"mcp_transport": "streamable_http",
				"upstream_url":  "http://127.0.0.1/",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 mcp agent with private upstream_url is rejected (SSRF)",
			body: map[string]any{
				"name":          "mcp-private-agent",
				"protocol":      "mcp",
				"mcp_transport": "streamable_http",
				"upstream_url":  "http://10.0.0.5/",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 mcp agent with link-local (cloud metadata) upstream_url is rejected (SSRF)",
			body: map[string]any{
				"name":          "mcp-metadata-agent",
				"protocol":      "mcp",
				"mcp_transport": "streamable_http",
				"upstream_url":  "http://169.254.169.254/latest/meta-data/",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 unknown protocol value",
			body:       map[string]any{"name": "bad-protocol-agent", "protocol": "carrier-pigeon"},
			wantStatus: http.StatusBadRequest,
		},
		{
			// spec 0004 decision 3: the MCP fields are only valid when
			// protocol=mcp; an http agent (the default) may not carry an
			// mcp_transport.
			name:       "400 http agent may not set mcp_transport",
			body:       map[string]any{"name": "http-with-transport", "mcp_transport": "streamable_http"},
			wantStatus: http.StatusBadRequest,
		},
		{
			// spec 0005: an http agent may be route-priced (no per-tool overrides).
			name: "201 http agent with route pricing",
			body: map[string]any{
				"name": "priced-http",
				"pricing": map[string]any{
					"amount":  "10000",
					"asset":   "USDC",
					"network": "base",
					"pay_to":  "0x0000000000000000000000000000000000000001",
				},
			},
			wantStatus: http.StatusCreated,
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
				if pricing["amount"] != "10000" || pricing["pay_to"] != "0x0000000000000000000000000000000000000001" {
					t.Errorf("pricing = %v, unexpected fields", pricing)
				}
			},
		},
		{
			// spec 0005 decision 5: per-tool overrides require protocol=mcp.
			name: "201 mcp agent with per-tool pricing",
			body: map[string]any{
				"name":          "priced-mcp",
				"protocol":      "mcp",
				"mcp_transport": "streamable_http",
				"pricing": map[string]any{
					"amount":  "10000",
					"asset":   "USDC",
					"network": "base",
					"pay_to":  "0x0000000000000000000000000000000000000001",
					"tools":   map[string]any{"search_docs": "50000"},
				},
			},
			wantStatus: http.StatusCreated,
		},
		{
			name: "400 pricing with unsupported network",
			body: map[string]any{
				"name": "bad-network",
				"pricing": map[string]any{
					"amount": "1", "asset": "USDC", "network": "ethereum",
					"pay_to": "0x0000000000000000000000000000000000000001",
				},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 pricing with unsupported asset",
			body: map[string]any{
				"name": "bad-asset",
				"pricing": map[string]any{
					"amount": "1", "asset": "DAI", "network": "base",
					"pay_to": "0x0000000000000000000000000000000000000001",
				},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 pricing with malformed pay_to",
			body: map[string]any{
				"name": "bad-payto",
				"pricing": map[string]any{
					"amount": "1", "asset": "USDC", "network": "base",
					"pay_to": "not-an-address",
				},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "400 pricing with non-integer amount",
			body: map[string]any{
				"name": "bad-amount",
				"pricing": map[string]any{
					"amount": "1.5", "asset": "USDC", "network": "base",
					"pay_to": "0x0000000000000000000000000000000000000001",
				},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			// spec 0005 decision 5: per-tool pricing on a non-mcp agent is rejected.
			name: "400 per-tool pricing on an http agent",
			body: map[string]any{
				"name": "http-with-tools",
				"pricing": map[string]any{
					"amount": "1", "asset": "USDC", "network": "base",
					"pay_to": "0x0000000000000000000000000000000000000001",
					"tools":  map[string]any{"search_docs": "50000"},
				},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			// ADR-0005 / spec 0001: tenant scoping is enforced in the storage layer.
			// An agent registered under tenant-1 must not collide with tenant-2's
			// namespace — the same name is valid for a different tenant (201, not 409).
			name: "tenant isolation: different tenant can register same-named agent",
			setup: func(s agent.AgentStore) {
				if err := s.Create(context.Background(), testTenant, &agent.Agent{
					Name:      "shared-name",
					CreatedBy: testUser,
				}); err != nil {
					panic(err)
				}
			},
			body:       map[string]any{"name": "shared-name"},
			reqTenant:  "tenant-2",
			reqUser:    "user-2",
			wantStatus: http.StatusCreated,
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
				req = unauthRequest(t, "/v1/agents", tc.body)
			} else {
				tenant, user := testTenant, testUser
				if tc.reqTenant != "" {
					tenant = tc.reqTenant
				}
				if tc.reqUser != "" {
					user = tc.reqUser
				}
				req = authedRequestAs(t, "/v1/agents", tc.body, tenant, user)
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
