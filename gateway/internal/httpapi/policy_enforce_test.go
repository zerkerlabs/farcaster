package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/farcaster/gateway/internal/httpapi"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
	"github.com/zerkerlabs/farcaster/gateway/internal/policy"
	"github.com/zerkerlabs/farcaster/gateway/internal/ratelimit"
)

// policyEnforcementHandler wires a proxy handler with fresh agent/invocation
// stores and the given policy store, returning a routed mux (spec 0009,
// ticket T4). Mirrors mcpTestHandler's shape, plus WithPolicy.
func policyEnforcementHandler(t *testing.T, fwd httpapi.ProxyForwarder, policyStore policy.PolicyStore, opts ...func(*httpapi.Handler)) (*http.ServeMux, agent.AgentStore, *invocation.MemoryStore) {
	t.Helper()
	agentStore := agent.NewMemoryStore()
	invStore := invocation.NewMemoryStore()
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore).
		WithPolicy(policyStore)
	for _, opt := range opts {
		opt(h)
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux, agentStore, invStore
}

// putPolicy seeds testTenant's policy document via store.Put, failing the
// test on error. Scoped to testTenant by design, mirroring waitForStatus —
// every current caller passes testTenant, and unparam flags an
// always-constant argument.
func putPolicy(t *testing.T, store policy.PolicyStore, fields policy.PutFields) {
	t.Helper()
	if _, err := store.Put(context.Background(), testTenant, fields); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
}

// erroringPolicyStore is a policy.PolicyStore whose Get always fails with err
// — distinct from policy.ErrNotFound ("no policy configured"), it simulates a
// genuine store outage while evaluating the PEP's on_error handling.
type erroringPolicyStore struct{ err error }

func (s erroringPolicyStore) Get(context.Context, string) (*policy.Policy, error) {
	return nil, s.err
}

func (s erroringPolicyStore) Put(context.Context, string, policy.PutFields) (*policy.Policy, error) {
	return nil, s.err
}

// invalidActionEvaluator is a policy.Evaluator that always returns an Action
// the PEP does not recognize, exercising the "engine errors" half of the
// on_error posture (spec 0009 Decision 3) with a policy that *did* load
// successfully.
type invalidActionEvaluator struct{}

func (invalidActionEvaluator) Evaluate(*policy.Policy, policy.RequestContext) policy.Decision {
	return policy.Decision{Action: policy.Action("not-a-real-action")}
}

// invalidVerdictClassifierClient is a policy.ClassifierClient stub that
// returns an Action the PEP does not recognize without going through
// httpClassifierClient's own validation — exercising the defense-in-depth
// "malformed verdict" half of invokeClassifier's on_error fallback directly
// against the ClassifierClient interface (T6 acceptance: "malformed →
// on_error").
type invalidVerdictClassifierClient struct{}

func (invalidVerdictClassifierClient) Classify(context.Context, string, policy.ClassifierRequest) (policy.ClassifierVerdict, error) {
	return policy.ClassifierVerdict{Action: policy.Action("not-a-real-action")}, nil
}

// classifierWebhook starts an httptest.Server that always returns verdict,
// returning the *http.Server and a ClassifierClient wired to it (bypassing
// the SSRF guard the way srv.Client() does throughout x402_settle_test.go).
func classifierWebhook(t *testing.T, verdict policy.ClassifierVerdict) (*httptest.Server, policy.ClassifierClient) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(verdict)
	}))
	t.Cleanup(srv.Close)
	return srv, httpapi.NewHTTPClassifierClient(srv.Client())
}

// neverCalledClassifierClient fails the test if Classify is ever invoked —
// used to pin down "pure-deterministic policies make zero external calls"
// (spec 0009 T6 acceptance) when no rule in the policy sets Classifier.
type neverCalledClassifierClient struct{ t *testing.T }

func (c neverCalledClassifierClient) Classify(context.Context, string, policy.ClassifierRequest) (policy.ClassifierVerdict, error) {
	c.t.Helper()
	c.t.Fatal("classifier webhook called for a policy with no Classifier-opted-in rule")
	return policy.ClassifierVerdict{}, nil
}

func decodeErrorBody(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode error body: %v; body = %s", err, body)
	}
	return m
}

// ---------------------------------------------------------- handleTransact ---

func TestHandleTransact_PolicyNoDocumentNoChange(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	mux, agentStore, invStore := policyEnforcementHandler(t, fwd, policy.NewMemoryStore())
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{"q":"x"}`), testTenant, testUser))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Farcaster-Policy-Warning"); got != "" {
		t.Errorf("X-Farcaster-Policy-Warning = %q, want empty (no policy configured)", got)
	}
	invID := rec.Header().Get("X-Farcaster-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)
}

func TestHandleTransact_PolicyDeny(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionDeny, Match: policy.Match{Tools: []string{"delete_*"}}},
		},
	})
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, store)
	agentID := seedMCPAgent(t, agentStore, "https://mcp-upstream.example.com/")

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_repo"}}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, body, testTenant, testUser))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Farcaster-Invocation-ID"); got != "" {
		t.Errorf("X-Farcaster-Invocation-ID = %q, want empty (denied before invocation creation)", got)
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if got["error"] != "denied by policy" {
		t.Errorf(`error = %q, want "denied by policy"`, got["error"])
	}
	if got["reason"] == "" {
		t.Error("reason is empty, want a coarse explanation")
	}
	if fwd.wasCalled() {
		t.Error("forwarder was called; denied calls must never be forwarded")
	}
}

func TestHandleTransact_PolicyWarnForwards(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionWarn, Match: policy.Match{Tools: []string{"risky_tool"}}},
		},
	})
	mux, agentStore, invStore := policyEnforcementHandler(t, fwd, store)
	agentID := seedMCPAgent(t, agentStore, "https://mcp-upstream.example.com/")

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"risky_tool"}}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, body, testTenant, testUser))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Farcaster-Policy-Warning"); got == "" {
		t.Error("X-Farcaster-Policy-Warning header missing, want a warning on a matched warn rule")
	}
	invID := rec.Header().Get("X-Farcaster-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)
	// The transactional path runs Do in a background goroutine (issue #53);
	// waiting for the invocation to reach a terminal state guarantees Do has
	// already returned before this check.
	if !fwd.wasCalled() {
		t.Error("forwarder was not called; a warn decision must forward like allow")
	}
}

func TestHandleTransact_PolicyDefaultApplied(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, policy.PutFields{
		Default: policy.ActionDeny,
		OnError: policy.ActionDeny,
	})
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, store)
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no rule matched, default=deny); body = %s", rec.Code, rec.Body.String())
	}
	if fwd.wasCalled() {
		t.Error("forwarder was called; a default-deny call must never be forwarded")
	}
}

func TestHandleTransact_PolicyStoreErrorReturns500(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, erroringPolicyStore{err: errors.New("db unavailable")})
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	if fwd.wasCalled() {
		t.Error("forwarder was called; a policy store failure must not forward the call")
	}
}

func TestHandleTransact_PolicyEngineErrorAppliesOnError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		onError  policy.Action
		wantCode int
		wantFwd  bool
	}{
		{"on_error deny", policy.ActionDeny, http.StatusForbidden, false},
		{"on_error allow", policy.ActionAllow, http.StatusAccepted, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
			store := policy.NewMemoryStore()
			putPolicy(t, store, policy.PutFields{Default: policy.ActionAllow, OnError: tt.onError})
			mux, agentStore, invStore := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
				h.WithPolicyEvaluator(invalidActionEvaluator{})
			})
			agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantCode, rec.Body.String())
			}
			// The transactional path runs Do in a background goroutine (issue
			// #53); on a forwarded call, wait for the invocation to reach a
			// terminal state before checking wasCalled so the check cannot
			// race the goroutine.
			if tt.wantFwd {
				invID := rec.Header().Get("X-Farcaster-Invocation-ID")
				waitForStatus(t, invStore, invID, invocation.StatusSucceeded)
			}
			if got := fwd.wasCalled(); got != tt.wantFwd {
				t.Errorf("forwarder called = %v, want %v", got, tt.wantFwd)
			}
		})
	}
}

// ------------------------------------------------------------ handleStream ---

func TestHandleStream_PolicyNoDocumentNoChange(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{streamResult: fakeResult(200, `{"ok":true}`)}
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, policy.NewMemoryStore())
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Farcaster-Policy-Warning"); got != "" {
		t.Errorf("X-Farcaster-Policy-Warning = %q, want empty (no policy configured)", got)
	}
}

func TestHandleStream_PolicyDeny(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{streamResult: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionDeny, Match: policy.Match{Tools: []string{"delete_*"}}},
		},
	})
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, store)
	agentID := seedMCPAgent(t, agentStore, "https://mcp-upstream.example.com/")

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_repo"}}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", body, testTenant, testUser))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Farcaster-Invocation-ID"); got != "" {
		t.Errorf("X-Farcaster-Invocation-ID = %q, want empty (denied before invocation creation)", got)
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if got["error"] != "denied by policy" {
		t.Errorf(`error = %q, want "denied by policy"`, got["error"])
	}
	if fwd.wasCalled() {
		t.Error("forwarder was called; denied calls must never be forwarded")
	}
}

func TestHandleStream_PolicyWarnForwards(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{streamResult: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionWarn, Match: policy.Match{Tools: []string{"risky_tool"}}},
		},
	})
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, store)
	agentID := seedMCPAgent(t, agentStore, "https://mcp-upstream.example.com/")

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"risky_tool"}}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", body, testTenant, testUser))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Farcaster-Policy-Warning"); got == "" {
		t.Error("X-Farcaster-Policy-Warning header missing, want a warning on a matched warn rule")
	}
	if !fwd.wasCalled() {
		t.Error("forwarder was not called; a warn decision must forward like allow")
	}
}

func TestHandleStream_PolicyDefaultApplied(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{streamResult: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, policy.PutFields{
		Default: policy.ActionDeny,
		OnError: policy.ActionDeny,
	})
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, store)
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no rule matched, default=deny); body = %s", rec.Code, rec.Body.String())
	}
	if fwd.wasCalled() {
		t.Error("forwarder was called; a default-deny call must never be forwarded")
	}
}

func TestHandleStream_PolicyStoreErrorReturns500(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{streamResult: fakeResult(200, `{"ok":true}`)}
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, erroringPolicyStore{err: errors.New("db unavailable")})
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	if fwd.wasCalled() {
		t.Error("forwarder was called; a policy store failure must not forward the call")
	}
}

func TestHandleStream_PolicyEngineErrorAppliesOnError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		onError  policy.Action
		wantCode int
		wantFwd  bool
	}{
		{"on_error deny", policy.ActionDeny, http.StatusForbidden, false},
		{"on_error allow", policy.ActionAllow, http.StatusOK, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fwd := &mockForwarder{streamResult: fakeResult(200, `{"ok":true}`)}
			store := policy.NewMemoryStore()
			putPolicy(t, store, policy.PutFields{Default: policy.ActionAllow, OnError: tt.onError})
			mux, agentStore, _ := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
				h.WithPolicyEvaluator(invalidActionEvaluator{})
			})
			agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser))

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantCode, rec.Body.String())
			}
			if got := fwd.wasCalled(); got != tt.wantFwd {
				t.Errorf("forwarder called = %v, want %v", got, tt.wantFwd)
			}
		})
	}
}

// TestHandleTransact_PolicyRateRuleFiresOnLiveTraffic proves a rate_per_min
// rule actually enforces on live traffic (spec 0009 fast-follow, #212): with
// a real *ratelimit.ObservedRateTracker wired in via WithRateObserver, the
// caller's observed rate climbs with each request through the proxy path,
// and a deny rule fires once it exceeds the configured threshold — replacing
// the retired TestHandleTransact_PolicyRateRuleNeverFiresYet, which pinned
// down the pre-fast-follow gap this test now closes.
func TestHandleTransact_PolicyRateRuleFiresOnLiveTraffic(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	limit := 1
	putPolicy(t, store, policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionDeny, Match: policy.Match{RatePerMin: &limit}},
		},
	})
	observer := ratelimit.NewObservedRateTracker(ratelimit.DefaultTTL)
	t.Cleanup(observer.Close)
	mux, agentStore, invStore := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
		h.WithRateObserver(observer)
	})
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

	// 1st call: observed rate is 1, not > the limit (1) — allowed through.
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("1st call status = %d, want 202; body = %s", rec1.Code, rec1.Body.String())
	}
	invID1 := rec1.Header().Get("X-Farcaster-Invocation-ID")
	waitForStatus(t, invStore, invID1, invocation.StatusSucceeded)
	if got := fwd.callCount(); got != 1 {
		t.Fatalf("forwarder called %d times after 1st call, want 1", got)
	}

	// 2nd call within the same window: observed rate is now 2 (> limit 1), so
	// the deny rule fires on real observed-rate data sourced from
	// internal/ratelimit rather than the RatePerMin zero value.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("2nd call status = %d, want 403 (rate_per_min rule must fire on live traffic); body = %s", rec2.Code, rec2.Body.String())
	}
	if got := rec2.Header().Get("X-Farcaster-Invocation-ID"); got != "" {
		t.Errorf("X-Farcaster-Invocation-ID = %q, want empty (denied before invocation creation)", got)
	}
	if got := fwd.callCount(); got != 1 {
		t.Errorf("forwarder called %d times after 2nd call, want 1 (denied call must never be forwarded)", got)
	}
}

// TestHandleStream_PolicyRateRuleFiresOnLiveTraffic mirrors
// TestHandleTransact_PolicyRateRuleFiresOnLiveTraffic for the streaming path,
// confirming handleStream sources RatePerMin from the same observer (spec
// 0009 fast-follow, #212 acceptance: "in both handleTransact and
// handleStream").
func TestHandleStream_PolicyRateRuleFiresOnLiveTraffic(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{streamResult: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	limit := 1
	putPolicy(t, store, policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionDeny, Match: policy.Match{RatePerMin: &limit}},
		},
	})
	observer := ratelimit.NewObservedRateTracker(ratelimit.DefaultTTL)
	t.Cleanup(observer.Close)
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
		h.WithRateObserver(observer)
	})
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")

	// 1st call: observed rate is 1, not > the limit (1) — allowed through.
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser))
	if rec1.Code != http.StatusOK {
		t.Fatalf("1st call status = %d, want 200; body = %s", rec1.Code, rec1.Body.String())
	}
	if got := fwd.callCount(); got != 1 {
		t.Fatalf("forwarder called %d times after 1st call, want 1", got)
	}

	// 2nd call within the same window: observed rate is now 2 (> limit 1), so
	// the deny rule fires.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("2nd call status = %d, want 403 (rate_per_min rule must fire on live traffic); body = %s", rec2.Code, rec2.Body.String())
	}
	if got := fwd.callCount(); got != 1 {
		t.Errorf("forwarder called %d times after 2nd call, want 1 (denied call must never be forwarded)", got)
	}
}

// TestHandleStream_MaxBodyBytesUnknownContentLengthMatches covers the fix for
// the streaming path's max_body_bytes bypass: a chunked/unknown-length
// request (r.ContentLength == -1) is treated as arbitrarily large rather than
// as size 0, so a max_body_bytes deny rule still fires instead of being
// silently bypassable by omitting Content-Length.
func TestHandleStream_MaxBodyBytesUnknownContentLengthMatches(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{streamResult: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	threshold := int64(1)
	putPolicy(t, store, policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionDeny, Match: policy.Match{MaxBodyBytes: &threshold}},
		},
	})
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, store)
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")

	body := []byte(`{"some":"payload"}`)
	// io.NopCloser(bytes.NewReader(...)) defeats httptest.NewRequest's usual
	// ContentLength inference from a *bytes.Reader, producing ContentLength
	// == -1 — the same "unknown length" shape a chunked request has.
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/"+agentID+"/stream", io.NopCloser(bytes.NewReader(body)))
	req = req.WithContext(authtest.WithIdentity(req.Context(), testTenant, testUser))
	if req.ContentLength != -1 {
		t.Fatalf("test setup: req.ContentLength = %d, want -1 (unknown)", req.ContentLength)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (unknown-length body must satisfy max_body_bytes); body = %s", rec.Code, rec.Body.String())
	}
	if fwd.wasCalled() {
		t.Error("forwarder was called; denied calls must never be forwarded")
	}
}

// -------------------------------------------------- T6 classifier hook ---

// classifierRule returns a single-rule policy that opts into a classifier
// hook at hookURL for any call (empty Match matches everything). Action is
// set to warn; the classifier hook's own verdict takes over when it matches
// (spec 0009 Decision 4), so this value is never actually applied.
func classifierRule(hookURL string) policy.PutFields {
	return policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionWarn, Match: policy.Match{}, Classifier: &policy.ClassifierHook{URL: hookURL}},
		},
	}
}

func TestHandleTransact_PolicyClassifierAllow(t *testing.T) {
	t.Parallel()

	srv, client := classifierWebhook(t, policy.ClassifierVerdict{Action: policy.ActionAllow})

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, classifierRule(srv.URL))
	mux, agentStore, invStore := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
		h.WithClassifierClient(client)
	})
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Farcaster-Policy-Warning"); got != "" {
		t.Errorf("X-Farcaster-Policy-Warning = %q, want empty (classifier verdict was allow)", got)
	}
	invID := rec.Header().Get("X-Farcaster-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)
	if !fwd.wasCalled() {
		t.Error("forwarder was not called; a classifier allow verdict must forward like allow")
	}
}

func TestHandleTransact_PolicyClassifierWarn(t *testing.T) {
	t.Parallel()

	srv, client := classifierWebhook(t, policy.ClassifierVerdict{Action: policy.ActionWarn, Reason: "borderline content"})

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, classifierRule(srv.URL))
	mux, agentStore, invStore := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
		h.WithClassifierClient(client)
	})
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Farcaster-Policy-Warning"); got == "" {
		t.Error("X-Farcaster-Policy-Warning header missing, want a warning from the classifier verdict")
	}
	invID := rec.Header().Get("X-Farcaster-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)
	if !fwd.wasCalled() {
		t.Error("forwarder was not called; a classifier warn verdict must forward like allow")
	}
}

func TestHandleTransact_PolicyClassifierDeny(t *testing.T) {
	t.Parallel()

	srv, client := classifierWebhook(t, policy.ClassifierVerdict{Action: policy.ActionDeny, Reason: "unsafe content"})

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, classifierRule(srv.URL))
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
		h.WithClassifierClient(client)
	})
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if fwd.wasCalled() {
		t.Error("forwarder was called; a classifier deny verdict must never be forwarded")
	}
}

func TestHandleTransact_PolicyClassifierInvalidVerdictAppliesOnError(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	fields := classifierRule("https://classifier.example.com/")
	fields.OnError = policy.ActionDeny
	putPolicy(t, store, fields)
	mux, agentStore, _ := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
		h.WithClassifierClient(invalidVerdictClassifierClient{})
	})
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (invalid verdict action must apply on_error); body = %s", rec.Code, rec.Body.String())
	}
	if fwd.wasCalled() {
		t.Error("forwarder was called; an invalid classifier verdict must apply on_error, never be forwarded as-is")
	}
}

// slowClassifierServer starts an httptest.Server whose handler sleeps past a
// short client timeout before responding, and returns a ClassifierClient
// wired to it with that short timeout — exercising the "best-effort: timeout
// falls back to on_error" path (spec 0009 Decision 4) without a real,
// test-suite-slowing wait on defaultClassifierTimeout.
func slowClassifierServer(t *testing.T) (*httptest.Server, policy.ClassifierClient) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(policy.ClassifierVerdict{Action: policy.ActionDeny})
	}))
	t.Cleanup(srv.Close)
	fastClient := *srv.Client()
	fastClient.Timeout = 10 * time.Millisecond
	return srv, httpapi.NewHTTPClassifierClient(&fastClient)
}

func TestHandleTransact_PolicyClassifierTimeoutAppliesOnError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		onError  policy.Action
		wantCode int
		wantFwd  bool
	}{
		{"on_error deny", policy.ActionDeny, http.StatusForbidden, false},
		{"on_error allow", policy.ActionAllow, http.StatusAccepted, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, client := slowClassifierServer(t)

			fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
			store := policy.NewMemoryStore()
			fields := classifierRule(srv.URL)
			fields.OnError = tt.onError
			putPolicy(t, store, fields)
			mux, agentStore, invStore := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
				h.WithClassifierClient(client)
			})
			agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantFwd {
				invID := rec.Header().Get("X-Farcaster-Invocation-ID")
				waitForStatus(t, invStore, invID, invocation.StatusSucceeded)
			}
			if got := fwd.wasCalled(); got != tt.wantFwd {
				t.Errorf("forwarder called = %v, want %v", got, tt.wantFwd)
			}
		})
	}
}

// TestHandleTransact_PolicyNoClassifierRuleMakesZeroExternalCalls pins down
// the T6 acceptance guarantee: a policy with no rule opting into a Classifier
// hook must never call out, even when h.classifierClient is wired to a client
// that would fail the test if invoked.
func TestHandleTransact_PolicyNoClassifierRuleMakesZeroExternalCalls(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	store := policy.NewMemoryStore()
	putPolicy(t, store, policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionWarn, Match: policy.Match{Tools: []string{"risky_tool"}}},
		},
	})
	mux, agentStore, invStore := policyEnforcementHandler(t, fwd, store, func(h *httpapi.Handler) {
		h.WithClassifierClient(neverCalledClassifierClient{t: t})
	})
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	invID := rec.Header().Get("X-Farcaster-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)
}
