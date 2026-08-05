package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/httpapi"
	"github.com/zerkerlabs/gateway/gateway/internal/policy"
)

// signalDecisionRecorder persists each captured decision to store and signals
// on ch once the write is done. It lets a test wait out the enforcement point's
// async `go Record(...)` deterministically (mirroring fakeEmitter's channel),
// while the read endpoint reads the same store back.
type signalDecisionRecorder struct {
	store policy.DecisionStore
	ch    chan struct{}
}

func (r *signalDecisionRecorder) Record(ctx context.Context, d policy.RecordedDecision) {
	_, _ = r.store.Insert(ctx, d)
	r.ch <- struct{}{}
}

// decisionListResponse mirrors the handler's policyDecisionListResponse for
// decoding in tests (the wire shape, not the unexported handler type).
type decisionListResponse struct {
	Data []struct {
		ID          string  `json:"id"`
		AgentID     string  `json:"agent_id"`
		Protocol    string  `json:"protocol"`
		MCPTool     *string `json:"mcp_tool"`
		Action      string  `json:"action"`
		MatchedRule string  `json:"matched_rule"`
		Reason      string  `json:"reason"`
		CreatedAt   string  `json:"created_at"`
	} `json:"data"`
	Limit int `json:"limit"`
}

func decodeDecisions(t *testing.T, body []byte) decisionListResponse {
	t.Helper()
	var resp decisionListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode decisions body: %v; body = %s", err, body)
	}
	return resp
}

// denyThenWarnPolicy is a two-rule document: deny delete_*, warn risky_tool.
func denyThenWarnPolicy() policy.PutFields {
	return policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionDeny, Match: policy.Match{Tools: []string{"delete_*"}}},
			{Action: policy.ActionWarn, Match: policy.Match{Tools: []string{"risky_tool"}}},
		},
	}
}

func TestPolicyDecisions_DenyAndWarnAppearOnRead(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{streamResult: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, denyThenWarnPolicy())

	decStore := policy.NewMemoryDecisionStore()
	sig := &signalDecisionRecorder{store: decStore, ch: make(chan struct{}, 8)}
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
		h.WithPolicyDecisions(decStore).WithPolicyDecisionRecorder(sig)
	})
	agentID := seedMCPAgent(t, agentStore, "https://mcp-upstream.example.com/")

	// A denied call (delete_repo → rule 1) and a warned call (risky_tool →
	// rule 2), both via the streaming path (which forwards synchronously).
	denyBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_repo"}}`)
	warnBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"risky_tool"}}`)

	denyRec := httptest.NewRecorder()
	mux.ServeHTTP(denyRec, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", denyBody, testTenant, testUser))
	if denyRec.Code != http.StatusForbidden {
		t.Fatalf("deny call status = %d, want 403; body = %s", denyRec.Code, denyRec.Body.String())
	}
	<-sig.ch // wait out the async capture

	warnRec := httptest.NewRecorder()
	mux.ServeHTTP(warnRec, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", warnBody, testTenant, testUser))
	if warnRec.Code != http.StatusOK {
		t.Fatalf("warn call status = %d, want 200; body = %s", warnRec.Code, warnRec.Body.String())
	}
	<-sig.ch

	// The read side: both decisions, newest first (warn, then deny).
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, authedGetRequest(t, "/v1/policy/decisions", testTenant, testUser))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /v1/policy/decisions status = %d, want 200; body = %s", getRec.Code, getRec.Body.String())
	}

	resp := decodeDecisions(t, getRec.Body.Bytes())
	if len(resp.Data) != 2 {
		t.Fatalf("read %d decisions, want 2 (a deny + a warn); body = %s", len(resp.Data), getRec.Body.String())
	}

	warn, deny := resp.Data[0], resp.Data[1]
	if warn.Action != "warn" || warn.MCPTool == nil || *warn.MCPTool != "risky_tool" {
		t.Errorf("newest decision = %+v, want warn on risky_tool", warn)
	}
	if deny.Action != "deny" || deny.MCPTool == nil || *deny.MCPTool != "delete_repo" {
		t.Errorf("older decision = %+v, want deny on delete_repo", deny)
	}
	// Each decision carries its matched rule + a coarse reason (spec 0009 audit
	// capture), and an id/agent.
	if deny.MatchedRule == "" {
		t.Error("deny decision has empty matched_rule, want the matched rule position")
	}
	if deny.Reason == "" {
		t.Error("deny decision has empty reason, want a coarse explanation")
	}
	if deny.ID == "" || deny.AgentID != agentID {
		t.Errorf("deny decision id/agent = %q/%q, want non-empty id and agent %q", deny.ID, deny.AgentID, agentID)
	}
}

func TestPolicyDecisions_TenantIsolation(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{streamResult: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, denyThenWarnPolicy())

	decStore := policy.NewMemoryDecisionStore()
	sig := &signalDecisionRecorder{store: decStore, ch: make(chan struct{}, 8)}
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
		h.WithPolicyDecisions(decStore).WithPolicyDecisionRecorder(sig)
	})
	agentID := seedMCPAgent(t, agentStore, "https://mcp-upstream.example.com/")

	denyBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_repo"}}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", denyBody, testTenant, testUser))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("deny call status = %d, want 403", rec.Code)
	}
	<-sig.ch

	// A different tenant must never see testTenant's decisions.
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, authedGetRequest(t, "/v1/policy/decisions", "tenant-2", testUser))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body = %s", getRec.Code, getRec.Body.String())
	}
	resp := decodeDecisions(t, getRec.Body.Bytes())
	if len(resp.Data) != 0 {
		t.Errorf("tenant-2 read %d decisions, want 0 — cross-tenant reads must not leak", len(resp.Data))
	}
}

func TestPolicyDecisions_Unauthenticated401(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{}
	mux, _, _ := policyEnforcementHandler(t, fwd, policy.NewMemoryStore(), func(h *httpapi.Handler) {
		h.WithPolicyDecisions(policy.NewMemoryDecisionStore())
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, unauthGetRequest(t, "/v1/policy/decisions"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no identity)", rec.Code)
	}
}

func TestPolicyDecisions_EmptyWhenNoCaptures(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{}
	mux, _, _ := policyEnforcementHandler(t, fwd, policy.NewMemoryStore(), func(h *httpapi.Handler) {
		h.WithPolicyDecisions(policy.NewMemoryDecisionStore())
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedGetRequest(t, "/v1/policy/decisions", testTenant, testUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeDecisions(t, rec.Body.Bytes())
	if len(resp.Data) != 0 {
		t.Errorf("read %d decisions on a fresh store, want 0", len(resp.Data))
	}
}
