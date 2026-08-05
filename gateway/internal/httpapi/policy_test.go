package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/gateway/gateway/internal/httpapi"
	"github.com/zerkerlabs/gateway/gateway/internal/policy"
)

// newPolicyHandler returns a Handler wired with a fresh in-memory policy store.
func newPolicyHandler(t *testing.T) *httpapi.Handler {
	t.Helper()
	return httpapi.NewHandler(agent.NewMemoryStore(), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithPolicy(policy.NewMemoryStore())
}

// policyReq builds a request to /v1/policy with body and auth context; passing
// tenant and user both empty produces an unauthenticated request.
func policyReq(t *testing.T, method string, body any, tenant, user string) *http.Request {
	t.Helper()
	var b []byte
	if raw, ok := body.([]byte); ok {
		b = raw
	} else if body != nil {
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, "/v1/policy", bytes.NewReader(b))
	if len(b) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if tenant == "" && user == "" {
		return req
	}
	return req.WithContext(authtest.WithIdentity(req.Context(), tenant, user))
}

func TestGetPolicy_AbsentReturnsEmptyDefault(t *testing.T) {
	t.Parallel()

	h := newPolicyHandler(t)
	rec := serve(t, h, policyReq(t, http.MethodGet, nil, testTenant, testUser))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["version"] != float64(0) {
		t.Errorf("version = %v, want 0", resp["version"])
	}
	rules, ok := resp["rules"].([]any)
	if !ok || len(rules) != 0 {
		t.Errorf("rules = %v, want empty array", resp["rules"])
	}
	if _, present := resp["updated_at"]; present {
		t.Errorf("updated_at present on absent policy: %v", resp["updated_at"])
	}
}

func TestGetPolicy_NoAuth(t *testing.T) {
	t.Parallel()

	h := newPolicyHandler(t)
	rec := serve(t, h, policyReq(t, http.MethodGet, nil, "", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestPutPolicy_AuthoringExample exercises the spec 0009 §Surface authoring
// example verbatim: PUT the document, expect version + updated_at back, then
// GET must round-trip the full document.
func TestPutPolicy_AuthoringExample(t *testing.T) {
	t.Parallel()

	h := newPolicyHandler(t)
	body := map[string]any{
		"default":  "allow",
		"on_error": "deny",
		"rules": []map[string]any{
			{"action": "allow", "match": map[string]any{"agents": []string{"agt_01J9X8"}, "tools": []string{"search_docs", "read_*"}}},
			{"action": "deny", "match": map[string]any{"tools": []string{"delete_*"}}},
			{"action": "warn", "match": map[string]any{"max_body_bytes": 32768}},
			{"action": "deny", "match": map[string]any{"rate_per_min": 60}},
		},
	}

	rec := serve(t, h, policyReq(t, http.MethodPut, body, testTenant, testUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var putResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	if putResp["version"] != float64(1) {
		t.Errorf("version = %v, want 1", putResp["version"])
	}
	if putResp["updated_at"] == nil || putResp["updated_at"] == "" {
		t.Errorf("updated_at missing from PUT response: %v", putResp)
	}

	getRec := serve(t, h, policyReq(t, http.MethodGet, nil, testTenant, testUser))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d, body = %s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	var getResp map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if getResp["default"] != "allow" || getResp["on_error"] != "deny" {
		t.Errorf("default/on_error = %v/%v, want allow/deny", getResp["default"], getResp["on_error"])
	}
	rules, ok := getResp["rules"].([]any)
	if !ok || len(rules) != 4 {
		t.Fatalf("rules = %v, want 4 entries", getResp["rules"])
	}
}

func TestPutPolicy_ReplacesAndIncrementsVersion(t *testing.T) {
	t.Parallel()

	h := newPolicyHandler(t)
	first := map[string]any{
		"default":  "allow",
		"on_error": "deny",
		"rules": []map[string]any{
			{"action": "deny", "match": map[string]any{"tools": []string{"delete_*"}}},
		},
	}
	rec := serve(t, h, policyReq(t, http.MethodPut, first, testTenant, testUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("first PUT status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	second := map[string]any{"default": "deny", "on_error": "deny", "rules": []map[string]any{}}
	rec = serve(t, h, policyReq(t, http.MethodPut, second, testTenant, testUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("second PUT status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["version"] != float64(2) {
		t.Errorf("version = %v, want 2", resp["version"])
	}

	getRec := serve(t, h, policyReq(t, http.MethodGet, nil, testTenant, testUser))
	var getResp map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if getResp["default"] != "deny" {
		t.Errorf("default = %v, want deny (wholesale replace)", getResp["default"])
	}
	if rules, ok := getResp["rules"].([]any); !ok || len(rules) != 0 {
		t.Errorf("rules = %v, want empty (wholesale replace)", getResp["rules"])
	}
}

func TestPutPolicy_NoAuth(t *testing.T) {
	t.Parallel()

	h := newPolicyHandler(t)
	body := map[string]any{"default": "allow", "on_error": "deny", "rules": []map[string]any{}}
	rec := serve(t, h, policyReq(t, http.MethodPut, body, "", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPutPolicy_MalformedDocumentRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body map[string]any
	}{
		{
			name: "unknown default action",
			body: map[string]any{"default": "block", "on_error": "deny", "rules": []map[string]any{}},
		},
		{
			name: "missing default",
			body: map[string]any{"on_error": "deny", "rules": []map[string]any{}},
		},
		{
			name: "unknown on_error action",
			body: map[string]any{"default": "allow", "on_error": "ignore", "rules": []map[string]any{}},
		},
		{
			name: "unknown rule action",
			body: map[string]any{
				"default": "allow", "on_error": "deny",
				"rules": []map[string]any{{"action": "flag", "match": map[string]any{"tools": []string{"delete_*"}}}},
			},
		},
		{
			name: "wildcard not trailing",
			body: map[string]any{
				"default": "allow", "on_error": "deny",
				"rules": []map[string]any{{"action": "deny", "match": map[string]any{"tools": []string{"*_delete"}}}},
			},
		},
		{
			name: "empty tool entry",
			body: map[string]any{
				"default": "allow", "on_error": "deny",
				"rules": []map[string]any{{"action": "deny", "match": map[string]any{"tools": []string{""}}}},
			},
		},
		{
			name: "negative max_body_bytes",
			body: map[string]any{
				"default": "allow", "on_error": "deny",
				"rules": []map[string]any{{"action": "warn", "match": map[string]any{"max_body_bytes": -1}}},
			},
		},
		{
			name: "non-positive rate_per_min",
			body: map[string]any{
				"default": "allow", "on_error": "deny",
				"rules": []map[string]any{{"action": "deny", "match": map[string]any{"rate_per_min": 0}}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newPolicyHandler(t)
			rec := serve(t, h, policyReq(t, http.MethodPut, tc.body, testTenant, testUser))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			var resp map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if resp["error"] == "" {
				t.Error("error field is missing or empty")
			}
		})
	}
}

func TestPutPolicy_MalformedJSON(t *testing.T) {
	t.Parallel()

	h := newPolicyHandler(t)
	rec := serve(t, h, policyReq(t, http.MethodPut, []byte(`{not valid json`), testTenant, testUser))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestPolicy_TenantIsolation(t *testing.T) {
	t.Parallel()

	h := newPolicyHandler(t)
	body := map[string]any{
		"default": "deny", "on_error": "deny",
		"rules": []map[string]any{{"action": "deny", "match": map[string]any{"tools": []string{"delete_*"}}}},
	}
	rec := serve(t, h, policyReq(t, http.MethodPut, body, testTenant, testUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// A different tenant must see the empty default, never tenant-1's document.
	getRec := serve(t, h, policyReq(t, http.MethodGet, nil, "tenant-2", testUser))
	if getRec.Code != http.StatusOK {
		t.Fatalf("cross-tenant GET status = %d, want %d, body = %s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["default"] != nil && resp["default"] != "" {
		t.Errorf("cross-tenant GET leaked tenant-1's default: %v", resp["default"])
	}
	if rules, ok := resp["rules"].([]any); !ok || len(rules) != 0 {
		t.Errorf("cross-tenant GET leaked tenant-1's rules: %v", resp["rules"])
	}
}
