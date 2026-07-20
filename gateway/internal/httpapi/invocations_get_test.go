package httpapi_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/farcaster/gateway/internal/httpapi"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
)

// newGetInvocationHandler wires a Handler with only an invocation store
// (no agent store or forwarder required for the get endpoint).
func newGetInvocationHandler(t *testing.T) (*httpapi.Handler, *invocation.MemoryStore) {
	t.Helper()
	return newInvocationListHandler(t)
}

// getInvocation issues GET /v1/invocations/{id} for testTenant and returns
// the recorder. Tests that need a different identity should build requests
// directly.
func getInvocation(t *testing.T, mux *http.ServeMux, id string, scopes []string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/invocations/"+id, nil)
	ctx := authtest.WithIdentity(req.Context(), testTenant, testUser)
	if len(scopes) > 0 {
		ctx = authtest.WithScopes(ctx, scopes)
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// decodeDetailResponse unmarshals the body into a map so tests can check
// presence/absence of optional fields.
func decodeDetailResponse(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode detail response: %v\nbody: %s", err, body)
	}
	return m
}

// TestHandleGetInvocation_SpecExample is the contract test matching the spec's
// request/response example (docs/specs/0003-observability.md §Request/Response):
//
//	GET /v1/invocations/inv_018f3a...
//
// With body capture on and the invocations:read_body scope the response must
// include body_captured:true and base64-encoded req_body / resp_body.
func TestHandleGetInvocation_SpecExample(t *testing.T) {
	t.Parallel()

	h, invStore := newGetInvocationHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	reqBodyRaw := []byte(`{"prompt":"hello"}`)
	respBodyRaw := []byte(`{"text":"hi"}`)
	upstreamStatus := 200
	latency := int64(842)
	model := "gpt-4o"

	inv := &invocation.Invocation{
		AgentID:        "agt_018f3a000000",
		Mode:           invocation.ModeTransactional,
		Status:         invocation.StatusSucceeded,
		UpstreamStatus: &upstreamStatus,
		LatencyMS:      &latency,
		RequestBody:    reqBodyRaw,
		ResponseBody:   respBodyRaw,
		Model:          &model,
	}
	seedInvocation(t, invStore, testTenant, inv)

	rec := getInvocation(t, mux, inv.ID, []string{"invocations:read_body"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	resp := decodeDetailResponse(t, rec.Body.Bytes())

	// Required fields.
	for _, field := range []string{"id", "agent_id", "mode", "status", "created_at", "body_captured"} {
		if resp[field] == nil {
			t.Errorf("field %q missing from response", field)
		}
	}
	if resp["id"] != inv.ID {
		t.Errorf("id = %v, want %q", resp["id"], inv.ID)
	}
	if resp["mode"] != "transactional" {
		t.Errorf("mode = %v, want transactional", resp["mode"])
	}
	if resp["status"] != "succeeded" {
		t.Errorf("status = %v, want succeeded", resp["status"])
	}
	if resp["body_captured"] != true {
		t.Errorf("body_captured = %v, want true", resp["body_captured"])
	}
	if resp["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", resp["model"])
	}

	// Bodies must be base64-encoded and match the raw bytes.
	wantReq := base64.StdEncoding.EncodeToString(reqBodyRaw)
	if resp["req_body"] != wantReq {
		t.Errorf("req_body = %v, want %q", resp["req_body"], wantReq)
	}
	wantResp := base64.StdEncoding.EncodeToString(respBodyRaw)
	if resp["resp_body"] != wantResp {
		t.Errorf("resp_body = %v, want %q", resp["resp_body"], wantResp)
	}
}

// TestHandleGetInvocation_NoScope_BodiesOmitted verifies the spec acceptance
// criterion "token without the scope → metadata only, bodies omitted, even
// when captured" (spec 0003 §Acceptance).
func TestHandleGetInvocation_NoScope_BodiesOmitted(t *testing.T) {
	t.Parallel()

	h, invStore := newGetInvocationHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	inv := &invocation.Invocation{
		AgentID:      "agt_a",
		Mode:         invocation.ModeTransactional,
		Status:       invocation.StatusSucceeded,
		RequestBody:  []byte(`{"q":"test"}`),
		ResponseBody: []byte(`{"a":"ok"}`),
	}
	seedInvocation(t, invStore, testTenant, inv)

	// No scopes — token does NOT carry invocations:read_body.
	rec := getInvocation(t, mux, inv.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	resp := decodeDetailResponse(t, rec.Body.Bytes())

	// body_captured must be true because capture was on.
	if resp["body_captured"] != true {
		t.Errorf("body_captured = %v, want true (capture was on)", resp["body_captured"])
	}
	// Bodies must be absent — scope is missing.
	if _, ok := resp["req_body"]; ok {
		t.Error("req_body must not appear when caller lacks invocations:read_body scope")
	}
	if _, ok := resp["resp_body"]; ok {
		t.Error("resp_body must not appear when caller lacks invocations:read_body scope")
	}
}

// TestHandleGetInvocation_WithScope_CapturedOff verifies that bodies are
// omitted when capture was off, even when the caller holds the scope (spec
// 0003 §Behavior — "only when capture was on for that row").
func TestHandleGetInvocation_WithScope_CapturedOff(t *testing.T) {
	t.Parallel()

	h, invStore := newGetInvocationHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	inv := &invocation.Invocation{
		AgentID: "agt_a",
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusSucceeded,
		// RequestBody and ResponseBody are nil — capture was off.
	}
	seedInvocation(t, invStore, testTenant, inv)

	rec := getInvocation(t, mux, inv.ID, []string{"invocations:read_body"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	resp := decodeDetailResponse(t, rec.Body.Bytes())

	if resp["body_captured"] != false {
		t.Errorf("body_captured = %v, want false (capture was off)", resp["body_captured"])
	}
	if _, ok := resp["req_body"]; ok {
		t.Error("req_body must not appear when capture was off")
	}
	if _, ok := resp["resp_body"]; ok {
		t.Error("resp_body must not appear when capture was off")
	}
}

// TestHandleGetInvocation_CrossTenant404 verifies that an ID belonging to
// another tenant returns 404, not 403 (spec 0003 §Behavior; AGENTS.md
// invariant #2 — no cross-tenant data, even in error bodies).
func TestHandleGetInvocation_CrossTenant404(t *testing.T) {
	t.Parallel()

	h, invStore := newGetInvocationHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// Seed an invocation under a different tenant.
	other := &invocation.Invocation{
		AgentID: "agt_other",
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusSucceeded,
	}
	seedInvocation(t, invStore, "tenant-other", other)

	// Request as testTenant — must not see tenant-other's record.
	rec := getInvocation(t, mux, other.ID, []string{"invocations:read_body"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for cross-tenant id", rec.Code)
	}
}

// TestHandleGetInvocation_NotFound returns 404 for an unknown id.
func TestHandleGetInvocation_NotFound(t *testing.T) {
	t.Parallel()

	h, _ := newGetInvocationHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := getInvocation(t, mux, "inv_doesnotexist", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestHandleGetInvocation_401WhenNoAuth verifies that the endpoint rejects
// requests without an authenticated context.
func TestHandleGetInvocation_401WhenNoAuth(t *testing.T) {
	t.Parallel()

	h, invStore := newGetInvocationHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	inv := &invocation.Invocation{
		AgentID: "agt_a",
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusSucceeded,
	}
	seedInvocation(t, invStore, testTenant, inv)

	req := httptest.NewRequest(http.MethodGet, "/v1/invocations/"+inv.ID, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestHandleGetInvocation_BodyCap verifies that bodies larger than 1 MiB are
// silently truncated in the base64 output (spec 0003 §4 — size-capped).
func TestHandleGetInvocation_BodyCap(t *testing.T) {
	t.Parallel()

	h, invStore := newGetInvocationHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	const cap = 1 << 20 // must match bodyCapBytes in invocations.go

	// Body larger than the cap.
	bigBody := make([]byte, cap+1024)
	for i := range bigBody {
		bigBody[i] = byte(i % 256)
	}
	inv := &invocation.Invocation{
		AgentID:      "agt_a",
		Mode:         invocation.ModeTransactional,
		Status:       invocation.StatusSucceeded,
		RequestBody:  bigBody,
		ResponseBody: bigBody,
	}
	seedInvocation(t, invStore, testTenant, inv)

	rec := getInvocation(t, mux, inv.ID, []string{"invocations:read_body"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	resp := decodeDetailResponse(t, rec.Body.Bytes())

	wantEncoded := base64.StdEncoding.EncodeToString(bigBody[:cap])
	if resp["req_body"] != wantEncoded {
		t.Errorf("req_body not capped correctly: len(encoded)=%d, want len=%d",
			len(resp["req_body"].(string)), len(wantEncoded))
	}
	if resp["resp_body"] != wantEncoded {
		t.Errorf("resp_body not capped correctly: len(encoded)=%d, want len=%d",
			len(resp["resp_body"].(string)), len(wantEncoded))
	}
}

// TestHandleGetInvocation_MetadataShape verifies that all required metadata
// fields are present in the response regardless of scope or capture state.
func TestHandleGetInvocation_MetadataShape(t *testing.T) {
	t.Parallel()

	h, invStore := newGetInvocationHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	ec := invocation.ErrorClassTimeout
	inv := &invocation.Invocation{
		AgentID:    "agt_a",
		Mode:       invocation.ModeStreaming,
		Status:     invocation.StatusFailed,
		ErrorClass: &ec,
	}
	seedInvocation(t, invStore, testTenant, inv)

	rec := getInvocation(t, mux, inv.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	resp := decodeDetailResponse(t, rec.Body.Bytes())

	// All metadata fields must be present (spec 0003 §Surface — always returned).
	for _, field := range []string{
		"id", "agent_id", "mode", "status",
		"body_captured", "created_at",
	} {
		if resp[field] == nil {
			t.Errorf("metadata field %q missing from response", field)
		}
	}
	if resp["error_class"] != "timeout" {
		t.Errorf("error_class = %v, want timeout", resp["error_class"])
	}
	if resp["mode"] != "streaming" {
		t.Errorf("mode = %v, want streaming", resp["mode"])
	}
}

// TestHandleGetInvocation_SettlementBlockAbsentWhenUnset verifies that
// unpriced/gate-only invocations (no settlement status set) omit the
// settlement block entirely, rather than returning it with all-null fields
// (spec 0006 acceptance).
func TestHandleGetInvocation_SettlementBlockAbsentWhenUnset(t *testing.T) {
	t.Parallel()

	h, invStore := newGetInvocationHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	inv := &invocation.Invocation{
		AgentID: "agt_a",
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusSucceeded,
	}
	seedInvocation(t, invStore, testTenant, inv)

	rec := getInvocation(t, mux, inv.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	resp := decodeDetailResponse(t, rec.Body.Bytes())
	if _, ok := resp["settlement"]; ok {
		t.Errorf("settlement block present for an unset settlement status: %v", resp["settlement"])
	}
}

// TestHandleGetInvocation_SettlementBlockPresent verifies the settlement
// sub-record (the canonical receipt, spec 0006) is exposed on the detail
// read with its full field set.
func TestHandleGetInvocation_SettlementBlockPresent(t *testing.T) {
	t.Parallel()

	h, invStore := newGetInvocationHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	status := invocation.SettlementStatusSettled
	txHash := "0xabc..."
	settledAmount := "10000"
	operatorAmount := "10000"
	facilitatorFee := "0"
	attempts := 1
	settledAt := time.Now().UTC().Truncate(time.Second)

	inv := &invocation.Invocation{
		AgentID:            "agt_a",
		Mode:               invocation.ModeTransactional,
		Status:             invocation.StatusSucceeded,
		SettlementStatus:   &status,
		SettlementTxHash:   &txHash,
		SettledAmount:      &settledAmount,
		OperatorAmount:     &operatorAmount,
		FacilitatorFee:     &facilitatorFee,
		SettlementAttempts: &attempts,
		SettledAt:          &settledAt,
	}
	seedInvocation(t, invStore, testTenant, inv)

	rec := getInvocation(t, mux, inv.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	resp := decodeDetailResponse(t, rec.Body.Bytes())
	settlement, ok := resp["settlement"].(map[string]any)
	if !ok {
		t.Fatalf("settlement block missing or wrong shape; resp = %v", resp)
	}
	if settlement["status"] != "settled" {
		t.Errorf("settlement.status = %v, want settled", settlement["status"])
	}
	if settlement["tx_hash"] != "0xabc..." {
		t.Errorf("settlement.tx_hash = %v, want 0xabc...", settlement["tx_hash"])
	}
	if settlement["settled_amount"] != "10000" {
		t.Errorf("settlement.settled_amount = %v, want 10000", settlement["settled_amount"])
	}
	if settlement["operator_amount"] != "10000" {
		t.Errorf("settlement.operator_amount = %v, want 10000", settlement["operator_amount"])
	}
	if settlement["facilitator_fee"] != "0" {
		t.Errorf("settlement.facilitator_fee = %v, want 0", settlement["facilitator_fee"])
	}
	if settlement["attempts"] != float64(1) {
		t.Errorf("settlement.attempts = %v, want 1", settlement["attempts"])
	}
	if _, ok := settlement["reason"]; ok {
		t.Errorf("settlement.reason present when unset: %v", settlement["reason"])
	}
}
