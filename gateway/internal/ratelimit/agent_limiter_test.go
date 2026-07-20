package ratelimit_test

import (
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/ratelimit"
)

func TestAgentLimiter_AllowsFirstRequest(t *testing.T) {
	t.Parallel()

	l := ratelimit.NewAgentLimiter(ratelimit.DefaultTTL)
	t.Cleanup(l.Close)

	// Burst of 1 — first request must always be allowed.
	delay := l.Allow("tenant-a", "agent-1", 1.0, 1)
	if delay != 0 {
		t.Errorf("first request: got delay %v, want 0", delay)
	}
}

func TestAgentLimiter_BlocksAfterBurst(t *testing.T) {
	t.Parallel()

	l := ratelimit.NewAgentLimiter(ratelimit.DefaultTTL)
	t.Cleanup(l.Close)

	// Burst 1, rate 1/s — second immediate request must be delayed.
	l.Allow("tenant-a", "agent-1", 1.0, 1)
	delay := l.Allow("tenant-a", "agent-1", 1.0, 1)
	if delay <= 0 {
		t.Errorf("second request: got delay %v, want > 0", delay)
	}
}

func TestAgentLimiter_IndependentAgents(t *testing.T) {
	t.Parallel()

	l := ratelimit.NewAgentLimiter(ratelimit.DefaultTTL)
	t.Cleanup(l.Close)

	// Exhaust agent-1's burst, then verify agent-2 still has its own full burst.
	l.Allow("tenant-a", "agent-1", 1.0, 1)
	l.Allow("tenant-a", "agent-1", 1.0, 1) // agent-1 exhausted

	delay := l.Allow("tenant-a", "agent-2", 1.0, 1)
	if delay != 0 {
		t.Errorf("agent-2 first request: got delay %v, want 0 (independent bucket)", delay)
	}
}

func TestAgentLimiter_CrossTenantIndependence(t *testing.T) {
	t.Parallel()

	l := ratelimit.NewAgentLimiter(ratelimit.DefaultTTL)
	t.Cleanup(l.Close)

	// Same agent ID in different tenants must have independent buckets.
	l.Allow("tenant-a", "agent-x", 1.0, 1)
	l.Allow("tenant-a", "agent-x", 1.0, 1) // tenant-a exhausted

	delay := l.Allow("tenant-b", "agent-x", 1.0, 1)
	if delay != 0 {
		t.Errorf("tenant-b first request: got delay %v, want 0 (cross-tenant independence)", delay)
	}
}

func TestAgentLimiter_ReconfiguresOnRateChange(t *testing.T) {
	t.Parallel()

	l := ratelimit.NewAgentLimiter(ratelimit.DefaultTTL)
	t.Cleanup(l.Close)

	// First call with rate=0.001 (very slow) exhausts the burst.
	l.Allow("tenant-a", "agent-1", 0.001, 1)
	delay := l.Allow("tenant-a", "agent-1", 0.001, 1)
	if delay <= 0 {
		t.Fatal("expected delay with rate=0.001 after burst exhausted")
	}

	// Changing the rate to a fast rate should recreate the limiter with a
	// fresh burst. The first request under the new config must be allowed.
	delay2 := l.Allow("tenant-a", "agent-1", 1000.0, 10)
	if delay2 != 0 {
		t.Errorf("first request after rate change: got delay %v, want 0 (limiter recreated)", delay2)
	}
}

func TestAgentLimiter_Eviction(t *testing.T) {
	t.Parallel()

	var now time.Time
	now = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	l := ratelimit.NewAgentLimiter(time.Hour)
	t.Cleanup(l.Close)
	ratelimit.SetAgentNow(l, clock)

	// Exhaust the burst.
	l.Allow("tenant-a", "agent-1", 1.0, 1)
	delay := l.Allow("tenant-a", "agent-1", 1.0, 1)
	if delay <= 0 {
		t.Fatal("expected delay after burst exhausted")
	}

	// Advance clock past the TTL, then evict.
	now = now.Add(2 * time.Hour)
	ratelimit.ForceAgentCleanup(l)

	// After eviction the entry is gone; next call gets a fresh limiter.
	delay2 := l.Allow("tenant-a", "agent-1", 1.0, 1)
	if delay2 != 0 {
		t.Errorf("post-eviction request: got delay %v, want 0 (fresh limiter after eviction)", delay2)
	}
}

func TestAgentLimiter_CloseIsIdempotent(t *testing.T) {
	t.Parallel()

	l := ratelimit.NewAgentLimiter(ratelimit.DefaultTTL)
	l.Close()
	l.Close() // must not panic
}
