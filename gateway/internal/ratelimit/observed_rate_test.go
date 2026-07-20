package ratelimit_test

import (
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/ratelimit"
)

func TestObservedRateTracker_CountsWithinWindow(t *testing.T) {
	t.Parallel()

	var now time.Time
	now = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tr := ratelimit.NewObservedRateTracker(time.Hour)
	t.Cleanup(tr.Close)
	ratelimit.SetObservedRateNow(tr, clock)

	if got := tr.Observe("tenant-a", "agent-1"); got != 1 {
		t.Errorf("1st observe: got %d, want 1", got)
	}
	now = now.Add(10 * time.Second)
	if got := tr.Observe("tenant-a", "agent-1"); got != 2 {
		t.Errorf("2nd observe: got %d, want 2", got)
	}
	now = now.Add(10 * time.Second)
	if got := tr.Observe("tenant-a", "agent-1"); got != 3 {
		t.Errorf("3rd observe: got %d, want 3", got)
	}
}

func TestObservedRateTracker_DropsRequestsOutsideWindow(t *testing.T) {
	t.Parallel()

	var now time.Time
	now = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tr := ratelimit.NewObservedRateTracker(time.Hour)
	t.Cleanup(tr.Close)
	ratelimit.SetObservedRateNow(tr, clock)

	tr.Observe("tenant-a", "agent-1")
	now = now.Add(30 * time.Second)
	tr.Observe("tenant-a", "agent-1")

	// Advance past the one-minute window relative to the first two requests;
	// only this third request should remain in the count.
	now = now.Add(65 * time.Second)
	if got := tr.Observe("tenant-a", "agent-1"); got != 1 {
		t.Errorf("got %d, want 1 (earlier requests aged out of the window)", got)
	}
}

func TestObservedRateTracker_IndependentCallers(t *testing.T) {
	t.Parallel()

	tr := ratelimit.NewObservedRateTracker(ratelimit.DefaultTTL)
	t.Cleanup(tr.Close)

	tr.Observe("tenant-a", "agent-1")
	tr.Observe("tenant-a", "agent-1")

	if got := tr.Observe("tenant-a", "agent-2"); got != 1 {
		t.Errorf("agent-2: got %d, want 1 (independent history)", got)
	}
	if got := tr.Observe("tenant-b", "agent-1"); got != 1 {
		t.Errorf("tenant-b/agent-1: got %d, want 1 (cross-tenant independence)", got)
	}
}

func TestObservedRateTracker_Eviction(t *testing.T) {
	t.Parallel()

	var now time.Time
	now = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tr := ratelimit.NewObservedRateTracker(time.Hour)
	t.Cleanup(tr.Close)
	ratelimit.SetObservedRateNow(tr, clock)

	tr.Observe("tenant-a", "agent-1")
	tr.Observe("tenant-a", "agent-1")

	now = now.Add(2 * time.Hour)
	ratelimit.ForceObservedRateCleanup(tr)

	// After eviction the entry is gone; the next call starts a fresh history.
	if got := tr.Observe("tenant-a", "agent-1"); got != 1 {
		t.Errorf("post-eviction observe: got %d, want 1 (fresh history)", got)
	}
}

func TestObservedRateTracker_CloseIsIdempotent(t *testing.T) {
	t.Parallel()

	tr := ratelimit.NewObservedRateTracker(ratelimit.DefaultTTL)
	tr.Close()
	tr.Close() // must not panic
}
