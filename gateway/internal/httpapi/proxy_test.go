package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/farcaster/gateway/internal/httpapi"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
	"github.com/zerkerlabs/farcaster/gateway/internal/proxy"
	"github.com/zerkerlabs/farcaster/gateway/internal/ratelimit"
	"github.com/zerkerlabs/farcaster/gateway/internal/receipt"
)

// mockForwarder is a test double for httpapi.ProxyForwarder.
type mockForwarder struct {
	result       *proxy.Result
	err          error
	streamResult *proxy.Result
	streamErr    error

	// mu guards called and calls, which Do/DoStream set from the request
	// goroutine (the transactional path runs them in a background goroutine —
	// issue #53) and tests read from the test goroutine via wasCalled/
	// callCount (spec 0009 ticket T4's deny tests: proving the forwarder was
	// never reached; the fast-follow rate-rule tests: proving it was reached
	// exactly once before a rate limit kicked in).
	mu     sync.Mutex
	called bool
	calls  int
}

func (m *mockForwarder) Do(_ context.Context, _, _, _ string, _ *http.Request) (*proxy.Result, error) {
	m.mu.Lock()
	m.called = true
	m.calls++
	m.mu.Unlock()
	return m.result, m.err
}

// DoStream falls back to the Do result/err if no stream-specific values are set.
func (m *mockForwarder) DoStream(_ context.Context, _, _, _ string, _ *http.Request) (*proxy.Result, error) {
	m.mu.Lock()
	m.called = true
	m.calls++
	m.mu.Unlock()
	if m.streamResult != nil || m.streamErr != nil {
		return m.streamResult, m.streamErr
	}
	return m.result, m.err
}

// wasCalled reports whether Do or DoStream has been invoked.
func (m *mockForwarder) wasCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.called
}

// callCount reports how many times Do or DoStream has been invoked in total.
func (m *mockForwarder) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// fakeResult returns a proxy.Result with the given status and body for use in
// tests. The caller does not need to close the body.
func fakeResult(status int, body string) *proxy.Result {
	return &proxy.Result{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		LatencyMS:  42,
	}
}

// newProxyHandler returns a Handler wired with a fresh MemoryStore and
// MemoryInvocationStore. The agent store is pre-populated by setup (optional).
func newProxyHandler(
	t *testing.T,
	setup func(agent.AgentStore),
	fwd httpapi.ProxyForwarder,
) (*httpapi.Handler, agent.AgentStore) {
	t.Helper()
	agentStore := agent.NewMemoryStore()
	if setup != nil {
		setup(agentStore)
	}
	invStore := invocation.NewMemoryStore()
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)
	return h, agentStore
}

// authedPostRequest returns a POST request to path with the given body bytes
// and auth context pre-populated.
func authedPostRequest(t *testing.T, path string, body []byte, tenant, user string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(authtest.WithIdentity(req.Context(), tenant, user))
}

// authedGETRequest returns a GET request to path with the auth context set.
func authedGETRequest(t *testing.T, path, tenant, user string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	return req.WithContext(authtest.WithIdentity(req.Context(), tenant, user))
}

// seedActiveAgent creates an agent in testTenant with the given upstream URL and returns its ID.
func seedActiveAgent(t *testing.T, store agent.AgentStore, upstreamURL string) string {
	t.Helper()
	a := &agent.Agent{Name: "proxy-test-agent", CreatedBy: testUser, UpstreamURL: &upstreamURL}
	if err := store.Create(context.Background(), testTenant, a); err != nil {
		t.Fatalf("seedActiveAgent: %v", err)
	}
	return a.ID
}

// seedPendingAgent creates an agent with no upstream URL (status pending) and returns its ID.
func seedPendingAgent(t *testing.T, store agent.AgentStore, tenant string) string {
	t.Helper()
	a := &agent.Agent{Name: "pending-agent", CreatedBy: testUser}
	if err := store.Create(context.Background(), tenant, a); err != nil {
		t.Fatalf("seedPendingAgent: %v", err)
	}
	return a.ID
}

// waitForStatus polls the invocation store until the invocation reaches the
// expected status or the deadline elapses. Fails the test on timeout.
//
// Scoped to testTenant by design — a cross-tenant invocation test should add an
// explicit tenant parameter back rather than assume this helper. (The parameter
// was dropped because every current caller passed testTenant; unparam flags an
// always-constant argument.)
func waitForStatus(t *testing.T, store *invocation.MemoryStore, id string, want invocation.Status) *invocation.Invocation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		inv, err := store.Get(context.Background(), testTenant, id)
		if err == nil && inv.Status == want {
			return inv
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("invocation %s did not reach status %s within 2s", id, want)
	return nil
}

func TestHandleTransact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setup       func(agent.AgentStore) string // returns agentID used in path
		fwd         httpapi.ProxyForwarder
		body        []byte
		noAuth      bool
		reqTenant   string
		reqUser     string
		wantStatus  int
		checkBody   func(t *testing.T, body []byte)
		checkHeader func(t *testing.T, h http.Header)
	}{
		{
			name: "202 with invocation_id for active agent",
			setup: func(s agent.AgentStore) string {
				return seedActiveAgent(t, s, "https://upstream.example.com/invoke")
			},
			fwd:        &mockForwarder{result: fakeResult(200, `{"ok":true}`)},
			body:       []byte(`{"prompt":"hello"}`),
			wantStatus: http.StatusAccepted,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]string
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if !strings.HasPrefix(resp["invocation_id"], "inv_") {
					t.Errorf("invocation_id = %q, want inv_ prefix", resp["invocation_id"])
				}
			},
			checkHeader: func(t *testing.T, h http.Header) {
				t.Helper()
				if got := h.Get("X-Zerker-Invocation-ID"); !strings.HasPrefix(got, "inv_") {
					t.Errorf("X-Zerker-Invocation-ID = %q, want inv_ prefix", got)
				}
			},
		},
		{
			name:       "401 when no auth context",
			setup:      func(s agent.AgentStore) string { return "agt_any" },
			fwd:        &mockForwarder{},
			noAuth:     true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "404 when agent does not exist",
			setup:      func(_ agent.AgentStore) string { return "agt_doesnotexist" },
			fwd:        &mockForwarder{},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "404 for agent owned by another tenant",
			setup: func(s agent.AgentStore) string {
				url := "https://other.example.com/invoke"
				a := &agent.Agent{Name: "other-agent", CreatedBy: "other-user", UpstreamURL: &url}
				if err := s.Create(context.Background(), "tenant-other", a); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return a.ID
			},
			fwd:        &mockForwarder{},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "422 when agent is inactive (soft-deleted)",
			setup: func(s agent.AgentStore) string {
				id := seedActiveAgent(t, s, "https://upstream.example.com/invoke")
				if err := s.Delete(context.Background(), testTenant, id); err != nil {
					t.Fatalf("setup delete: %v", err)
				}
				return id
			},
			fwd:        &mockForwarder{},
			wantStatus: http.StatusUnprocessableEntity,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]string
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp["error"] != "agent is inactive" {
					t.Errorf("error = %q, want %q", resp["error"], "agent is inactive")
				}
			},
		},
		{
			name: "422 when agent has no upstream_url (pending)",
			setup: func(s agent.AgentStore) string {
				return seedPendingAgent(t, s, testTenant)
			},
			fwd:        &mockForwarder{},
			wantStatus: http.StatusUnprocessableEntity,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]string
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp["error"] != "missing upstream_url" {
					t.Errorf("error = %q, want %q", resp["error"], "missing upstream_url")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, agentStore := newProxyHandler(t, nil, tc.fwd)
			agentID := tc.setup(agentStore)

			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			path := "/v1/proxy/" + agentID
			var req *http.Request
			if tc.noAuth {
				req = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(tc.body))
			} else {
				tenant, user := testTenant, testUser
				if tc.reqTenant != "" {
					tenant = tc.reqTenant
				}
				if tc.reqUser != "" {
					user = tc.reqUser
				}
				req = authedPostRequest(t, path, tc.body, tenant, user)
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
			if tc.checkHeader != nil {
				tc.checkHeader(t, rec.Header())
			}
		})
	}
}

func TestHandleTransactLifecycle(t *testing.T) {
	t.Parallel()

	// Verify the full lifecycle: POST → 202 → goroutine runs → invocation
	// transitions pending → running → succeeded.
	agentStore := agent.NewMemoryStore()
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/invoke")

	invStore := invocation.NewMemoryStore()
	fwd := &mockForwarder{result: fakeResult(200, `{"answer":"42"}`)}
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{"q":"test"}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}

	// X-Zerker-Invocation-ID header must be present.
	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	if !strings.HasPrefix(invID, "inv_") {
		t.Fatalf("X-Zerker-Invocation-ID = %q, want inv_ prefix", invID)
	}

	// Response body must contain the same invocation_id.
	var postResp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &postResp); err != nil {
		t.Fatalf("unmarshal POST response: %v", err)
	}
	if postResp["invocation_id"] != invID {
		t.Errorf("body invocation_id = %q, header = %q; must match", postResp["invocation_id"], invID)
	}

	// Wait for the background goroutine to mark the invocation as succeeded.
	inv := waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

	if inv.AgentID != agentID {
		t.Errorf("invocation AgentID = %q, want %q", inv.AgentID, agentID)
	}
	if inv.Mode != invocation.ModeTransactional {
		t.Errorf("invocation Mode = %q, want transactional", inv.Mode)
	}
	if inv.UpstreamStatus == nil || *inv.UpstreamStatus != 200 {
		t.Errorf("UpstreamStatus = %v, want 200", inv.UpstreamStatus)
	}
	if inv.LatencyMS == nil || *inv.LatencyMS != 42 {
		t.Errorf("LatencyMS = %v, want 42", inv.LatencyMS)
	}
}

func TestHandleTransactUpstreamError(t *testing.T) {
	t.Parallel()

	// Verify that an upstream failure transitions the invocation to failed.
	agentStore := agent.NewMemoryStore()
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/invoke")

	invStore := invocation.NewMemoryStore()
	fwd := &mockForwarder{err: proxy.ErrUpstreamUnreachable}
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", rec.Code)
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")

	waitForStatus(t, invStore, invID, invocation.StatusFailed)
}

// TestHandleTransactCapturesBodyAsBase64 verifies the body-capture contract: a
// poll response exposes the captured upstream body in response_body_b64,
// base64-encoded (a captured body can be arbitrary bytes, so it cannot be
// inlined verbatim into JSON).
func TestHandleTransactCapturesBodyAsBase64(t *testing.T) {
	t.Parallel()

	const upstreamBody = `{"answer":"42"}`
	agentStore := agent.NewMemoryStore()
	url := "https://upstream.example.com/invoke"
	a := &agent.Agent{Name: "capture-agent", CreatedBy: testUser, UpstreamURL: &url, CaptureBody: true}
	if err := agentStore.Create(context.Background(), testTenant, a); err != nil {
		t.Fatalf("seed capture agent: %v", err)
	}

	invStore := invocation.NewMemoryStore()
	fwd := &mockForwarder{result: fakeResult(200, upstreamBody)}
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	post := authedPostRequest(t, "/v1/proxy/"+a.ID, []byte(`{"q":"x"}`), testTenant, testUser)
	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", postRec.Code)
	}
	invID := postRec.Header().Get("X-Zerker-Invocation-ID")

	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

	poll := authedGetRequest(t, "/v1/proxy/"+a.ID+"/invocations/"+invID, testTenant, testUser)
	pollRec := httptest.NewRecorder()
	mux.ServeHTTP(pollRec, poll)
	if pollRec.Code != http.StatusOK {
		t.Fatalf("poll status = %d, want 200; body = %s", pollRec.Code, pollRec.Body.String())
	}

	var resp struct {
		ResponseBodyB64 string `json:"response_body_b64"`
	}
	if err := json.Unmarshal(pollRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal poll: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.ResponseBodyB64)
	if err != nil {
		t.Fatalf("response_body_b64 is not valid base64: %v", err)
	}
	if string(decoded) != upstreamBody {
		t.Errorf("decoded response_body_b64 = %q, want %q", decoded, upstreamBody)
	}
}

func TestHandlePoll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		buildInv   func(t *testing.T, store *invocation.MemoryStore, agentID string) string // returns inv_id in path
		noAuth     bool
		reqTenant  string
		wantStatus int
		checkBody  func(t *testing.T, body []byte)
	}{
		{
			name: "200 with pending status",
			buildInv: func(t *testing.T, store *invocation.MemoryStore, agentID string) string {
				t.Helper()
				inv := &invocation.Invocation{
					AgentID: agentID,
					Mode:    invocation.ModeTransactional,
					Status:  invocation.StatusPending,
				}
				if err := store.Create(context.Background(), testTenant, inv); err != nil {
					t.Fatalf("create invocation: %v", err)
				}
				return inv.ID
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp["status"] != "pending" {
					t.Errorf("status = %q, want pending", resp["status"])
				}
				if resp["upstream_status"] != nil {
					t.Errorf("upstream_status = %v, want nil (not yet completed)", resp["upstream_status"])
				}
			},
		},
		{
			name: "200 with succeeded status and upstream_status",
			buildInv: func(t *testing.T, store *invocation.MemoryStore, agentID string) string {
				t.Helper()
				inv := &invocation.Invocation{
					AgentID: agentID,
					Mode:    invocation.ModeTransactional,
					Status:  invocation.StatusPending,
				}
				if err := store.Create(context.Background(), testTenant, inv); err != nil {
					t.Fatalf("create invocation: %v", err)
				}
				succeeded := invocation.StatusSucceeded
				upstreamStatus := 200
				latency := int64(99)
				now := time.Now().UTC()
				if _, err := store.Update(context.Background(), testTenant, inv.ID, invocation.UpdateFields{
					Status:         &succeeded,
					UpstreamStatus: &upstreamStatus,
					LatencyMS:      &latency,
					CompletedAt:    &now,
				}); err != nil {
					t.Fatalf("update invocation: %v", err)
				}
				return inv.ID
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if resp["status"] != "succeeded" {
					t.Errorf("status = %q, want succeeded", resp["status"])
				}
				// JSON numbers decode as float64.
				if resp["upstream_status"] != float64(200) {
					t.Errorf("upstream_status = %v, want 200", resp["upstream_status"])
				}
				if resp["latency_ms"] != float64(99) {
					t.Errorf("latency_ms = %v, want 99", resp["latency_ms"])
				}
				if resp["completed_at"] == nil {
					t.Error("completed_at is nil, want non-nil")
				}
			},
		},
		{
			name: "404 when invocation not found",
			buildInv: func(_ *testing.T, _ *invocation.MemoryStore, _ string) string {
				return "inv_doesnotexist"
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "404 when invocation belongs to different agent",
			buildInv: func(t *testing.T, store *invocation.MemoryStore, _ string) string {
				t.Helper()
				// Create invocation for a different agent ID.
				inv := &invocation.Invocation{
					AgentID: "agt_other_agent",
					Mode:    invocation.ModeTransactional,
					Status:  invocation.StatusRunning,
				}
				if err := store.Create(context.Background(), testTenant, inv); err != nil {
					t.Fatalf("create invocation: %v", err)
				}
				return inv.ID
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "404 cross-tenant isolation",
			buildInv: func(t *testing.T, store *invocation.MemoryStore, agentID string) string {
				t.Helper()
				// Invocation created under a different tenant.
				inv := &invocation.Invocation{
					AgentID: agentID,
					Mode:    invocation.ModeTransactional,
					Status:  invocation.StatusRunning,
				}
				if err := store.Create(context.Background(), "tenant-other", inv); err != nil {
					t.Fatalf("create invocation: %v", err)
				}
				return inv.ID
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "401 when no auth context",
			buildInv: func(_ *testing.T, _ *invocation.MemoryStore, _ string) string {
				return "inv_any"
			},
			noAuth:     true,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agentStore := agent.NewMemoryStore()
			// Seed a placeholder agent; the poll handler only reads the path value
			// and checks the invocation's AgentID, not the agent store itself.
			agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com")

			invStore := invocation.NewMemoryStore()
			h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
				WithProxy(&mockForwarder{}, invStore)

			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			invID := tc.buildInv(t, invStore, agentID)
			path := "/v1/proxy/" + agentID + "/invocations/" + invID

			var req *http.Request
			if tc.noAuth {
				req = httptest.NewRequest(http.MethodGet, path, nil)
			} else {
				tenant := testTenant
				if tc.reqTenant != "" {
					tenant = tc.reqTenant
				}
				req = authedGETRequest(t, path, tenant, testUser)
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

func TestHandleStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setup       func(agent.AgentStore) string
		fwd         httpapi.ProxyForwarder
		body        []byte
		noAuth      bool
		wantStatus  int
		checkBody   func(t *testing.T, body []byte)
		checkHeader func(t *testing.T, h http.Header)
	}{
		{
			name: "200 with invocation_id header and streamed body",
			setup: func(s agent.AgentStore) string {
				return seedActiveAgent(t, s, "https://upstream.example.com/stream")
			},
			fwd:        &mockForwarder{streamResult: fakeResult(200, "data: hello\n\n")},
			body:       []byte(`{"prompt":"stream me"}`),
			wantStatus: http.StatusOK,
			checkHeader: func(t *testing.T, h http.Header) {
				t.Helper()
				if got := h.Get("X-Zerker-Invocation-ID"); !strings.HasPrefix(got, "inv_") {
					t.Errorf("X-Zerker-Invocation-ID = %q, want inv_ prefix", got)
				}
				if got := h.Get("X-Accel-Buffering"); got != "no" {
					t.Errorf("X-Accel-Buffering = %q, want no", got)
				}
			},
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				if string(b) != "data: hello\n\n" {
					t.Errorf("body = %q, want streamed body verbatim", b)
				}
			},
		},
		{
			name:       "401 when no auth context",
			setup:      func(s agent.AgentStore) string { return "agt_any" },
			fwd:        &mockForwarder{},
			noAuth:     true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "404 when agent does not exist",
			setup:      func(_ agent.AgentStore) string { return "agt_doesnotexist" },
			fwd:        &mockForwarder{},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "404 for agent owned by another tenant",
			setup: func(s agent.AgentStore) string {
				url := "https://other.example.com/stream"
				a := &agent.Agent{Name: "other-agent", CreatedBy: "other-user", UpstreamURL: &url}
				if err := s.Create(context.Background(), "tenant-other", a); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return a.ID
			},
			fwd:        &mockForwarder{},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "422 when agent is inactive",
			setup: func(s agent.AgentStore) string {
				id := seedActiveAgent(t, s, "https://upstream.example.com/stream")
				if err := s.Delete(context.Background(), testTenant, id); err != nil {
					t.Fatalf("setup delete: %v", err)
				}
				return id
			},
			fwd:        &mockForwarder{},
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name: "422 when agent has no upstream_url",
			setup: func(s agent.AgentStore) string {
				return seedPendingAgent(t, s, testTenant)
			},
			fwd:        &mockForwarder{},
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name: "upstream non-200 status forwarded verbatim",
			setup: func(s agent.AgentStore) string {
				return seedActiveAgent(t, s, "https://upstream.example.com/stream")
			},
			fwd:        &mockForwarder{streamResult: fakeResult(429, "rate limited")},
			wantStatus: http.StatusTooManyRequests,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				if string(b) != "rate limited" {
					t.Errorf("body = %q, want %q", b, "rate limited")
				}
			},
		},
		{
			name: "502 on upstream unreachable; invocation_id header present",
			setup: func(s agent.AgentStore) string {
				return seedActiveAgent(t, s, "https://upstream.example.com/stream")
			},
			fwd:        &mockForwarder{streamErr: proxy.ErrUpstreamUnreachable},
			wantStatus: http.StatusBadGateway,
			checkHeader: func(t *testing.T, h http.Header) {
				t.Helper()
				if got := h.Get("X-Zerker-Invocation-ID"); !strings.HasPrefix(got, "inv_") {
					t.Errorf("X-Zerker-Invocation-ID = %q, want inv_ prefix even on error", got)
				}
			},
		},
		{
			name: "502 on open circuit breaker",
			setup: func(s agent.AgentStore) string {
				return seedActiveAgent(t, s, "https://upstream.example.com/stream")
			},
			fwd:        &mockForwarder{streamErr: proxy.ErrCircuitOpen},
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "504 on upstream timeout",
			setup: func(s agent.AgentStore) string {
				return seedActiveAgent(t, s, "https://upstream.example.com/stream")
			},
			fwd:        &mockForwarder{streamErr: proxy.ErrUpstreamTimeout},
			wantStatus: http.StatusGatewayTimeout,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, agentStore := newProxyHandler(t, nil, tc.fwd)
			agentID := tc.setup(agentStore)

			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			path := "/v1/proxy/" + agentID + "/stream"
			var req *http.Request
			if tc.noAuth {
				req = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(tc.body))
			} else {
				req = authedPostRequest(t, path, tc.body, testTenant, testUser)
			}

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s",
					rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.checkHeader != nil {
				tc.checkHeader(t, rec.Header())
			}
			if tc.checkBody != nil {
				tc.checkBody(t, rec.Body.Bytes())
			}
		})
	}
}

// TestHandleStreamInvocationLifecycle verifies the full synchronous lifecycle:
// the handler blocks until streaming completes, then marks the invocation
// succeeded with the upstream status and response size captured.
func TestHandleStreamInvocationLifecycle(t *testing.T) {
	t.Parallel()

	const streamBody = "data: chunk1\n\ndata: chunk2\n\n"
	agentStore := agent.NewMemoryStore()
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")

	invStore := invocation.NewMemoryStore()
	fwd := &mockForwarder{streamResult: fakeResult(200, streamBody)}
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte("{}"), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	if !strings.HasPrefix(invID, "inv_") {
		t.Fatalf("X-Zerker-Invocation-ID = %q, want inv_ prefix", invID)
	}

	// The handler is synchronous: by the time ServeHTTP returns the invocation
	// must already be in a terminal state (no polling required).
	inv, err := invStore.Get(context.Background(), testTenant, invID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if inv.Mode != invocation.ModeStreaming {
		t.Errorf("invocation Mode = %q, want streaming", inv.Mode)
	}
	if inv.Status != invocation.StatusSucceeded {
		t.Errorf("invocation Status = %q, want succeeded", inv.Status)
	}
	if inv.AgentID != agentID {
		t.Errorf("invocation AgentID = %q, want %q", inv.AgentID, agentID)
	}
	if inv.UpstreamStatus == nil || *inv.UpstreamStatus != 200 {
		t.Errorf("UpstreamStatus = %v, want 200", inv.UpstreamStatus)
	}
	if inv.ResponseSize == nil || *inv.ResponseSize != int64(len(streamBody)) {
		t.Errorf("ResponseSize = %v, want %d", inv.ResponseSize, len(streamBody))
	}
	if rec.Body.String() != streamBody {
		t.Errorf("response body = %q, want %q", rec.Body.String(), streamBody)
	}
}

// TestHandleStreamUpstreamError verifies that a DoStream failure transitions
// the invocation to failed and returns the appropriate HTTP status.
func TestHandleStreamUpstreamError(t *testing.T) {
	t.Parallel()

	agentStore := agent.NewMemoryStore()
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")

	invStore := invocation.NewMemoryStore()
	fwd := &mockForwarder{streamErr: proxy.ErrUpstreamUnreachable}
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte("{}"), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	if !strings.HasPrefix(invID, "inv_") {
		t.Fatalf("X-Zerker-Invocation-ID = %q, want inv_ prefix", invID)
	}

	inv, err := invStore.Get(context.Background(), testTenant, invID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if inv.Status != invocation.StatusFailed {
		t.Errorf("invocation Status = %q, want failed", inv.Status)
	}
}

// ----------------------------------------------------- Treeship receipts ---

// fakeEmitter records emitted receipts and can simulate a Treeship failure.
type fakeEmitter struct {
	mu       sync.Mutex
	receipts []receipt.Receipt
	err      error
	emitted  chan struct{}
}

func (f *fakeEmitter) Emit(_ context.Context, r receipt.Receipt) error {
	f.mu.Lock()
	f.receipts = append(f.receipts, r)
	f.mu.Unlock()
	if f.emitted != nil {
		f.emitted <- struct{}{}
	}
	return f.err
}

func (f *fakeEmitter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.receipts)
}

// newReceiptHandler wires a proxy handler whose agent has EmitReceipts set as
// given, with a 200-returning upstream. Returns the handler, the invocation
// store, and the agent ID.
func newReceiptHandler(t *testing.T, emitReceipts bool, em receipt.Emitter) (*httpapi.Handler, *invocation.MemoryStore, string) {
	t.Helper()
	agentStore := agent.NewMemoryStore()
	url := "https://upstream.example.com/invoke"
	a := &agent.Agent{Name: "receipt-agent", CreatedBy: testUser, UpstreamURL: &url, EmitReceipts: emitReceipts}
	if err := agentStore.Create(context.Background(), testTenant, a); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	invStore := invocation.NewMemoryStore()
	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore).
		WithReceipts(em)
	return h, invStore, a.ID
}

func TestReceiptEmittedWhenEnabled(t *testing.T) {
	t.Parallel()
	em := &fakeEmitter{emitted: make(chan struct{}, 1)}
	h, invStore, agentID := newReceiptHandler(t, true, em)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{"q":"x"}`), testTenant, testUser))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", rec.Code)
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

	select {
	case <-em.emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("no receipt emitted within 2s")
	}
	em.mu.Lock()
	defer em.mu.Unlock()
	if len(em.receipts) != 1 {
		t.Fatalf("got %d receipts, want 1", len(em.receipts))
	}
	r := em.receipts[0]
	if r.InvocationID != invID {
		t.Errorf("receipt InvocationID = %q, want %q", r.InvocationID, invID)
	}
	if r.AgentID != agentID {
		t.Errorf("receipt AgentID = %q, want %q", r.AgentID, agentID)
	}
	if r.Status != string(invocation.StatusSucceeded) {
		t.Errorf("receipt Status = %q, want succeeded", r.Status)
	}
	if r.UpstreamStatus == nil || *r.UpstreamStatus != 200 {
		t.Errorf("receipt UpstreamStatus = %v, want 200", r.UpstreamStatus)
	}
}

func TestReceiptNotEmittedWhenDisabled(t *testing.T) {
	t.Parallel()
	em := &fakeEmitter{}
	h, invStore, agentID := newReceiptHandler(t, false, em)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))
	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

	if n := em.count(); n != 0 {
		t.Errorf("emitter received %d receipts with EmitReceipts off; want 0", n)
	}
}

func TestReceiptEmitterFailureIsFailOpen(t *testing.T) {
	t.Parallel()
	em := &fakeEmitter{err: errors.New("treeship down"), emitted: make(chan struct{}, 1)}
	h, invStore, agentID := newReceiptHandler(t, true, em)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", rec.Code)
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")

	// The invocation must still succeed even though receipt emission errors.
	inv := waitForStatus(t, invStore, invID, invocation.StatusSucceeded)
	if inv.Status != invocation.StatusSucceeded {
		t.Errorf("invocation Status = %q, want succeeded despite receipt failure", inv.Status)
	}
	// The emitter was still invoked (fail-open ≠ skipped).
	select {
	case <-em.emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("emitter was not invoked")
	}
}

// TestReceiptEmittedOnStream covers the streaming path, whose emit is driven by
// a defer over finalInv (set at handleStream's terminal Update sites) rather
// than the inline call runTransact uses.
func TestReceiptEmittedOnStream(t *testing.T) {
	t.Parallel()
	em := &fakeEmitter{emitted: make(chan struct{}, 1)}
	h, invStore, agentID := newReceiptHandler(t, true, em)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{"q":"x"}`), testTenant, testUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

	select {
	case <-em.emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("no streaming receipt emitted within 2s")
	}
	em.mu.Lock()
	defer em.mu.Unlock()
	if len(em.receipts) != 1 {
		t.Fatalf("got %d receipts, want 1", len(em.receipts))
	}
	r := em.receipts[0]
	if r.InvocationID != invID {
		t.Errorf("receipt InvocationID = %q, want %q", r.InvocationID, invID)
	}
	if r.Mode != string(invocation.ModeStreaming) {
		t.Errorf("receipt Mode = %q, want streaming", r.Mode)
	}
	if r.Status != string(invocation.StatusSucceeded) {
		t.Errorf("receipt Status = %q, want succeeded", r.Status)
	}
}

// TestHandleTransact_SuspendedAgent verifies that a suspended agent rejects
// invocations with 422 and stays in the catalog (spec 0002 Q11/Q12).
func TestHandleTransact_SuspendedAgent(t *testing.T) {
	t.Parallel()

	var agentID string
	h, agentStore := newProxyHandler(t, func(s agent.AgentStore) {
		url := "https://upstream.example.com/invoke"
		a := &agent.Agent{Name: "suspended-transact", CreatedBy: testUser, UpstreamURL: &url}
		if err := s.Create(context.Background(), testTenant, a); err != nil {
			t.Fatalf("create: %v", err)
		}
		suspended := true
		if _, err := s.Update(context.Background(), testTenant, a.ID, agent.UpdateFields{
			Suspended: &suspended,
		}); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		agentID = a.ID
	}, &mockForwarder{result: fakeResult(200, `{}`)})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["error"] != "agent is suspended" {
		t.Errorf("error = %q, want %q", resp["error"], "agent is suspended")
	}

	// The suspended agent must still be retrievable from the catalog.
	got, err := agentStore.Get(context.Background(), testTenant, agentID)
	if err != nil {
		t.Fatalf("get suspended agent from catalog: %v", err)
	}
	if !got.Suspended {
		t.Error("agent.Suspended = false after suspension; want true")
	}
	if got.Status == agent.StatusInactive {
		t.Error("agent.Status = inactive; suspension must not soft-delete the catalog entry")
	}
}

// TestHandleTransact_SuspendedAgentCrossTenant verifies that suspension is
// tenant-scoped: a different tenant requesting a suspended agent's ID gets 404
// (existence not confirmed across tenants, ADR-0005) — never 422, which would
// leak that the agent exists and is suspended in another tenant.
func TestHandleTransact_SuspendedAgentCrossTenant(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	var agentID string
	h, _ := newProxyHandler(t, func(s agent.AgentStore) {
		a := &agent.Agent{Name: "suspended-x", CreatedBy: testUser, UpstreamURL: &upstream.URL}
		if err := s.Create(context.Background(), testTenant, a); err != nil {
			t.Fatalf("create: %v", err)
		}
		suspended := true
		if _, err := s.Update(context.Background(), testTenant, a.ID, agent.UpdateFields{Suspended: &suspended}); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		agentID = a.ID
	}, &mockForwarder{result: fakeResult(200, `{}`)})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// tenant-other invokes testTenant's suspended agent ID.
	req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), "tenant-other", testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant suspended agent: status = %d, want 404 (not 422); body = %s",
			rec.Code, rec.Body.String())
	}
	if n := upstreamHits.Load(); n != 0 {
		t.Errorf("upstream contacted %d time(s) for a cross-tenant request; want 0", n)
	}
}

// TestHandleStream_SuspendedAgent verifies the same suspension check on the
// streaming endpoint (spec 0002 Q11/Q12).
func TestHandleStream_SuspendedAgent(t *testing.T) {
	t.Parallel()

	var agentID string
	h, _ := newProxyHandler(t, func(s agent.AgentStore) {
		url := "https://upstream.example.com/stream"
		a := &agent.Agent{Name: "suspended-stream", CreatedBy: testUser, UpstreamURL: &url}
		if err := s.Create(context.Background(), testTenant, a); err != nil {
			t.Fatalf("create: %v", err)
		}
		suspended := true
		if _, err := s.Update(context.Background(), testTenant, a.ID, agent.UpdateFields{
			Suspended: &suspended,
		}); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		agentID = a.ID
	}, &mockForwarder{})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["error"] != "agent is suspended" {
		t.Errorf("error = %q, want %q", resp["error"], "agent is suspended")
	}
}

// TestHandleTransact_PerAgentRateLimit verifies that an agent with
// InvocationRateLimit set enforces its own rate-limit bucket, independent of
// the global per-caller limit (spec 0002 Q11/Q12 — "per-agent override beats
// global").
func TestHandleTransact_PerAgentRateLimit(t *testing.T) {
	t.Parallel()

	agentRate := 1.0
	agentBurst := 1
	var agentID string

	invStore := invocation.NewMemoryStore()
	fwd := &mockForwarder{result: fakeResult(200, `{}`)}
	agentLim := ratelimit.NewAgentLimiter(ratelimit.DefaultTTL)
	t.Cleanup(agentLim.Close)

	agentStore := agent.NewMemoryStore()
	url := "https://upstream.example.com/invoke"
	a := &agent.Agent{
		Name:                "rate-limited-agent",
		CreatedBy:           testUser,
		UpstreamURL:         &url,
		InvocationRateLimit: &agentRate,
		InvocationBurst:     &agentBurst,
	}
	if err := agentStore.Create(context.Background(), testTenant, a); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	agentID = a.ID

	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore).
		WithAgentLimiter(agentLim)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// First request: burst=1 means one token available — must be accepted.
	req1 := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first request: got %d, want 202", rec1.Code)
	}

	// Second immediate request: bucket exhausted — must be rate limited.
	req2 := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429", rec2.Code)
	}
	retryAfter := rec2.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("second request: missing Retry-After header on 429")
	}
	secs, err := strconv.Atoi(retryAfter)
	if err != nil {
		t.Fatalf("Retry-After %q is not an integer: %v", retryAfter, err)
	}
	if secs < 1 {
		t.Errorf("Retry-After = %d, want >= 1", secs)
	}
}

// TestHandleStream_PerAgentRateLimit verifies per-agent rate limiting on the
// streaming endpoint (spec 0002 Q11/Q12).
func TestHandleStream_PerAgentRateLimit(t *testing.T) {
	t.Parallel()

	agentRate := 1.0
	agentBurst := 1
	var agentID string

	invStore := invocation.NewMemoryStore()
	fwd := &mockForwarder{streamResult: fakeResult(200, "data: ok\n\n")}
	agentLim := ratelimit.NewAgentLimiter(ratelimit.DefaultTTL)
	t.Cleanup(agentLim.Close)

	agentStore := agent.NewMemoryStore()
	url := "https://upstream.example.com/stream"
	a := &agent.Agent{
		Name:                "rate-limited-stream-agent",
		CreatedBy:           testUser,
		UpstreamURL:         &url,
		InvocationRateLimit: &agentRate,
		InvocationBurst:     &agentBurst,
	}
	if err := agentStore.Create(context.Background(), testTenant, a); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	agentID = a.ID

	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore).
		WithAgentLimiter(agentLim)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req1 := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser)
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rec1.Code)
	}

	req2 := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("second request: missing Retry-After header on 429")
	}
}

// TestHandleTransact_PerAgentRateLimitOverridesGlobal verifies that the
// per-agent bucket is keyed per-agent (not per-caller), so two different callers
// share the same per-agent bucket when calling the same agent.
func TestHandleTransact_PerAgentRateLimitOverridesGlobal(t *testing.T) {
	t.Parallel()

	agentRate := 1.0
	agentBurst := 1

	invStore := invocation.NewMemoryStore()
	fwd := &mockForwarder{result: fakeResult(200, `{}`)}
	agentLim := ratelimit.NewAgentLimiter(ratelimit.DefaultTTL)
	t.Cleanup(agentLim.Close)

	agentStore := agent.NewMemoryStore()
	url := "https://upstream.example.com/invoke"
	a := &agent.Agent{
		Name:                "shared-agent",
		CreatedBy:           testUser,
		UpstreamURL:         &url,
		InvocationRateLimit: &agentRate,
		InvocationBurst:     &agentBurst,
	}
	if err := agentStore.Create(context.Background(), testTenant, a); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore).
		WithAgentLimiter(agentLim)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// Caller A exhausts the per-agent bucket.
	reqA := authedPostRequest(t, "/v1/proxy/"+a.ID, []byte(`{}`), testTenant, "user-a")
	recA := httptest.NewRecorder()
	mux.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusAccepted {
		t.Fatalf("caller A: got %d, want 202", recA.Code)
	}

	// Caller B, same agent — bucket is now empty (per-agent, not per-caller).
	reqB := authedPostRequest(t, "/v1/proxy/"+a.ID, []byte(`{}`), testTenant, "user-b")
	recB := httptest.NewRecorder()
	mux.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusTooManyRequests {
		t.Errorf("caller B: got %d, want 429 (per-agent bucket shared across callers)", recB.Code)
	}
}

// blockingForwarder is a test double that blocks Do until either unblock is
// closed or the request context is cancelled. It is used to simulate a
// long-running upstream call so drain/shutdown tests can race against it.
type blockingForwarder struct {
	unblock <-chan struct{}
}

func (b *blockingForwarder) Do(ctx context.Context, _, _, _ string, _ *http.Request) (*proxy.Result, error) {
	select {
	case <-b.unblock:
		return fakeResult(200, `{"ok":true}`), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *blockingForwarder) DoStream(ctx context.Context, _, _, _ string, _ *http.Request) (*proxy.Result, error) {
	return b.Do(ctx, "", "", "", nil)
}

// errBodyReader is a test double for an io.Reader that always returns an error
// on the first Read. It is used to simulate body-read and copy errors.
type errBodyReader struct{ err error }

func (r *errBodyReader) Read(_ []byte) (int, error) { return 0, r.err }

// TestHandleTransact_ErrorClass verifies that every transactional failure path
// populates error_class with the correct taxonomy value (spec 0003).
func TestHandleTransact_ErrorClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		fwd       httpapi.ProxyForwarder
		wantClass invocation.ErrorClass
	}{
		{
			name:      "timeout",
			fwd:       &mockForwarder{err: proxy.ErrUpstreamTimeout},
			wantClass: invocation.ErrorClassTimeout,
		},
		{
			name:      "upstream_5xx via unreachable",
			fwd:       &mockForwarder{err: proxy.ErrUpstreamUnreachable},
			wantClass: invocation.ErrorClassUpstream5xx,
		},
		{
			name:      "upstream_5xx via circuit open",
			fwd:       &mockForwarder{err: proxy.ErrCircuitOpen},
			wantClass: invocation.ErrorClassUpstream5xx,
		},
		{
			name:      "ssrf_blocked",
			fwd:       &mockForwarder{err: proxy.ErrSSRFBlocked},
			wantClass: invocation.ErrorClassSSRFBlocked,
		},
		{
			name:      "credential_error",
			fwd:       &mockForwarder{err: proxy.ErrCredentialError},
			wantClass: invocation.ErrorClassCredentialError,
		},
		{
			name:      "cancelled",
			fwd:       &mockForwarder{err: context.Canceled},
			wantClass: invocation.ErrorClassCancelled,
		},
		{
			name:      "timeout via deadline exceeded",
			fwd:       &mockForwarder{err: context.DeadlineExceeded},
			wantClass: invocation.ErrorClassTimeout,
		},
		{
			// A 4xx upstream response is returned as a *Result (not retried), so
			// it reaches the success branch and must be recorded as failed +
			// upstream_4xx, not succeeded (spec 0003).
			name:      "upstream_4xx via status code",
			fwd:       &mockForwarder{result: fakeResult(404, "not found")},
			wantClass: invocation.ErrorClassUpstream4xx,
		},
		{
			// A non-transient 5xx (not 502/503/504) is likewise returned as a
			// *Result and must be recorded as upstream_5xx.
			name:      "upstream_5xx via non-transient status code",
			fwd:       &mockForwarder{result: fakeResult(500, "boom")},
			wantClass: invocation.ErrorClassUpstream5xx,
		},
		{
			name: "internal via body read error",
			fwd: &mockForwarder{result: &proxy.Result{
				StatusCode: 200,
				Header:     http.Header{},
				Body:       io.NopCloser(&errBodyReader{err: errors.New("read error")}),
				LatencyMS:  10,
			}},
			wantClass: invocation.ErrorClassInternal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agentStore := agent.NewMemoryStore()
			agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/invoke")

			invStore := invocation.NewMemoryStore()
			h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
				WithProxy(tc.fwd, invStore)
			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("POST status = %d, want 202", rec.Code)
			}

			invID := rec.Header().Get("X-Zerker-Invocation-ID")
			inv := waitForStatus(t, invStore, invID, invocation.StatusFailed)

			if inv.ErrorClass == nil {
				t.Fatalf("ErrorClass = nil, want %q", tc.wantClass)
			}
			if *inv.ErrorClass != tc.wantClass {
				t.Errorf("ErrorClass = %q, want %q", *inv.ErrorClass, tc.wantClass)
			}
		})
	}
}

// TestHandleTransact_SuccessLeaveErrorClassNull verifies that a successful
// transactional invocation leaves error_class NULL and ttft_ms NULL (spec 0003).
func TestHandleTransact_SuccessLeaveErrorClassNull(t *testing.T) {
	t.Parallel()

	agentStore := agent.NewMemoryStore()
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/invoke")

	invStore := invocation.NewMemoryStore()
	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	inv := waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

	if inv.ErrorClass != nil {
		t.Errorf("ErrorClass = %q on success; want nil", *inv.ErrorClass)
	}
	if inv.TTFTMS != nil {
		t.Errorf("TTFTMS = %v on transactional path; want nil", *inv.TTFTMS)
	}
}

// TestHandleStream_ErrorClass verifies that every streaming failure path
// populates error_class with the correct taxonomy value (spec 0003).
func TestHandleStream_ErrorClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		fwd       httpapi.ProxyForwarder
		wantClass invocation.ErrorClass
	}{
		{
			name:      "timeout",
			fwd:       &mockForwarder{streamErr: proxy.ErrUpstreamTimeout},
			wantClass: invocation.ErrorClassTimeout,
		},
		{
			name:      "upstream_5xx via unreachable",
			fwd:       &mockForwarder{streamErr: proxy.ErrUpstreamUnreachable},
			wantClass: invocation.ErrorClassUpstream5xx,
		},
		{
			name:      "upstream_5xx via circuit open",
			fwd:       &mockForwarder{streamErr: proxy.ErrCircuitOpen},
			wantClass: invocation.ErrorClassUpstream5xx,
		},
		{
			name:      "ssrf_blocked",
			fwd:       &mockForwarder{streamErr: proxy.ErrSSRFBlocked},
			wantClass: invocation.ErrorClassSSRFBlocked,
		},
		{
			name:      "credential_error",
			fwd:       &mockForwarder{streamErr: proxy.ErrCredentialError},
			wantClass: invocation.ErrorClassCredentialError,
		},
		{
			name:      "cancelled",
			fwd:       &mockForwarder{streamErr: context.Canceled},
			wantClass: invocation.ErrorClassCancelled,
		},
		{
			name:      "timeout via deadline exceeded",
			fwd:       &mockForwarder{streamErr: context.DeadlineExceeded},
			wantClass: invocation.ErrorClassTimeout,
		},
		{
			// A 4xx upstream response streams cleanly (copyErr == nil) but must
			// be recorded as failed + upstream_4xx for analytics (spec 0003).
			name:      "upstream_4xx via status code",
			fwd:       &mockForwarder{streamResult: fakeResult(422, "unprocessable")},
			wantClass: invocation.ErrorClassUpstream4xx,
		},
		{
			name:      "upstream_5xx via non-transient status code",
			fwd:       &mockForwarder{streamResult: fakeResult(500, "boom")},
			wantClass: invocation.ErrorClassUpstream5xx,
		},
		{
			name: "internal via copy error",
			fwd: &mockForwarder{streamResult: &proxy.Result{
				StatusCode: 200,
				Header:     http.Header{},
				Body:       io.NopCloser(&errBodyReader{err: errors.New("stream error")}),
				LatencyMS:  10,
			}},
			wantClass: invocation.ErrorClassInternal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agentStore := agent.NewMemoryStore()
			agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")

			invStore := invocation.NewMemoryStore()
			h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
				WithProxy(tc.fwd, invStore)
			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			req := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			// The streaming handler is synchronous; invocation is updated by the
			// time ServeHTTP returns. X-Zerker-Invocation-ID is always set
			// even when no HTTP response body is written (spec 0002 addressability).
			invID := rec.Header().Get("X-Zerker-Invocation-ID")
			if !strings.HasPrefix(invID, "inv_") {
				t.Fatalf("X-Zerker-Invocation-ID = %q, want inv_ prefix", invID)
			}

			inv, err := invStore.Get(context.Background(), testTenant, invID)
			if err != nil {
				t.Fatalf("get invocation: %v", err)
			}
			if inv.Status != invocation.StatusFailed {
				t.Fatalf("Status = %q, want failed", inv.Status)
			}
			if inv.ErrorClass == nil {
				t.Fatalf("ErrorClass = nil, want %q", tc.wantClass)
			}
			if *inv.ErrorClass != tc.wantClass {
				t.Errorf("ErrorClass = %q, want %q", *inv.ErrorClass, tc.wantClass)
			}
		})
	}
}

// TestHandleStream_TTFT verifies that a streaming invocation records a non-NULL
// ttft_ms when response bytes are received, and that error_class is NULL on
// success (spec 0003 acceptance criteria).
func TestHandleStream_TTFT(t *testing.T) {
	t.Parallel()

	agentStore := agent.NewMemoryStore()
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")

	invStore := invocation.NewMemoryStore()
	fwd := &mockForwarder{streamResult: fakeResult(200, "data: hello\n\n")}
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	inv, err := invStore.Get(context.Background(), testTenant, invID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if inv.TTFTMS == nil {
		t.Fatal("TTFTMS = nil, want non-nil on streaming path with bytes")
	}
	if *inv.TTFTMS < 0 {
		t.Errorf("TTFTMS = %d, want >= 0", *inv.TTFTMS)
	}
	if inv.ErrorClass != nil {
		t.Errorf("ErrorClass = %q on success; want nil", *inv.ErrorClass)
	}
}

// TestHandleModelHeader verifies that X-Zerker-Model is persisted to the
// invocation record on both transactional and streaming paths, and that its
// absence leaves Model nil (spec 0003 scoping decision 8).
func TestHandleModelHeader(t *testing.T) {
	t.Parallel()

	const wantModel = "gpt-4o"

	t.Run("transactional with model", func(t *testing.T) {
		t.Parallel()

		agentStore := agent.NewMemoryStore()
		agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/invoke")
		invStore := invocation.NewMemoryStore()
		fwd := &mockForwarder{result: fakeResult(200, `{}`)}
		h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
			WithProxy(fwd, invStore)
		mux := http.NewServeMux()
		h.RegisterRoutes(mux)

		req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
		req.Header.Set("X-Zerker-Model", wantModel)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		invID := rec.Header().Get("X-Zerker-Invocation-ID")
		inv := waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

		if inv.Model == nil {
			t.Fatal("Model = nil, want non-nil")
		}
		if *inv.Model != wantModel {
			t.Errorf("Model = %q, want %q", *inv.Model, wantModel)
		}
	})

	t.Run("transactional without model", func(t *testing.T) {
		t.Parallel()

		agentStore := agent.NewMemoryStore()
		agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/invoke")
		invStore := invocation.NewMemoryStore()
		fwd := &mockForwarder{result: fakeResult(200, `{}`)}
		h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
			WithProxy(fwd, invStore)
		mux := http.NewServeMux()
		h.RegisterRoutes(mux)

		req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		invID := rec.Header().Get("X-Zerker-Invocation-ID")
		inv := waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

		if inv.Model != nil {
			t.Errorf("Model = %q, want nil when header absent", *inv.Model)
		}
	})

	t.Run("streaming with model", func(t *testing.T) {
		t.Parallel()

		agentStore := agent.NewMemoryStore()
		agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")
		invStore := invocation.NewMemoryStore()
		fwd := &mockForwarder{streamResult: fakeResult(200, "data: ok\n\n")}
		h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
			WithProxy(fwd, invStore)
		mux := http.NewServeMux()
		h.RegisterRoutes(mux)

		req := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser)
		req.Header.Set("X-Zerker-Model", wantModel)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		invID := rec.Header().Get("X-Zerker-Invocation-ID")
		inv, err := invStore.Get(context.Background(), testTenant, invID)
		if err != nil {
			t.Fatalf("get invocation: %v", err)
		}
		if inv.Model == nil {
			t.Fatal("Model = nil, want non-nil")
		}
		if *inv.Model != wantModel {
			t.Errorf("Model = %q, want %q", *inv.Model, wantModel)
		}
	})

	t.Run("streaming without model", func(t *testing.T) {
		t.Parallel()

		agentStore := agent.NewMemoryStore()
		agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")
		invStore := invocation.NewMemoryStore()
		fwd := &mockForwarder{streamResult: fakeResult(200, "data: ok\n\n")}
		h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
			WithProxy(fwd, invStore)
		mux := http.NewServeMux()
		h.RegisterRoutes(mux)

		req := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		invID := rec.Header().Get("X-Zerker-Invocation-ID")
		inv, err := invStore.Get(context.Background(), testTenant, invID)
		if err != nil {
			t.Fatalf("get invocation: %v", err)
		}
		if inv.Model != nil {
			t.Errorf("Model = %q, want nil when header absent", *inv.Model)
		}
	})
}

// TestHandleModelHeaderTooLong verifies that an X-Zerker-Model header longer
// than the handler's cap (maxModelNameBytes) is rejected at the boundary with
// 400 on both proxy paths, before any invocation record is created (AGENTS.md
// invariant #3 — validate inbound payloads; spec 0003).
func TestHandleModelHeaderTooLong(t *testing.T) {
	t.Parallel()

	// One byte over the handler's 256-byte cap; kept literal because the
	// constant is unexported from package httpapi.
	oversize := strings.Repeat("m", 257)

	tests := []struct {
		name   string
		stream bool
		fwd    httpapi.ProxyForwarder
	}{
		{"transactional", false, &mockForwarder{result: fakeResult(200, `{}`)}},
		{"streaming", true, &mockForwarder{streamResult: fakeResult(200, "data: ok\n\n")}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agentStore := agent.NewMemoryStore()
			agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/invoke")
			invStore := invocation.NewMemoryStore()
			h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
				WithProxy(tc.fwd, invStore)
			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			path := "/v1/proxy/" + agentID
			if tc.stream {
				path += "/stream"
			}
			req := authedPostRequest(t, path, []byte(`{}`), testTenant, testUser)
			req.Header.Set("X-Zerker-Model", oversize)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			// Rejected before Create: no invocation ID header is emitted.
			if id := rec.Header().Get("X-Zerker-Invocation-ID"); id != "" {
				t.Errorf("X-Zerker-Invocation-ID = %q, want empty (rejected before create)", id)
			}
		})
	}
}

// TestHandleTransactDrainOnShutdown verifies that Handler.Shutdown drains
// in-flight transactional goroutines: after Shutdown returns every invocation
// is in a terminal state (succeeded or failed), never left in running.
// This covers the acceptance criterion for issue #53.
func TestHandleTransactDrainOnShutdown(t *testing.T) {
	t.Parallel()

	// unblock is never closed; the forwarder blocks until the context is
	// cancelled by Shutdown, at which point it returns context.Canceled and
	// the goroutine marks the invocation failed.
	unblock := make(chan struct{})
	fwd := &blockingForwarder{unblock: unblock}

	agentStore := agent.NewMemoryStore()
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/invoke")
	invStore := invocation.NewMemoryStore()
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// POST → 202: spawns the background goroutine which will block in Do.
	req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", rec.Code)
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	if !strings.HasPrefix(invID, "inv_") {
		t.Fatalf("X-Zerker-Invocation-ID = %q, want inv_ prefix", invID)
	}

	// Wait until the goroutine is running (invocation transitions to running).
	waitForStatus(t, invStore, invID, invocation.StatusRunning)

	// Shutdown: cancels the goroutine's context and waits for it to drain.
	// The goroutine unblocks from ctx.Done(), marks the invocation failed, and
	// decrements the WaitGroup. Shutdown should return well within 5 s.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() = %v, want nil", err)
	}

	// After Shutdown the invocation must be in a terminal state.
	inv, err := invStore.Get(context.Background(), testTenant, invID)
	if err != nil {
		t.Fatalf("get invocation after shutdown: %v", err)
	}
	if inv.Status == invocation.StatusRunning {
		t.Errorf("invocation Status = running after Shutdown; want terminal (succeeded or failed)")
	}
}

// seedMCPAgent creates a Protocol=mcp agent (streamable_http transport) in
// store and returns its ID. Mirrors internal/proxy's seedMCPAgent (spec 0004):
// the proxy handlers route mcp agents through the same code path as http
// agents (decisions 4/5), so httpapi-layer tests need their own mcp-flagged
// seed helper to prove no protocol branch exists at this layer either.
func seedMCPAgent(t *testing.T, store agent.AgentStore, upstreamURL string) string {
	t.Helper()
	transport := "streamable_http"
	a := &agent.Agent{
		Name:         "mcp-proxy-test-agent",
		CreatedBy:    testUser,
		UpstreamURL:  &upstreamURL,
		Protocol:     "mcp",
		MCPTransport: &transport,
	}
	if err := store.Create(context.Background(), testTenant, a); err != nil {
		t.Fatalf("seedMCPAgent: %v", err)
	}
	return a.ID
}

// capturingForwarder wraps mockForwarder and records the *http.Request headers
// passed to Do/DoStream, letting tests assert the caller's headers reached the
// forwarder unmodified — i.e. handleTransact/handleStream apply no header
// filtering of their own before delegating to the Forwarder (spec 0004
// decision 5: transparent pipe).
type capturingForwarder struct {
	mockForwarder
	gotDoHeader       http.Header
	gotDoStreamHeader http.Header
}

func (c *capturingForwarder) Do(ctx context.Context, tenantID, agentID, invocationID string, r *http.Request) (*proxy.Result, error) {
	c.gotDoHeader = r.Header.Clone()
	return c.mockForwarder.Do(ctx, tenantID, agentID, invocationID, r)
}

func (c *capturingForwarder) DoStream(ctx context.Context, tenantID, agentID, invocationID string, r *http.Request) (*proxy.Result, error) {
	c.gotDoStreamHeader = r.Header.Clone()
	return c.mockForwarder.DoStream(ctx, tenantID, agentID, invocationID, r)
}

// TestHandleTransact_MCPSessionIDForwardedToForwarder verifies that
// POST /v1/proxy/{id} for a Protocol=mcp agent passes the caller's
// Mcp-Session-Id header through to Forwarder.Do unchanged (spec 0004 issue
// #92 acceptance: "MCP invocations route correctly (txn); Mcp-Session-Id
// round-trips"). Do itself (internal/proxy) separately proves the header then
// reaches the upstream; this test guards the handler layer in between.
func TestHandleTransact_MCPSessionIDForwardedToForwarder(t *testing.T) {
	t.Parallel()

	agentStore := agent.NewMemoryStore()
	agentID := seedMCPAgent(t, agentStore, "https://mcp-upstream.example.com/")

	invStore := invocation.NewMemoryStore()
	fwd := &capturingForwarder{mockForwarder: mockForwarder{result: fakeResult(200, `{"jsonrpc":"2.0","id":1,"result":{}}`)}}
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), testTenant, testUser)
	req.Header.Set("Mcp-Session-Id", "sess-abc123")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

	if fwd.gotDoHeader == nil {
		t.Fatal("forwarder Do was never called")
	}
	if got := fwd.gotDoHeader.Get("Mcp-Session-Id"); got != "sess-abc123" {
		t.Errorf("forwarder received Mcp-Session-Id = %q, want %q", got, "sess-abc123")
	}
}

// TestHandleStream_MCPSessionIDRoundTrip verifies that POST
// /v1/proxy/{id}/stream for a Protocol=mcp agent both (a) forwards the
// caller's Mcp-Session-Id to Forwarder.DoStream and (b) copies the upstream's
// Mcp-Session-Id response header (e.g. server-assigned on initialize) back
// onto the response to the caller — the full round trip spec 0004 issue #92
// requires for the streaming endpoint.
func TestHandleStream_MCPSessionIDRoundTrip(t *testing.T) {
	t.Parallel()

	agentStore := agent.NewMemoryStore()
	agentID := seedMCPAgent(t, agentStore, "https://mcp-upstream.example.com/stream")

	streamResult := fakeResult(200, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n")
	streamResult.Header.Set("Mcp-Session-Id", "sess-server-assigned")
	fwd := &capturingForwarder{mockForwarder: mockForwarder{streamResult: streamResult}}

	invStore := invocation.NewMemoryStore()
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_docs"}}`), testTenant, testUser)
	req.Header.Set("Mcp-Session-Id", "sess-caller")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if fwd.gotDoStreamHeader == nil {
		t.Fatal("forwarder DoStream was never called")
	}
	if got := fwd.gotDoStreamHeader.Get("Mcp-Session-Id"); got != "sess-caller" {
		t.Errorf("forwarder received Mcp-Session-Id = %q, want %q", got, "sess-caller")
	}
	if got := rec.Header().Get("Mcp-Session-Id"); got != "sess-server-assigned" {
		t.Errorf("caller response Mcp-Session-Id = %q, want %q", got, "sess-server-assigned")
	}
}

// TestHandleStream_SessionIDRoundTripUnaffectedForHTTPProtocol is a
// regression guard for spec 0004's "Protocol=http agents: zero behavior
// change" (issue #92 acceptance): it repeats
// TestHandleStream_MCPSessionIDRoundTrip's exact scenario against a plain
// Protocol=http agent to prove the round trip was never mcp-gated.
func TestHandleStream_SessionIDRoundTripUnaffectedForHTTPProtocol(t *testing.T) {
	t.Parallel()

	agentStore := agent.NewMemoryStore()
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/stream")

	streamResult := fakeResult(200, "data: hello\n\n")
	streamResult.Header.Set("Mcp-Session-Id", "sess-server-assigned")
	fwd := &capturingForwarder{mockForwarder: mockForwarder{streamResult: streamResult}}

	invStore := invocation.NewMemoryStore()
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{"prompt":"not mcp at all"}`), testTenant, testUser)
	req.Header.Set("Mcp-Session-Id", "sess-caller")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if got := fwd.gotDoStreamHeader.Get("Mcp-Session-Id"); got != "sess-caller" {
		t.Errorf("forwarder received Mcp-Session-Id = %q, want %q", got, "sess-caller")
	}
	if got := rec.Header().Get("Mcp-Session-Id"); got != "sess-server-assigned" {
		t.Errorf("caller response Mcp-Session-Id = %q, want %q", got, "sess-server-assigned")
	}
}
