package ratelimit

import "time"

// SetNow replaces the clock function used by m. Call this in tests to freeze
// or advance time without real wall-clock sleeps.
func SetNow(m *Middleware, fn func() time.Time) {
	m.now = fn
}

// ForceCleanup runs the eviction pass synchronously. Use in tests to verify
// that idle entries are removed without waiting for the background ticker.
func ForceCleanup(m *Middleware) {
	m.cleanup()
}

// SetAgentNow replaces the clock function used by l.
func SetAgentNow(l *AgentLimiter, fn func() time.Time) {
	l.now = fn
}

// ForceAgentCleanup runs the eviction pass on l synchronously.
func ForceAgentCleanup(l *AgentLimiter) {
	l.cleanup()
}

// SetObservedRateNow replaces the clock function used by t.
func SetObservedRateNow(t *ObservedRateTracker, fn func() time.Time) {
	t.now = fn
}

// ForceObservedRateCleanup runs the eviction pass on t synchronously.
func ForceObservedRateCleanup(t *ObservedRateTracker) {
	t.cleanup()
}
