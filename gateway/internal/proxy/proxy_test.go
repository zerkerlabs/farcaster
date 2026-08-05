package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/credential"
	"github.com/zerkerlabs/gateway/gateway/internal/proxy"
)

const (
	testTenant = "tenant-proxy-test"
	testUser   = "user-proxy-test"
)

// stubCredSvc is a minimal CredentialService for tests.
type stubCredSvc struct {
	cred      *credential.Credential
	plaintext []byte
}

func (s *stubCredSvc) Get(_ context.Context, _, _ string) (*credential.Credential, error) {
	if s.cred == nil {
		return nil, credential.ErrNotFound
	}
	return s.cred, nil
}

func (s *stubCredSvc) Decrypt(_ context.Context, _, _ string) ([]byte, error) {
	return s.plaintext, nil
}

// seedAgent creates an agent in store and returns its ID.
func seedAgent(t *testing.T, store agent.AgentStore, upstreamURL string, credRef *string) string {
	t.Helper()
	a := &agent.Agent{
		Name:          "test-agent-" + upstreamURL,
		CreatedBy:     testUser,
		UpstreamURL:   &upstreamURL,
		CredentialRef: credRef,
	}
	if err := store.Create(context.Background(), testTenant, a); err != nil {
		t.Fatalf("seedAgent: %v", err)
	}
	return a.ID
}

// seedMCPAgent is seedAgent's Protocol=mcp counterpart, used to confirm the
// dial-time SSRF guard (safeDialContext / ssrf.IsBlockedIP) applies to an MCP
// agent's upstream_url exactly as it does for an HTTP agent — the Forwarder is
// a transparent pipe (spec 0004 decision 5) with no protocol-based branch that
// could bypass the dialer.
func seedMCPAgent(t *testing.T, store agent.AgentStore, upstreamURL string) string {
	t.Helper()
	transport := "streamable_http"
	a := &agent.Agent{
		Name:         "test-mcp-agent-" + upstreamURL,
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

// noRetryConfig returns a Config with minimal retry overhead for tests that do
// not exercise retry behaviour (fast retry delay; BreakerThreshold set high so
// the circuit never trips during a single-assertion test).
func noRetryConfig(client *http.Client) proxy.Config {
	return proxy.Config{
		MaxRetries:       2,
		RetryBaseDelay:   1 * time.Millisecond,
		BreakerThreshold: 100,
		BreakerCooldown:  1 * time.Hour,
		HTTPClient:       client,
	}
}

// TestBodyForwardedVerbatim verifies that the upstream receives the caller's
// body byte-for-byte (spec 0002 §Q4: "verbatim body forwarding").
func TestBodyForwardedVerbatim(t *testing.T) {
	t.Parallel()

	want := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024}`)

	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(want))
	req.Header.Set("Content-Type", "application/json")

	result, err := f.Do(context.Background(), testTenant, agentID, "inv_test", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	if !bytes.Equal(got, want) {
		t.Errorf("upstream body = %q, want %q", got, want)
	}
}

// TestCrossTenantAgentIsolation verifies that calling Do with a tenant that does
// not own the agent returns agent.ErrNotFound and never reaches the upstream
// (spec 0002 acceptance: cross-tenant id returns 404; AGENTS.md invariant #2).
func TestCrossTenantAgentIsolation(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil) // owned by testTenant
	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	_, err := f.Do(context.Background(), "other-tenant", agentID, "inv_x", req)

	if !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("cross-tenant Do: err = %v, want agent.ErrNotFound", err)
	}
	if n := upstreamHits.Load(); n != 0 {
		t.Errorf("upstream was contacted %d time(s) for a cross-tenant request; want 0", n)
	}
}

// TestAuthorizationStripped verifies that the caller's Authorization header
// never reaches the upstream (spec 0002 §Q5).
func TestAuthorizationStripped(t *testing.T) {
	t.Parallel()

	var upstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer caller-secret-token")

	result, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	if upstreamAuth != "" {
		t.Errorf("upstream received Authorization = %q, want empty", upstreamAuth)
	}
	if strings.Contains(upstreamAuth, "caller-secret-token") {
		t.Error("caller Authorization token must never reach upstream")
	}
}

// TestUpstreamCredentialInjectedBearer verifies that when the agent has a
// bearer credential configured, it is injected as Authorization: Bearer on the
// upstream request (replacing the stripped caller Authorization).
func TestUpstreamCredentialInjectedBearer(t *testing.T) {
	t.Parallel()

	const secret = "upstream-bearer-secret"
	var upstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	credID := "cred_test"
	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, &credID)

	svc := &stubCredSvc{
		cred: &credential.Credential{
			ID:       credID,
			AuthType: credential.AuthTypeBearer,
			Source:   credential.SourceManaged,
		},
		plaintext: []byte(secret),
	}
	f := proxy.New(store, svc, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer caller-token") // must be stripped

	result, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	want := "Bearer " + secret
	if upstreamAuth != want {
		t.Errorf("upstream Authorization = %q, want %q", upstreamAuth, want)
	}
}

// TestUpstreamCredentialInjectedAPIKey verifies that api_key credentials are
// injected as X-Api-Key (not Authorization).
func TestUpstreamCredentialInjectedAPIKey(t *testing.T) {
	t.Parallel()

	apiKeyValue := "sk-test-api-key" //nolint:gosec // test credential, not a real secret
	var upstreamAPIKey, upstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAPIKey = r.Header.Get("X-Api-Key")
		upstreamAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	credID := "cred_apikey"
	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, &credID)

	svc := &stubCredSvc{
		cred: &credential.Credential{
			ID:       credID,
			AuthType: credential.AuthTypeAPIKey,
			Source:   credential.SourceManaged,
		},
		plaintext: []byte(apiKeyValue),
	}
	f := proxy.New(store, svc, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer caller-token")

	result, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	if upstreamAPIKey != apiKeyValue {
		t.Errorf("upstream X-Api-Key = %q, want %q", upstreamAPIKey, apiKeyValue)
	}
	// Caller Authorization must still be stripped; api_key goes to X-Api-Key.
	if upstreamAuth != "" {
		t.Errorf("upstream Authorization = %q, want empty (caller auth stripped)", upstreamAuth)
	}
}

// TestXRequestIDHonored verifies that a caller-supplied X-Request-ID is
// forwarded unchanged (spec 0002 §Q5: "honour the caller's if present").
func TestXRequestIDHonored(t *testing.T) {
	t.Parallel()

	const callerID = "req-caller-supplied-123"
	var upstreamID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamID = r.Header.Get("X-Request-ID")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", callerID)

	result, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	if upstreamID != callerID {
		t.Errorf("upstream X-Request-ID = %q, want %q", upstreamID, callerID)
	}
}

// TestXRequestIDInjected verifies that Zerker injects an X-Request-ID when
// the caller does not supply one (spec 0002 §Q5: "otherwise Zerker injects
// one").
func TestXRequestIDInjected(t *testing.T) {
	t.Parallel()

	var upstreamID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamID = r.Header.Get("X-Request-ID")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// Deliberately no X-Request-ID header.

	result, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	if upstreamID == "" {
		t.Error("upstream received no X-Request-ID; forwarder must inject one")
	}
}

// TestXZerkerHeadersSpoofPrevented verifies that a caller cannot inject
// X-Zerker-* headers — they are stripped before forwarding.
func TestXZerkerHeadersSpoofPrevented(t *testing.T) {
	t.Parallel()

	var upstreamAgentID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAgentID = r.Header.Get("X-Zerker-Agent-ID")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Zerker-Agent-ID", "agt_spoofed") // caller tries to spoof

	result, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	// The upstream should see the real agent ID set by the forwarder, not the
	// spoofed value.
	if upstreamAgentID == "agt_spoofed" {
		t.Error("caller-supplied X-Zerker-Agent-ID must be stripped before forwarding")
	}
	if upstreamAgentID != agentID {
		t.Errorf("upstream X-Zerker-Agent-ID = %q, want %q", upstreamAgentID, agentID)
	}
}

// TestLegacyXFarcasterHeadersSpoofPrevented verifies that the pre-rename
// X-Farcaster-* prefix is still stripped. The gateway no longer emits these,
// so an upstream still keyed to the old contract would otherwise receive a
// value supplied entirely by the caller.
func TestLegacyXFarcasterHeadersSpoofPrevented(t *testing.T) {
	t.Parallel()

	var upstreamLegacyAgentID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamLegacyAgentID = r.Header.Get("X-Farcaster-Agent-ID")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Farcaster-Agent-ID", "agt_spoofed") // caller tries to spoof

	result, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	if upstreamLegacyAgentID != "" {
		t.Errorf("caller-supplied X-Farcaster-Agent-ID = %q, want it stripped before forwarding", upstreamLegacyAgentID)
	}
}

// TestMissingUpstreamURL verifies that invoking an agent with no upstream_url
// returns ErrMissingUpstreamURL without making any upstream call.
func TestMissingUpstreamURL(t *testing.T) {
	t.Parallel()

	var upstreamCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	// Create an agent with no upstream_url (pending status).
	a := &agent.Agent{Name: "pending-agent", CreatedBy: testUser}
	if err := store.Create(context.Background(), testTenant, a); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	_, err := f.Do(context.Background(), testTenant, a.ID, "", req)
	if !errors.Is(err, proxy.ErrMissingUpstreamURL) {
		t.Errorf("err = %v, want ErrMissingUpstreamURL", err)
	}
	if upstreamCalled {
		t.Error("upstream must not be called when upstream_url is missing")
	}
}

// TestRetryOnTransientStatus verifies that the forwarder retries when the
// upstream returns a transient status code (503) and eventually succeeds.
func TestRetryOnTransientStatus(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := callCount.Add(1); n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 — triggers retry
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       2,
		RetryBaseDelay:   1 * time.Millisecond,
		BreakerThreshold: 100,
		HTTPClient:       upstream.Client(),
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	result, err := f.Do(context.Background(), testTenant, agentID, "inv_retry_test", req)
	if err != nil {
		t.Fatalf("Do: %v (want success after retries)", err)
	}
	_ = result.Body.Close()

	if got := callCount.Load(); got != 3 {
		t.Errorf("upstream call count = %d, want 3 (2 failures + 1 success)", got)
	}
}

// TestRetryExhausted verifies that when all retry attempts fail with a
// transient status, Do returns ErrUpstreamUnreachable.
func TestRetryExhausted(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusBadGateway) // 502
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       2,
		RetryBaseDelay:   1 * time.Millisecond,
		BreakerThreshold: 100,
		HTTPClient:       upstream.Client(),
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	_, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if !errors.Is(err, proxy.ErrUpstreamUnreachable) {
		t.Errorf("err = %v, want ErrUpstreamUnreachable", err)
	}
	// 3 total attempts (1 initial + 2 retries).
	if got := callCount.Load(); got != 3 {
		t.Errorf("call count = %d, want 3", got)
	}
}

// TestMCPToolsCallNeverRetried verifies that a Protocol=mcp agent's tools/call
// invocation is NOT retried when the upstream returns a transient status
// (503): retrying could double-execute a side-effecting tool call (spec 0004,
// Decision 6). Only the initial attempt reaches the upstream and Do returns
// ErrUpstreamUnreachable rather than succeeding on a later attempt.
func TestMCPToolsCallNeverRetried(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := callCount.Add(1); n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 — transient, would trigger retry
			return
		}
		w.WriteHeader(http.StatusOK) // would succeed on the 3rd attempt if retried
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedMCPAgent(t, store, upstream.URL)
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       2,
		RetryBaseDelay:   1 * time.Millisecond,
		BreakerThreshold: 100,
		HTTPClient:       upstream.Client(),
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_docs"}}`))
	_, err := f.Do(context.Background(), testTenant, agentID, "inv_mcp_toolscall_no_retry", req)
	if !errors.Is(err, proxy.ErrUpstreamUnreachable) {
		t.Errorf("err = %v, want ErrUpstreamUnreachable (no retry on tools/call)", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("upstream call count = %d, want 1 (tools/call must never be retried)", got)
	}
}

// TestMCPAllowlistedMethodStillRetried verifies that a Protocol=mcp agent's
// tools/list call — a known-idempotent, allowlisted method (spec 0004,
// Decision 6) — is still retried on a transient upstream status exactly like
// an HTTP agent would be.
func TestMCPAllowlistedMethodStillRetried(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := callCount.Add(1); n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedMCPAgent(t, store, upstream.URL)
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       2,
		RetryBaseDelay:   1 * time.Millisecond,
		BreakerThreshold: 100,
		HTTPClient:       upstream.Client(),
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	result, err := f.Do(context.Background(), testTenant, agentID, "inv_mcp_toolslist_retry", req)
	if err != nil {
		t.Fatalf("Do: %v (want success after retries)", err)
	}
	_ = result.Body.Close()

	if got := callCount.Load(); got != 3 {
		t.Errorf("upstream call count = %d, want 3 (2 failures + 1 success)", got)
	}
}

// TestMCPUnrecognisedMethodNotRetried verifies the fail-closed behaviour of the
// retry allowlist: a method that is neither allowlisted nor "tools/call" (any
// unrecognised/future MCP method) is also not retried, and a body that fails
// to parse as JSON-RPC at all is treated the same way. spec 0004 Decision 6
// intends the allowlist as an allow-only gate, not a deny-list of tools/call.
func TestMCPUnrecognisedMethodNotRetried(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedMCPAgent(t, store, upstream.URL)
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       2,
		RetryBaseDelay:   1 * time.Millisecond,
		BreakerThreshold: 100,
		HTTPClient:       upstream.Client(),
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"notifications/cancelled"}`))
	_, err := f.Do(context.Background(), testTenant, agentID, "inv_mcp_unknown_method", req)
	if !errors.Is(err, proxy.ErrUpstreamUnreachable) {
		t.Errorf("err = %v, want ErrUpstreamUnreachable", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("upstream call count = %d, want 1 (unrecognised method must not be retried)", got)
	}
}

// TestHTTPProtocolRetryUnaffected is a regression guard for spec 0004's
// "Protocol=http retry behavior unchanged" requirement: a plain HTTP agent
// (no Protocol/MCPTransport set) whose request body happens to contain a
// "method" field shaped like tools/call must still retry on a transient
// status — the allowlist gate applies only to Protocol=mcp agents.
func TestHTTPProtocolRetryUnaffected(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := callCount.Add(1); n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       2,
		RetryBaseDelay:   1 * time.Millisecond,
		BreakerThreshold: 100,
		HTTPClient:       upstream.Client(),
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_docs"}}`))
	result, err := f.Do(context.Background(), testTenant, agentID, "inv_http_toolscall_retry", req)
	if err != nil {
		t.Fatalf("Do: %v (want success after retries — Protocol=http ignores the mcp allowlist)", err)
	}
	_ = result.Body.Close()

	if got := callCount.Load(); got != 3 {
		t.Errorf("upstream call count = %d, want 3 (2 failures + 1 success)", got)
	}
}

// TestCircuitBreakerOpens verifies that after BreakerThreshold consecutive Do
// failures the circuit opens and subsequent calls return ErrCircuitOpen
// without hitting the upstream.
func TestCircuitBreakerOpens(t *testing.T) {
	t.Parallel()

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)

	const threshold = 2
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       1,
		RetryBaseDelay:   1 * time.Millisecond,
		BreakerThreshold: threshold,
		BreakerCooldown:  1 * time.Hour, // keep circuit open for the test's lifetime
		HTTPClient:       upstream.Client(),
	}, nil)

	// Each Do call makes (1 initial + 1 retry) = 2 upstream requests, then
	// records one failure against the breaker.
	for i := 0; i < threshold; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		_, err := f.Do(context.Background(), testTenant, agentID, "", req)
		if err == nil {
			t.Fatalf("attempt %d: expected error, got nil", i+1)
		}
	}

	callsBefore := upstreamCalls.Load()

	// The next call must be rejected by the open circuit without touching the
	// upstream.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if !errors.Is(err, proxy.ErrCircuitOpen) {
		t.Errorf("err = %v, want ErrCircuitOpen", err)
	}
	if after := upstreamCalls.Load(); after != callsBefore {
		t.Errorf("upstream was called %d extra time(s) after circuit opened; want 0", after-callsBefore)
	}
}

// TestCircuitBreakerOpensFromStreaming is a regression test: DoStream used to
// call br.success() on any upstream response, including 5xx, so the shared
// breaker never opened from streaming traffic. A transient 5xx streamed back
// must count as a failure.
func TestCircuitBreakerOpensFromStreaming(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)

	const threshold = 2
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		BreakerThreshold: threshold,
		BreakerCooldown:  1 * time.Hour, // keep the circuit open for the test
		HTTPClient:       upstream.Client(),
	}, nil)

	// Each streamed 5xx must record one breaker failure. DoStream returns the
	// response (no error) — the 503 is streamed back to the caller.
	for i := 0; i < threshold; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("data"))
		res, err := f.DoStream(context.Background(), testTenant, agentID, "", req)
		if err != nil {
			t.Fatalf("attempt %d: DoStream err = %v, want nil (503 streamed back)", i+1, err)
		}
		if res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d: status = %d, want 503", i+1, res.StatusCode)
		}
		_ = res.Body.Close()
	}

	// The breaker must now be open and reject the next stream without an upstream call.
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("data"))
	if _, err := f.DoStream(context.Background(), testTenant, agentID, "", req); !errors.Is(err, proxy.ErrCircuitOpen) {
		t.Errorf("DoStream after %d streamed 5xx: err = %v, want ErrCircuitOpen", threshold, err)
	}
}

// TestCircuitBreakerHalfOpen verifies that after the cooldown the breaker
// allows one probe, and a successful probe closes the circuit.
func TestCircuitBreakerHalfOpen(t *testing.T) {
	t.Parallel()

	var failMode atomic.Bool
	failMode.Store(true)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failMode.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)

	const cooldown = 20 * time.Millisecond
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       1,
		RetryBaseDelay:   1 * time.Millisecond,
		BreakerThreshold: 1,
		BreakerCooldown:  cooldown,
		HTTPClient:       upstream.Client(),
	}, nil)

	// Trip the circuit (threshold = 1, so one Do failure opens it).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, _ = f.Do(context.Background(), testTenant, agentID, "", req)

	// Verify circuit is open.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if !errors.Is(err, proxy.ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen immediately after tripping; got %v", err)
	}

	// Wait for the cooldown to elapse.
	time.Sleep(cooldown + 5*time.Millisecond)

	// Switch upstream to healthy, then send the probe.
	failMode.Store(false)
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	result, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if err != nil {
		t.Fatalf("probe after cooldown: %v (want success)", err)
	}
	_ = result.Body.Close()

	// Circuit should now be closed — further requests succeed.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	result, err = f.Do(context.Background(), testTenant, agentID, "", req)
	if err != nil {
		t.Fatalf("request after circuit closed: %v", err)
	}
	_ = result.Body.Close()
}

// TestNonTransientStatusNotRetried verifies that a non-transient upstream
// status (e.g. 400 Bad Request) is returned as-is without retrying.
func TestNonTransientStatusNotRetried(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusBadRequest) // 400 — not transient
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       2,
		RetryBaseDelay:   1 * time.Millisecond,
		BreakerThreshold: 100,
		HTTPClient:       upstream.Client(),
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	result, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	if got := callCount.Load(); got != 1 {
		t.Errorf("call count = %d, want 1 (non-transient status must not be retried)", got)
	}
	if result.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", result.StatusCode, http.StatusBadRequest)
	}
}

// TestDNSRebindingRejected verifies that a proxied request is refused when the
// upstream host resolves to a blocked IP address (DNS-rebinding SSRF mitigation,
// issue #51). The agent is seeded directly — bypassing httpapi.validateUpstreamURL
// — to simulate a hostname that was legitimate at store time but now resolves to
// an internal address. The forwarder's safeDialContext must catch this at
// connection time before any data is sent.
func TestDNSRebindingRejected(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	// Seed with a loopback URL, bypassing input validation to simulate a
	// DNS-rebinding scenario: the hostname was accepted at CRUD time but the
	// resolved address is now in a blocked range.
	agentID := seedAgent(t, store, "http://localhost:19999/", nil)

	// Use a nil HTTPClient so the forwarder constructs its real transport with
	// safeDialContext — the path under test.
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       0,
		BreakerThreshold: 100,
		BreakerCooldown:  1 * time.Hour,
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	_, err := f.Do(context.Background(), testTenant, agentID, "inv_ssrf_test", req)
	if err == nil {
		t.Fatal("Do: expected error for loopback upstream, got nil")
	}
	if !errors.Is(err, proxy.ErrSSRFBlocked) {
		t.Errorf("Do: err = %v, want ErrSSRFBlocked (SSRF blocked)", err)
	}
}

// TestDNSRebindingRejectedStream is the streaming counterpart to
// TestDNSRebindingRejected: streamClient has its own transport, so the dialer
// must be asserted on the DoStream path independently — a refactor that drops
// safeDialContext from streamClient would otherwise go uncaught.
func TestDNSRebindingRejectedStream(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, "http://localhost:19999/", nil)

	// nil HTTPClient → the forwarder builds its real streaming transport with
	// safeDialContext, the path under test.
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       0,
		BreakerThreshold: 100,
		BreakerCooldown:  1 * time.Hour,
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	_, err := f.DoStream(context.Background(), testTenant, agentID, "inv_ssrf_stream_test", req)
	if err == nil {
		t.Fatal("DoStream: expected error for loopback upstream, got nil")
	}
	if !errors.Is(err, proxy.ErrSSRFBlocked) {
		t.Errorf("DoStream: err = %v, want ErrSSRFBlocked (SSRF blocked)", err)
	}
}

// TestMCPUpstreamDialTimeSSRFRejected verifies that safeDialContext /
// ssrf.IsBlockedIP reject a blocked-range address for a Protocol=mcp agent's
// upstream_url exactly as they do for an HTTP agent (spec 0004 decision 10):
// Do must return ErrSSRFBlocked, and no bypass exists for the mcp protocol.
func TestMCPUpstreamDialTimeSSRFRejected(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedMCPAgent(t, store, "http://localhost:19999/")

	// nil HTTPClient so the forwarder builds its real transport with
	// safeDialContext — the path under test.
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       0,
		BreakerThreshold: 100,
		BreakerCooldown:  1 * time.Hour,
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	_, err := f.Do(context.Background(), testTenant, agentID, "inv_mcp_ssrf_test", req)
	if err == nil {
		t.Fatal("Do: expected error for loopback mcp upstream_url, got nil")
	}
	if !errors.Is(err, proxy.ErrSSRFBlocked) {
		t.Errorf("Do: err = %v, want ErrSSRFBlocked (SSRF blocked)", err)
	}
}

// TestMCPUpstreamDialTimeSSRFRejectedStream is the streaming counterpart to
// TestMCPUpstreamDialTimeSSRFRejected: an MCP agent using the streamable-HTTP
// transport (spec 0004) that upgrades to an SSE stream goes through DoStream,
// which has its own transport — asserted independently so a refactor that drops
// safeDialContext from streamClient would not go uncaught for MCP traffic.
func TestMCPUpstreamDialTimeSSRFRejectedStream(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedMCPAgent(t, store, "http://localhost:19999/")

	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       0,
		BreakerThreshold: 100,
		BreakerCooldown:  1 * time.Hour,
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_docs"}}`))
	_, err := f.DoStream(context.Background(), testTenant, agentID, "inv_mcp_ssrf_stream_test", req)
	if err == nil {
		t.Fatal("DoStream: expected error for loopback mcp upstream_url, got nil")
	}
	if !errors.Is(err, proxy.ErrSSRFBlocked) {
		t.Errorf("DoStream: err = %v, want ErrSSRFBlocked (SSRF blocked)", err)
	}
}

// TestSSRFBlockDoesNotTripBreaker verifies an SSRF-blocked dial is a
// deterministic rejection, not an upstream-health failure: it is not retried and
// does not increment the circuit breaker. With BreakerThreshold=1, if a block
// counted as a failure the breaker would open and the second call would return
// ErrCircuitOpen instead of ErrSSRFBlocked.
func TestSSRFBlockDoesNotTripBreaker(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, "http://localhost:19999/", nil)
	f := proxy.New(store, &stubCredSvc{}, proxy.Config{
		MaxRetries:       2,
		BreakerThreshold: 1,
		BreakerCooldown:  1 * time.Hour,
	}, nil)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
		_, err := f.Do(context.Background(), testTenant, agentID, "inv_ssrf_breaker", req)
		if errors.Is(err, proxy.ErrCircuitOpen) {
			t.Fatalf("call %d: circuit opened on an SSRF block — a block must not count as an upstream failure", i)
		}
		if !errors.Is(err, proxy.ErrSSRFBlocked) {
			t.Fatalf("call %d: err = %v, want ErrSSRFBlocked", i, err)
		}
	}
}

// TestCallerHeadersForwarded verifies that non-blocked caller headers
// (e.g. Content-Type) reach the upstream.
func TestCallerHeadersForwarded(t *testing.T) {
	t.Parallel()

	var upstreamCT string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")

	result, err := f.Do(context.Background(), testTenant, agentID, "", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	if upstreamCT != "application/json" {
		t.Errorf("upstream Content-Type = %q, want application/json", upstreamCT)
	}
}

// TestMCPSessionIDRoundTripDo verifies the MCP Streamable-HTTP session
// correlation header round-trips through Forwarder.Do in both directions
// (spec 0004 decision 4/5, issue #92): the caller's Mcp-Session-Id reaches the
// upstream unchanged, and the upstream's Mcp-Session-Id (e.g. assigned on an
// initialize response) reaches back out in Result.Header. Do has no
// protocol-aware branch — this exercises the transparent pipe for a
// Protocol=mcp agent specifically so a future change that special-cases mcp
// routing cannot silently drop the header either direction.
func TestMCPSessionIDRoundTripDo(t *testing.T) {
	t.Parallel()

	const callerSessionID = "9d3f2b1a-caller"
	const serverSessionID = "9d3f2b1a-server-assigned"

	var upstreamGotSessionID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamGotSessionID = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Mcp-Session-Id", serverSessionID)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedMCPAgent(t, store, upstream.URL)
	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Mcp-Session-Id", callerSessionID)

	result, err := f.Do(context.Background(), testTenant, agentID, "inv_mcp_session_test", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	if upstreamGotSessionID != callerSessionID {
		t.Errorf("upstream received Mcp-Session-Id = %q, want %q", upstreamGotSessionID, callerSessionID)
	}
	if got := result.Header.Get("Mcp-Session-Id"); got != serverSessionID {
		t.Errorf("Result.Header Mcp-Session-Id = %q, want %q", got, serverSessionID)
	}
}

// TestMCPSessionIDRoundTripDoStream is TestMCPSessionIDRoundTripDo's streaming
// counterpart, exercising DoStream — the path a tools/call SSE upgrade uses
// (spec 0004) — independently, since DoStream has its own request-build and
// response-header path.
func TestMCPSessionIDRoundTripDoStream(t *testing.T) {
	t.Parallel()

	const callerSessionID = "9d3f2b1a-caller-stream"
	const serverSessionID = "9d3f2b1a-server-assigned-stream"

	var upstreamGotSessionID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamGotSessionID = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Mcp-Session-Id", serverSessionID)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n"))
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedMCPAgent(t, store, upstream.URL)
	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_docs"}}`))
	req.Header.Set("Mcp-Session-Id", callerSessionID)

	result, err := f.DoStream(context.Background(), testTenant, agentID, "inv_mcp_session_stream_test", req)
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	_ = result.Body.Close()

	if upstreamGotSessionID != callerSessionID {
		t.Errorf("upstream received Mcp-Session-Id = %q, want %q", upstreamGotSessionID, callerSessionID)
	}
	if got := result.Header.Get("Mcp-Session-Id"); got != serverSessionID {
		t.Errorf("Result.Header Mcp-Session-Id = %q, want %q", got, serverSessionID)
	}
}

// TestSessionIDRoundTripUnaffectedForHTTPProtocol is a regression guard for
// spec 0004's "Protocol=http agents: zero behavior change" requirement
// (issue #92 acceptance): it repeats TestMCPSessionIDRoundTripDo's exact
// scenario against a plain Protocol=http agent (seedAgent, no Protocol/
// MCPTransport set) to prove header round-tripping was never protocol-gated —
// Do has no mcp-specific branch to regress.
func TestSessionIDRoundTripUnaffectedForHTTPProtocol(t *testing.T) {
	t.Parallel()

	const callerSessionID = "http-agent-caller-session"
	const serverSessionID = "http-agent-server-session"

	var upstreamGotSessionID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamGotSessionID = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Mcp-Session-Id", serverSessionID)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	store := agent.NewMemoryStore()
	agentID := seedAgent(t, store, upstream.URL, nil)
	f := proxy.New(store, &stubCredSvc{}, noRetryConfig(upstream.Client()), nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"prompt":"not mcp at all"}`))
	req.Header.Set("Mcp-Session-Id", callerSessionID)

	result, err := f.Do(context.Background(), testTenant, agentID, "inv_http_session_test", req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = result.Body.Close()

	if upstreamGotSessionID != callerSessionID {
		t.Errorf("upstream received Mcp-Session-Id = %q, want %q", upstreamGotSessionID, callerSessionID)
	}
	if got := result.Header.Get("Mcp-Session-Id"); got != serverSessionID {
		t.Errorf("Result.Header Mcp-Session-Id = %q, want %q", got, serverSessionID)
	}
}
