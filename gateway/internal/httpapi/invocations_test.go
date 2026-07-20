package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/farcaster/gateway/internal/httpapi"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
)

// newInvocationListHandler wires a Handler with only an invocation store
// (no agent store or forwarder required for the list endpoint).
func newInvocationListHandler(t *testing.T) (*httpapi.Handler, *invocation.MemoryStore) {
	t.Helper()
	invStore := invocation.NewMemoryStore()
	h := httpapi.NewHandler(nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(&mockForwarder{}, invStore)
	return h, invStore
}

// seedInvocation creates an invocation in the store and returns it.
func seedInvocation(t *testing.T, store *invocation.MemoryStore, tenantID string, inv *invocation.Invocation) *invocation.Invocation {
	t.Helper()
	if err := store.Create(context.Background(), tenantID, inv); err != nil {
		t.Fatalf("seedInvocation: %v", err)
	}
	return inv
}

// listInvocations issues GET /v1/invocations with the given query string under
// testTenant and returns the recorder. Cross-tenant tests should build requests
// directly rather than using this helper.
func listInvocations(t *testing.T, mux *http.ServeMux, query string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v1/invocations"
	if query != "" {
		path += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(authtest.WithIdentity(req.Context(), testTenant, testUser))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// decodeListResponse unmarshals the response body into invocationListResponse.
func decodeListResponse(t *testing.T, body []byte) struct {
	Data   []map[string]any `json:"data"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
} {
	t.Helper()
	var resp struct {
		Data   []map[string]any `json:"data"`
		Total  int              `json:"total"`
		Limit  int              `json:"limit"`
		Offset int              `json:"offset"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode list response: %v\nbody: %s", err, body)
	}
	return resp
}

// TestHandleListInvocations_SpecExample is the contract test for the spec's
// request/response example from docs/specs/0003-observability.md:
//
//	GET /v1/invocations?agent_id=<id>&status=failed&limit=2
//
// The response must carry: data (array of metadata-only items), total, limit,
// offset, and each item must have all required fields.
func TestHandleListInvocations_SpecExample(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	const agentID = "agt_018f3a000000"
	model := "gpt-4o"
	ec := invocation.ErrorClassTimeout
	upstreamStatus := 502
	latency := int64(30001)

	// Seed 17 failed invocations for the target agent (matching the spec example
	// total=17) so we can verify the pagination envelope is correct.
	for i := range 17 {
		inv := &invocation.Invocation{
			AgentID:        agentID,
			Mode:           invocation.ModeStreaming,
			Status:         invocation.StatusFailed,
			ErrorClass:     &ec,
			Model:          &model,
			UpstreamStatus: &upstreamStatus,
			LatencyMS:      &latency,
		}
		seedInvocation(t, invStore, testTenant, inv)
		_ = i
	}

	// Seed a non-failed invocation and a different-agent invocation; they must
	// NOT appear in the filtered results.
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: agentID,
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusSucceeded,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_other",
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusFailed,
	})

	rec := listInvocations(t, mux,
		fmt.Sprintf("agent_id=%s&status=failed&limit=2", agentID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	resp := decodeListResponse(t, rec.Body.Bytes())

	// Envelope shape (spec contract).
	if resp.Total != 17 {
		t.Errorf("total = %d, want 17", resp.Total)
	}
	if resp.Limit != 2 {
		t.Errorf("limit = %d, want 2", resp.Limit)
	}
	if resp.Offset != 0 {
		t.Errorf("offset = %d, want 0", resp.Offset)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2", len(resp.Data))
	}

	// Item shape: every required field must be present (spec 0003 example).
	for _, item := range resp.Data {
		for _, field := range []string{"id", "agent_id", "mode", "status", "created_at"} {
			if item[field] == nil || item[field] == "" {
				t.Errorf("item field %q missing or empty; item = %v", field, item)
			}
		}
		if item["agent_id"] != agentID {
			t.Errorf("agent_id = %v, want %q", item["agent_id"], agentID)
		}
		if item["status"] != "failed" {
			t.Errorf("status = %v, want failed", item["status"])
		}
		if item["mode"] != "streaming" {
			t.Errorf("mode = %v, want streaming", item["mode"])
		}
		if item["error_class"] != "timeout" {
			t.Errorf("error_class = %v, want timeout", item["error_class"])
		}
		if item["model"] != "gpt-4o" {
			t.Errorf("model = %v, want gpt-4o", item["model"])
		}
		// Bodies must never appear on the list endpoint.
		if _, ok := item["req_body"]; ok {
			t.Error("req_body must not appear on the list endpoint")
		}
		if _, ok := item["resp_body"]; ok {
			t.Error("resp_body must not appear on the list endpoint")
		}
	}
}

func TestHandleListInvocations_401WhenNoAuth(t *testing.T) {
	t.Parallel()

	h, _ := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/invocations", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleListInvocations_EmptyResult(t *testing.T) {
	t.Parallel()

	h, _ := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := listInvocations(t, mux, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	resp := decodeListResponse(t, rec.Body.Bytes())
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
	if resp.Data == nil {
		t.Error("data must be a non-nil array, not null")
	}
	if len(resp.Data) != 0 {
		t.Errorf("len(data) = %d, want 0", len(resp.Data))
	}
}

func TestHandleListInvocations_TenantIsolation(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// Seed one invocation for testTenant and one for another tenant.
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_mine",
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusSucceeded,
	})
	seedInvocation(t, invStore, "tenant-other", &invocation.Invocation{
		AgentID: "agt_other",
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusSucceeded,
	})

	rec := listInvocations(t, mux, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	resp := decodeListResponse(t, rec.Body.Bytes())
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1 (tenant isolation breach)", resp.Total)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(resp.Data))
	}
	if resp.Data[0]["agent_id"] != "agt_mine" {
		t.Errorf("agent_id = %v, want agt_mine", resp.Data[0]["agent_id"])
	}
}

func TestHandleListInvocations_FilterByStatus(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional, Status: invocation.StatusSucceeded,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional, Status: invocation.StatusFailed,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional, Status: invocation.StatusFailed,
	})

	rec := listInvocations(t, mux, "status=failed")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeListResponse(t, rec.Body.Bytes())
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
	for _, item := range resp.Data {
		if item["status"] != "failed" {
			t.Errorf("unexpected status %v in filtered results", item["status"])
		}
	}
}

func TestHandleListInvocations_InvalidStatus400(t *testing.T) {
	t.Parallel()

	h, _ := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := listInvocations(t, mux, "status=bogus")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	var errResp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	// Must match the spec's exact error message (spec 0003 §Behavior).
	want := "invalid status: must be one of pending, running, succeeded, failed"
	if errResp["error"] != want {
		t.Errorf("error = %q, want %q", errResp["error"], want)
	}
}

func TestHandleListInvocations_FilterByMode(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional, Status: invocation.StatusSucceeded,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeStreaming, Status: invocation.StatusSucceeded,
	})

	rec := listInvocations(t, mux, "mode=streaming")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeListResponse(t, rec.Body.Bytes())
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
	if len(resp.Data) == 1 && resp.Data[0]["mode"] != "streaming" {
		t.Errorf("mode = %v, want streaming", resp.Data[0]["mode"])
	}
}

func TestHandleListInvocations_InvalidMode400(t *testing.T) {
	t.Parallel()

	h, _ := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := listInvocations(t, mux, "mode=sync")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var errResp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	want := "invalid mode: must be one of transactional, streaming"
	if errResp["error"] != want {
		t.Errorf("error = %q, want %q", errResp["error"], want)
	}
}

func TestHandleListInvocations_FilterByAgentID(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_target", Mode: invocation.ModeTransactional, Status: invocation.StatusSucceeded,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_target", Mode: invocation.ModeTransactional, Status: invocation.StatusSucceeded,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_other", Mode: invocation.ModeTransactional, Status: invocation.StatusSucceeded,
	})

	rec := listInvocations(t, mux, "agent_id=agt_target")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeListResponse(t, rec.Body.Bytes())
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
}

func TestHandleListInvocations_FilterByErrorClass(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	ec := invocation.ErrorClassTimeout
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional,
		Status: invocation.StatusFailed, ErrorClass: &ec,
	})
	ec2 := invocation.ErrorClassUpstream5xx
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional,
		Status: invocation.StatusFailed, ErrorClass: &ec2,
	})

	rec := listInvocations(t, mux, "error_class=timeout")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeListResponse(t, rec.Body.Bytes())
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
	if len(resp.Data) == 1 && resp.Data[0]["error_class"] != "timeout" {
		t.Errorf("error_class = %v, want timeout", resp.Data[0]["error_class"])
	}
}

func TestHandleListInvocations_FilterByModel(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	model1 := "gpt-4o"
	model2 := "claude-sonnet-4-6"
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional,
		Status: invocation.StatusSucceeded, Model: &model1,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional,
		Status: invocation.StatusSucceeded, Model: &model2,
	})

	rec := listInvocations(t, mux, "model=gpt-4o")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeListResponse(t, rec.Body.Bytes())
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
	if len(resp.Data) == 1 && resp.Data[0]["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", resp.Data[0]["model"])
	}
}

func TestHandleListInvocations_FilterSinceUntil(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// Seed two invocations; we'll filter with since/until to select only one.
	inv1 := seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional, Status: invocation.StatusSucceeded,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional, Status: invocation.StatusSucceeded,
	})

	// Truncate to whole-second boundary before formatting as RFC 3339, then add
	// a 1-second buffer on each side so that inv1's sub-second CreatedAt is
	// reliably within the window regardless of when the test runs.
	since := inv1.CreatedAt.Truncate(time.Second).Add(-time.Second).UTC().Format(time.RFC3339)
	until := inv1.CreatedAt.Truncate(time.Second).Add(2 * time.Second).UTC().Format(time.RFC3339)

	rec := listInvocations(t, mux,
		fmt.Sprintf("since=%s&until=%s", since, until))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeListResponse(t, rec.Body.Bytes())
	// At least 1 result expected (inv1 must be within the range).
	if resp.Total < 1 {
		t.Errorf("total = %d, want >= 1 (since/until filter included inv1)", resp.Total)
	}
}

func TestHandleListInvocations_InvalidSince400(t *testing.T) {
	t.Parallel()

	h, _ := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := listInvocations(t, mux, "since=not-a-date")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleListInvocations_InvalidUntil400(t *testing.T) {
	t.Parallel()

	h, _ := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := listInvocations(t, mux, "until=not-a-date")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleListInvocations_Pagination(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// Seed 5 invocations.
	for range 5 {
		seedInvocation(t, invStore, testTenant, &invocation.Invocation{
			AgentID: "agt_a", Mode: invocation.ModeTransactional, Status: invocation.StatusSucceeded,
		})
	}

	// Page 1: limit=2, offset=0 → 2 items, total=5.
	rec := listInvocations(t, mux, "limit=2&offset=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeListResponse(t, rec.Body.Bytes())
	if resp.Total != 5 {
		t.Errorf("total = %d, want 5", resp.Total)
	}
	if resp.Limit != 2 {
		t.Errorf("limit = %d, want 2", resp.Limit)
	}
	if resp.Offset != 0 {
		t.Errorf("offset = %d, want 0", resp.Offset)
	}
	if len(resp.Data) != 2 {
		t.Errorf("len(data) = %d, want 2", len(resp.Data))
	}

	// Page 3: offset=4 → 1 item remaining.
	rec2 := listInvocations(t, mux, "limit=2&offset=4")
	if rec2.Code != http.StatusOK {
		t.Fatalf("page3 status = %d; body = %s", rec2.Code, rec2.Body.String())
	}
	resp2 := decodeListResponse(t, rec2.Body.Bytes())
	if resp2.Total != 5 {
		t.Errorf("page3 total = %d, want 5", resp2.Total)
	}
	if len(resp2.Data) != 1 {
		t.Errorf("page3 len(data) = %d, want 1", len(resp2.Data))
	}

	// Beyond last page: offset=10 → 0 items, total still 5.
	rec3 := listInvocations(t, mux, "limit=2&offset=10")
	if rec3.Code != http.StatusOK {
		t.Fatalf("beyond status = %d; body = %s", rec3.Code, rec3.Body.String())
	}
	resp3 := decodeListResponse(t, rec3.Body.Bytes())
	if resp3.Total != 5 {
		t.Errorf("beyond total = %d, want 5", resp3.Total)
	}
	if len(resp3.Data) != 0 {
		t.Errorf("beyond len(data) = %d, want 0", len(resp3.Data))
	}
}

func TestHandleListInvocations_LimitClamped(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	for range 5 {
		seedInvocation(t, invStore, testTenant, &invocation.Invocation{
			AgentID: "agt_a", Mode: invocation.ModeTransactional, Status: invocation.StatusSucceeded,
		})
	}

	// Request limit=999 — must be silently clamped to 100.
	rec := listInvocations(t, mux, "limit=999")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeListResponse(t, rec.Body.Bytes())
	if resp.Limit != 100 {
		t.Errorf("limit = %d, want 100 (clamped from 999)", resp.Limit)
	}
}

func TestHandleListInvocations_FiltersCompose(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	const targetAgent = "agt_compose"
	ec := invocation.ErrorClassTimeout

	// Seed: 2 matching (target agent, failed, timeout), 1 non-matching (different
	// status), 1 non-matching (different agent), 1 non-matching (different error).
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: targetAgent, Mode: invocation.ModeStreaming,
		Status: invocation.StatusFailed, ErrorClass: &ec,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: targetAgent, Mode: invocation.ModeStreaming,
		Status: invocation.StatusFailed, ErrorClass: &ec,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: targetAgent, Mode: invocation.ModeTransactional,
		Status: invocation.StatusSucceeded,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_other", Mode: invocation.ModeStreaming,
		Status: invocation.StatusFailed, ErrorClass: &ec,
	})
	ec2 := invocation.ErrorClassUpstream5xx
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: targetAgent, Mode: invocation.ModeStreaming,
		Status: invocation.StatusFailed, ErrorClass: &ec2,
	})

	rec := listInvocations(t, mux,
		fmt.Sprintf("agent_id=%s&status=failed&error_class=timeout", targetAgent))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeListResponse(t, rec.Body.Bytes())
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2 (composed filters)", resp.Total)
	}
	for _, item := range resp.Data {
		if item["agent_id"] != targetAgent {
			t.Errorf("agent_id = %v, want %q", item["agent_id"], targetAgent)
		}
		if item["status"] != "failed" {
			t.Errorf("status = %v, want failed", item["status"])
		}
		if item["error_class"] != "timeout" {
			t.Errorf("error_class = %v, want timeout", item["error_class"])
		}
	}
}

func TestHandleListInvocations_FilterBySettlement(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	settled := invocation.SettlementStatusSettled
	failed := invocation.SettlementStatusSettlementFailed
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional,
		Status: invocation.StatusSucceeded, SettlementStatus: &settled,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional,
		Status: invocation.StatusFailed, SettlementStatus: &failed,
	})
	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional,
		Status: invocation.StatusSucceeded,
	})

	rec := listInvocations(t, mux, "settlement=settlement_failed")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeListResponse(t, rec.Body.Bytes())
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
	if len(resp.Data) == 1 {
		settlement, ok := resp.Data[0]["settlement"].(map[string]any)
		if !ok || settlement["status"] != "settlement_failed" {
			t.Errorf("settlement = %v, want status settlement_failed", resp.Data[0]["settlement"])
		}
	}
}

func TestHandleListInvocations_InvalidSettlement400(t *testing.T) {
	t.Parallel()

	h, _ := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := listInvocations(t, mux, "settlement=bogus")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	var errResp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	want := "invalid settlement: must be one of pending, settled, settlement_failed, settled_upstream_failed"
	if errResp["error"] != want {
		t.Errorf("error = %q, want %q", errResp["error"], want)
	}
}

// TestHandleListInvocations_SettlementBlockAbsentWhenUnset verifies list
// items with no settlement status omit the settlement block entirely (spec
// 0006 acceptance).
func TestHandleListInvocations_SettlementBlockAbsentWhenUnset(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_a", Mode: invocation.ModeTransactional, Status: invocation.StatusSucceeded,
	})

	rec := listInvocations(t, mux, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeListResponse(t, rec.Body.Bytes())
	if len(resp.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(resp.Data))
	}
	if _, ok := resp.Data[0]["settlement"]; ok {
		t.Errorf("settlement block present for an unset settlement status: %v", resp.Data[0]["settlement"])
	}
}
