package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// agentEntry pairs a rate limiter with the configuration it was built from and
// the last time it was accessed (for eviction).
type agentEntry struct {
	limiter  *rate.Limiter
	cfgRate  rate.Limit
	cfgBurst int
	lastSeen time.Time
}

// AgentLimiter maintains per-agent token-bucket rate limiters, each configured
// with its own rate and burst. Unlike Middleware (which applies a single global
// rate to all callers), AgentLimiter lets each agent carry its own invocation
// rate limit that overrides the tenant global (spec 0002 Q11/Q12).
//
// Create with NewAgentLimiter; call Close when done to stop background eviction.
type AgentLimiter struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]*agentEntry
	now     func() time.Time
	done    chan struct{}
}

// NewAgentLimiter returns an AgentLimiter that evicts idle entries after ttl.
// If ttl is zero or negative, DefaultTTL is used. A background goroutine runs
// the eviction loop; stop it with Close.
func NewAgentLimiter(ttl time.Duration) *AgentLimiter {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	l := &AgentLimiter{
		ttl:     ttl,
		entries: make(map[string]*agentEntry),
		now:     time.Now,
		done:    make(chan struct{}),
	}
	go l.runCleanup()
	return l
}

// Allow checks the per-agent rate limit keyed on (tenantID, agentID). r is the
// configured rate in requests per second; burst is the maximum burst depth. If
// the stored limiter's parameters differ from r/burst (configuration changed),
// the limiter is recreated. Returns 0 if the request is allowed immediately,
// or the delay until the next token becomes available.
func (l *AgentLimiter) Allow(tenantID, agentID string, r float64, burst int) time.Duration {
	key := tenantID + "\x00" + agentID
	rl := rate.Limit(r)

	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok || e.cfgRate != rl || e.cfgBurst != burst {
		e = &agentEntry{
			limiter:  rate.NewLimiter(rl, burst),
			cfgRate:  rl,
			cfgBurst: burst,
		}
		l.entries[key] = e
	}
	e.lastSeen = l.now()

	rsv := e.limiter.Reserve()
	if delay := rsv.Delay(); delay > 0 {
		rsv.Cancel()
		return delay
	}
	return 0
}

// Close stops the background eviction goroutine. Safe to call more than once.
func (l *AgentLimiter) Close() {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
}

func (l *AgentLimiter) cleanup() {
	cutoff := l.now().Add(-l.ttl)
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, e := range l.entries {
		if e.lastSeen.Before(cutoff) {
			delete(l.entries, key)
		}
	}
}

func (l *AgentLimiter) runCleanup() {
	ticker := time.NewTicker(l.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.cleanup()
		case <-l.done:
			return
		}
	}
}
