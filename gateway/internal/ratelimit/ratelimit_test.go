package ratelimit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/farcaster/gateway/internal/ratelimit"
)

// ctxFor returns a context with the given tenant and user set via the authtest
// package, simulating an authenticated request that passed through the auth
// middleware.
func ctxFor(tenant, user string) context.Context {
	return authtest.WithIdentity(context.Background(), tenant, user)
}

// newPassthrough returns an http.Handler that always responds 200 OK.
func newPassthrough() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestMiddleware_UnderLimit(t *testing.T) {
	t.Parallel()

	// Burst of 5 — the first 5 requests must all pass immediately.
	m := ratelimit.New(ratelimit.Config{Rate: 100, Burst: 5})
	t.Cleanup(m.Close)
	h := m.Handler()(newPassthrough())
	ctx := ctxFor("tenant-a", "user-a")

	for i := range 5 {
		req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: got %d, want 200", i+1, rec.Code)
		}
	}
}

func TestMiddleware_OverLimit(t *testing.T) {
	t.Parallel()

	// Burst of 1, low rate — the second immediate request must be rejected.
	m := ratelimit.New(ratelimit.Config{Rate: 1, Burst: 1})
	t.Cleanup(m.Close)
	h := m.Handler()(newPassthrough())
	ctx := ctxFor("tenant-a", "user-a")

	first := httptest.NewRequest(http.MethodGet, "/v1/agents", nil).WithContext(ctx)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, first)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rec1.Code)
	}

	second := httptest.NewRequest(http.MethodGet, "/v1/agents", nil).WithContext(ctx)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, second)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429", rec2.Code)
	}
	retryAfter := rec2.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("second request: missing Retry-After header")
	}
	secs, err := strconv.Atoi(retryAfter)
	if err != nil {
		t.Fatalf("Retry-After %q is not an integer: %v", retryAfter, err)
	}
	if secs < 1 {
		t.Errorf("Retry-After = %d, want >= 1", secs)
	}
}

func TestMiddleware_IndependentCallers(t *testing.T) {
	t.Parallel()

	// Burst of 1 so each caller's bucket is exhausted after one request.
	m := ratelimit.New(ratelimit.Config{Rate: 1, Burst: 1})
	t.Cleanup(m.Close)
	h := m.Handler()(newPassthrough())

	ctxA := ctxFor("tenant-a", "user-a")
	ctxB := ctxFor("tenant-b", "user-b")

	// Exhaust caller A's bucket.
	reqA1 := httptest.NewRequest(http.MethodGet, "/v1/agents", nil).WithContext(ctxA)
	recA1 := httptest.NewRecorder()
	h.ServeHTTP(recA1, reqA1)
	if recA1.Code != http.StatusOK {
		t.Fatalf("caller A first request: got %d, want 200", recA1.Code)
	}

	reqA2 := httptest.NewRequest(http.MethodGet, "/v1/agents", nil).WithContext(ctxA)
	recA2 := httptest.NewRecorder()
	h.ServeHTTP(recA2, reqA2)
	if recA2.Code != http.StatusTooManyRequests {
		t.Fatalf("caller A second request: got %d, want 429", recA2.Code)
	}

	// Caller B's first request must still succeed — buckets are independent.
	reqB1 := httptest.NewRequest(http.MethodGet, "/v1/agents", nil).WithContext(ctxB)
	recB1 := httptest.NewRecorder()
	h.ServeHTTP(recB1, reqB1)
	if recB1.Code != http.StatusOK {
		t.Errorf("caller B first request: got %d, want 200 (per-caller buckets must be independent)", recB1.Code)
	}
}

func TestMiddleware_ExemptPaths(t *testing.T) {
	t.Parallel()

	// Near-zero rate — any non-exempt path would be rate-limited on the
	// second request, but /healthz and /version must never be.
	m := ratelimit.New(ratelimit.Config{Rate: 0.001, Burst: 1})
	t.Cleanup(m.Close)
	h := m.Handler()(newPassthrough())

	for _, path := range []string{"/healthz", "/version"} {
		for i := range 5 {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("path %s request %d: got %d, want 200", path, i+1, rec.Code)
			}
		}
	}
}

func TestMiddleware_UnauthenticatedReturns401(t *testing.T) {
	t.Parallel()

	// A request with no auth context (empty tenant and user) must be rejected
	// with 401, not leaked into a shared catch-all bucket.
	m := ratelimit.New(ratelimit.Config{Rate: 100, Burst: 100})
	t.Cleanup(m.Close)
	h := m.Handler()(newPassthrough())

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated request: got %d, want 401", rec.Code)
	}
}

func TestMiddleware_Eviction(t *testing.T) {
	t.Parallel()

	// Use an injectable clock to control time without wall-clock sleeps.
	var now time.Time
	now = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	m := ratelimit.New(ratelimit.Config{Rate: 100, Burst: 100, TTL: time.Hour})
	t.Cleanup(m.Close)
	ratelimit.SetNow(m, clock)

	h := m.Handler()(newPassthrough())
	ctx := ctxFor("tenant-evict", "user-evict")

	// Send a request to create the entry.
	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Advance clock past the TTL so the entry is stale.
	now = now.Add(2 * time.Hour)

	// Force the cleanup pass — the stale entry must be removed.
	ratelimit.ForceCleanup(m)

	// Send another request: the middleware must create a fresh limiter (burst
	// is reset), so this request must succeed even though the previous burst
	// was consumed.  To observe the reset we use burst=1.
	m2 := ratelimit.New(ratelimit.Config{Rate: 1, Burst: 1, TTL: time.Hour})
	t.Cleanup(m2.Close)
	ratelimit.SetNow(m2, clock)
	h2 := m2.Handler()(newPassthrough())

	// Use up the burst.
	req1 := httptest.NewRequest(http.MethodGet, "/v1/agents", nil).WithContext(ctx)
	h2.ServeHTTP(httptest.NewRecorder(), req1)

	// Advance time and evict — resets the limiter state.
	now = now.Add(2 * time.Hour)
	ratelimit.ForceCleanup(m2)

	// After eviction the entry is gone; a new request gets a fresh bucket.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/agents", nil).WithContext(ctx)
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("post-eviction request: got %d, want 200 (evicted entry should have a fresh limiter)", rec2.Code)
	}
}
