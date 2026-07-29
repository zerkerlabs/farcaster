package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/zerkerlabs/farcaster/rooms/internal/auth"
	"github.com/zerkerlabs/farcaster/rooms/internal/auth/authtest"
	"github.com/zerkerlabs/farcaster/rooms/internal/gateway"
	"github.com/zerkerlabs/farcaster/rooms/internal/httpapi"
	"github.com/zerkerlabs/farcaster/rooms/internal/memory"
	"github.com/zerkerlabs/farcaster/rooms/internal/receipt"
	"github.com/zerkerlabs/farcaster/rooms/internal/room"
)

const (
	tenantA = "tenant-alpha"
	tenantB = "tenant-beta"

	// testAudience, tenantClaim, and userClaim are the OIDC parameters every
	// handler test's tokens and middleware agree on.
	testAudience = "rooms-test-audience"
	tenantClaim  = "org_id"
	userClaim    = "sub"
)

// oidc issues the tokens handler tests authenticate with, and authMiddleware
// validates them exactly as the server does. Both are per-package rather than
// per-test: an RSA keypair and OIDC discovery per mux would dominate the
// runtime of a suite that builds one for nearly every case.
var (
	oidc           *authtest.Server
	authMiddleware func(http.Handler) http.Handler
)

func TestMain(m *testing.M) {
	oidc = authtest.New()

	mw, err := auth.NewMiddleware(context.Background(), auth.Config{
		IssuerURL:   oidc.URL,
		Audience:    testAudience,
		TenantClaim: tenantClaim,
		UserClaim:   userClaim,
		HTTPClient:  oidc.Client(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		oidc.Close()
		panic("init auth middleware: " + err.Error())
	}
	authMiddleware = mw

	code := m.Run()
	oidc.Close()
	os.Exit(code)
}

// unreachableGateway is a httpapi.GatewayCaller for tests that never exercise
// addressed messaging: any call fails the test loudly rather than silently
// pretending to deliver, so an unexpected gateway call doesn't go unnoticed.
type unreachableGateway struct{ t *testing.T }

func (g unreachableGateway) Call(_ context.Context, _, agentID string, _ []byte) (*gateway.Result, error) {
	g.t.Helper()
	g.t.Fatalf("unexpected gateway call to %q", agentID)
	return nil, nil
}

// newMux returns a handler serving the four v1 room routes behind the real
// auth middleware, backed by a fresh in-memory room store and a fresh
// in-memory memory store.
func newMux(t *testing.T) (http.Handler, *room.Store) {
	t.Helper()
	return newMuxWithMemory(t, memory.NewFake())
}

// newMuxWithMemory is newMux with a caller-supplied memory.Store, for tests
// that need to seed onboarding entries or exercise a failing memory backend.
func newMuxWithMemory(t *testing.T, memoryStore memory.Store) (http.Handler, *room.Store) {
	t.Helper()
	return newMuxWithMemoryAndGateway(t, memoryStore, unreachableGateway{t})
}

// newMuxWithMemoryAndGateway is newMuxWithMemory with a caller-supplied
// httpapi.GatewayCaller, for tests exercising addressed-message delivery. It
// backs the handler with a fresh receipt.Fake that no test here inspects —
// tests that need to observe or override receipt emission use
// newMuxWithReceipts instead.
//
// The router is wrapped in the same middleware main.go wraps it in, so every
// handler test reaches its handler by presenting a real bearer token — the
// tenant a handler sees is the one a signed token asserted, never one a test
// wrote onto the context directly.
func newMuxWithMemoryAndGateway(t *testing.T, memoryStore memory.Store, gatewayClient httpapi.GatewayCaller) (http.Handler, *room.Store) {
	t.Helper()
	mux, store, _ := newMuxWithReceipts(t, memoryStore, gatewayClient, receipt.NewFake())
	return mux, store
}

// newMuxWithReceipts is newMuxWithMemoryAndGateway with a caller-supplied
// receipt.Emitter, for tests that observe what an addressed message emits or
// exercise the async fail-open emission behaviour. It also returns the
// *httpapi.Handler itself so a test can call Shutdown.
func newMuxWithReceipts(t *testing.T, memoryStore memory.Store, gatewayClient httpapi.GatewayCaller, emitter receipt.Emitter) (http.Handler, *room.Store, *httpapi.Handler) {
	t.Helper()
	store := room.NewStore()
	h := httpapi.NewHandler(store, memoryStore, gatewayClient, emitter, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return authMiddleware(mux), store, h
}

// requestAs returns a request to path with body, authenticated as tenantID
// with a freshly minted bearer token. An empty tenantID sends no Authorization
// header, which the middleware refuses. A nil body sends no request body; a
// []byte body is sent raw (for malformed-JSON cases); any other value is
// JSON-marshaled.
func requestAs(t *testing.T, method, path string, body any, tenantID string) *http.Request {
	t.Helper()
	req := request(t, method, path, body)
	if tenantID != "" {
		req.Header.Set("Authorization", "Bearer "+mintToken(t, oidc.Claims(testAudience, tenantClaim, tenantID, userClaim, "user-"+tenantID)))
	}
	return req
}

// request builds an unauthenticated request; requestAs adds the token.
func request(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
		rdr = nil
	case []byte:
		rdr = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// mintToken signs claims with the test OIDC server's key.
func mintToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	tok, err := oidc.Mint(claims)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body: %v; raw = %s", err, rec.Body.String())
	}
	return body
}

// mustCreateRoom creates a room under tenantA, the tenant every handler test
// treats as the room's owner; cross-tenant cases look the room up as tenantB.
func mustCreateRoom(t *testing.T, s *room.Store, goal string) *room.Room {
	t.Helper()
	r, err := s.CreateRoom(context.Background(), tenantA, goal)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	return r
}

func mustAddMember(t *testing.T, s *room.Store, roomID, agentID string) *room.Member {
	t.Helper()
	m, err := s.AddMember(context.Background(), tenantA, roomID, agentID, tenantA, nil)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	return m
}
