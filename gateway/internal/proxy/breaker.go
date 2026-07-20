package proxy

import (
	"sync"
	"time"
)

// breakerState is the state of a per-upstream circuit breaker.
type breakerState int

const (
	breakerClosed   breakerState = iota // normal operation; requests flow through
	breakerOpen                         // upstream is failing; requests are rejected
	breakerHalfOpen                     // one probe request is allowed; outcome determines next state
)

// breaker is a single per-upstream circuit breaker. It opens after threshold
// consecutive Do failures, then enters half-open after cooldown to allow one
// probe. A successful probe closes the circuit; a failed probe re-opens it.
//
// All methods are safe for concurrent use.
type breaker struct {
	mu        sync.Mutex
	state     breakerState
	failures  int
	threshold int
	cooldown  time.Duration
	openedAt  time.Time
}

// allow reports whether a request may proceed. Returns true when the circuit is
// closed or when the cooldown has elapsed (transitioning to half-open for a
// single probe). Returns false when the circuit is open and the cooldown has not
// elapsed, or when the circuit is already half-open with a probe in flight.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerClosed:
		return true
	case breakerOpen:
		if time.Since(b.openedAt) >= b.cooldown {
			b.state = breakerHalfOpen
			return true
		}
		return false
	case breakerHalfOpen:
		// Only one concurrent probe; deny additional requests while probing.
		return false
	default:
		return false
	}
}

// success records a successful upstream call. If the circuit was half-open (the
// probe succeeded), it transitions to closed and resets the failure counter.
func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.state = breakerClosed
}

// failure records a failed upstream Do call. The circuit opens immediately if
// the breaker was half-open (the probe failed) or if the failure count reaches
// the threshold.
func (b *breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.state == breakerHalfOpen || b.failures >= b.threshold {
		b.state = breakerOpen
		b.openedAt = time.Now()
	}
}

// breakerRegistry holds one breaker per upstream URL, creating entries on
// demand. It is safe for concurrent use.
type breakerRegistry struct {
	mu        sync.Mutex
	breakers  map[string]*breaker
	threshold int
	cooldown  time.Duration
}

func newBreakerRegistry(threshold int, cooldown time.Duration) *breakerRegistry {
	return &breakerRegistry{
		breakers:  make(map[string]*breaker),
		threshold: threshold,
		cooldown:  cooldown,
	}
}

// get returns the circuit breaker for upstreamURL, creating it on first access.
func (r *breakerRegistry) get(upstreamURL string) *breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.breakers[upstreamURL]
	if !ok {
		b = &breaker{
			threshold: r.threshold,
			cooldown:  r.cooldown,
		}
		r.breakers[upstreamURL] = b
	}
	return b
}
