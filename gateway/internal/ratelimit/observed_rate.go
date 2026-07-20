package ratelimit

import (
	"sync"
	"time"
)

// ObservedRateWindow is the trailing duration ObservedRateTracker counts
// requests over, matching the granularity of a policy rate_per_min condition
// (spec 0009 fast-follow, #212).
const ObservedRateWindow = time.Minute

// callerHistory holds one caller's request timestamps within the trailing
// window, plus the last time any request was observed (for eviction).
type callerHistory struct {
	times    []time.Time
	lastSeen time.Time
}

// ObservedRateTracker records each caller's request timestamps and reports
// their observed rate over a trailing one-minute window, keyed the same way
// as AgentLimiter: (tenantID, agentID). Unlike AgentLimiter (a token bucket
// that only answers allow/deny), it answers "how many requests has this
// caller made in the last minute" — the value the policy enforcement point
// needs to populate policy.RequestContext.RatePerMin, whose doc comment
// requires it be "precomputed by the caller from the existing rate-limit
// machinery" (spec 0009 fast-follow, #212).
//
// Create with NewObservedRateTracker; call Close when done to stop
// background eviction.
type ObservedRateTracker struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]*callerHistory
	now     func() time.Time
	done    chan struct{}
}

// NewObservedRateTracker returns an ObservedRateTracker that evicts idle
// entries after ttl. If ttl is zero or negative, DefaultTTL is used. A
// background goroutine runs the eviction loop; stop it with Close.
func NewObservedRateTracker(ttl time.Duration) *ObservedRateTracker {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	t := &ObservedRateTracker{
		ttl:     ttl,
		entries: make(map[string]*callerHistory),
		now:     time.Now,
		done:    make(chan struct{}),
	}
	go t.runCleanup()
	return t
}

// Observe records a request for (tenantID, agentID) as of now and returns the
// number of requests observed for that caller within the trailing
// ObservedRateWindow, including this one.
func (t *ObservedRateTracker) Observe(tenantID, agentID string) int {
	key := tenantID + "\x00" + agentID
	now := t.now()
	cutoff := now.Add(-ObservedRateWindow)

	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[key]
	if !ok {
		e = &callerHistory{}
		t.entries[key] = e
	}

	kept := e.times[:0]
	for _, ts := range e.times {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	e.times = kept
	e.lastSeen = now
	return len(e.times)
}

// Close stops the background eviction goroutine. Safe to call more than once.
func (t *ObservedRateTracker) Close() {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
}

func (t *ObservedRateTracker) cleanup() {
	cutoff := t.now().Add(-t.ttl)
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, e := range t.entries {
		if e.lastSeen.Before(cutoff) {
			delete(t.entries, key)
		}
	}
}

func (t *ObservedRateTracker) runCleanup() {
	ticker := time.NewTicker(t.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.cleanup()
		case <-t.done:
			return
		}
	}
}
