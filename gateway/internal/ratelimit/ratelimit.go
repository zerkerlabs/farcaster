// Package ratelimit provides per-caller HTTP rate-limiting middleware for
// the Farcaster gateway (security invariant #8, AGENTS.md §3).
package ratelimit

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/zerkerlabs/farcaster/gateway/internal/auth"
)

// Default rate limit values applied when Config fields are zero or negative.
const (
	// DefaultRate is the steady-state token budget: 10 requests per second
	// per caller.
	DefaultRate = 10.0
	// DefaultBurst is the maximum token bucket depth: 20 tokens, allowing
	// short traffic spikes above the steady rate.
	DefaultBurst = 20
	// DefaultTTL is the idle duration after which a per-caller limiter is
	// evicted from the in-process map. Eviction bounds memory growth in
	// long-running multi-tenant deployments.
	DefaultTTL = 5 * time.Minute
)

// retryAfterCap is the maximum value (seconds) written to the Retry-After
// response header. Caps pathological cases without exposing internal state.
const retryAfterCap = 60

// Config holds per-caller rate limit parameters. Zero or negative values use
// the documented defaults, so a zero-value Config is ready to use.
type Config struct {
	// Rate is the steady-state request budget per caller, in requests per
	// second. Defaults to DefaultRate (10 req/s) when zero or negative.
	Rate float64

	// Burst is the maximum number of requests a caller may issue in a single
	// burst above the steady-state rate. Defaults to DefaultBurst (20) when
	// zero or negative.
	Burst int

	// TTL is the idle duration after which a per-caller limiter entry is
	// removed from the in-process map. Defaults to DefaultTTL (5 minutes)
	// when zero or negative.
	//
	// Single-instance note: limiters live in process memory. This
	// implementation does not coordinate across multiple gateway replicas;
	// distributed rate limiting (e.g., via Redis) is a follow-up concern
	// tracked separately. Entries for callers not seen within TTL are evicted
	// by a background goroutine; stop it with Middleware.Close.
	TTL time.Duration
}

func (c Config) effectiveRate() rate.Limit {
	if c.Rate <= 0 {
		return rate.Limit(DefaultRate)
	}
	return rate.Limit(c.Rate)
}

func (c Config) effectiveBurst() int {
	if c.Burst <= 0 {
		return DefaultBurst
	}
	return c.Burst
}

func (c Config) effectiveTTL() time.Duration {
	if c.TTL <= 0 {
		return DefaultTTL
	}
	return c.TTL
}

// entry pairs a rate limiter with the last time it was accessed.
type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Middleware is a per-caller HTTP rate-limiting middleware. Create it with New;
// call Close when the middleware is no longer needed to stop the background
// eviction goroutine.
type Middleware struct {
	lim   rate.Limit
	burst int
	ttl   time.Duration

	mu      sync.Mutex
	entries map[string]*entry

	now  func() time.Time
	done chan struct{}
}

// New returns a Middleware enforcing per-caller rate limits described by cfg.
// A background goroutine evicts idle entries on a ticker whose period equals
// cfg.TTL. Call Close to stop it.
func New(cfg Config) *Middleware {
	m := &Middleware{
		lim:     cfg.effectiveRate(),
		burst:   cfg.effectiveBurst(),
		ttl:     cfg.effectiveTTL(),
		entries: make(map[string]*entry),
		now:     time.Now,
		done:    make(chan struct{}),
	}
	go m.runCleanup()
	return m
}

// Handler returns the middleware function for use in an HTTP handler chain.
//
// The rate-limit key is "tenant\x00user" derived from the authenticated
// identity in the request context (populated by the auth middleware). Each
// unique caller gets its own independent token-bucket limiter.
//
// The paths /healthz and /version are exempt (invariants #1 and #8, AGENTS.md
// §3). Requests that exceed the limit receive 429 with a Retry-After header;
// no internal detail appears in the response body (invariant #3).
//
// If the request context carries no authenticated identity (both tenant and
// user are empty), the middleware rejects the request with 401. This defends
// against incorrect middleware chain ordering.
func (m *Middleware) Handler() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			if path == "/healthz" || path == "/version" {
				next.ServeHTTP(w, r)
				return
			}

			tenant := auth.TenantFromContext(r.Context())
			user := auth.UserFromContext(r.Context())
			if tenant == "" || user == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			key := tenant + "\x00" + user

			if delay := m.Allow(key); delay > 0 {
				retryAfter := int(math.Ceil(delay.Seconds()))
				if retryAfter > retryAfterCap {
					retryAfter = retryAfterCap
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Allow reports whether a request from the caller identified by key is within
// the rate limit. It returns 0 when the request is admitted (consuming a
// token), or the delay until the next token would be available (consuming
// nothing). Used by Handler for the global chain and by endpoint handlers that
// enforce their own tighter per-caller bound (e.g. /v1/analytics, spec 0003).
func (m *Middleware) Allow(key string) time.Duration {
	rsv := m.getLimiter(key).Reserve()
	if delay := rsv.Delay(); delay > 0 {
		rsv.Cancel()
		return delay
	}
	return 0
}

// Close stops the background eviction goroutine. Safe to call more than once.
func (m *Middleware) Close() {
	select {
	case <-m.done:
	default:
		close(m.done)
	}
}

func (m *Middleware) getLimiter(key string) *rate.Limiter {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		e = &entry{limiter: rate.NewLimiter(m.lim, m.burst)}
		m.entries[key] = e
	}
	e.lastSeen = m.now()
	return e.limiter
}

// cleanup removes entries that have been idle longer than m.ttl.
func (m *Middleware) cleanup() {
	cutoff := m.now().Add(-m.ttl)
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, e := range m.entries {
		if e.lastSeen.Before(cutoff) {
			delete(m.entries, key)
		}
	}
}

func (m *Middleware) runCleanup() {
	ticker := time.NewTicker(m.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.cleanup()
		case <-m.done:
			return
		}
	}
}
