package httpapi

import (
	"strings"
	"sync"
	"time"
)

// nonceTTL bounds how long a seen (payer, nonce) pair is remembered by the
// best-effort replay guard (spec 0005: "bounded TTL is fine for v1"). It only
// needs to outlive the longest authorization window Farcaster advertises
// (x402MaxTimeoutSeconds); the margin here covers clock skew between the
// caller's signed validBefore and the gateway's clock.
const nonceTTL = 10 * time.Minute

// nonceStore is a best-effort, single-process record of (payer, nonce) pairs
// already presented on a priced call. It is NOT authoritative single-use
// enforcement on its own: it only catches replay against the SAME running
// gateway process within nonceTTL, so a signed X-PAYMENT could in principle be
// replayed against another gateway instance, or against this same process
// after nonceTTL has elapsed.
//
// On a gate-only route (no facilitator configured) this is the only replay
// defense there is (spec 0005 Behavior: "Replay is a known gate-only
// limitation"). On a settlement-enabled route it is demoted to a cheap
// pre-filter that rejects obvious replays before ever calling the facilitator
// — the on-chain nonce consumed by a successful settle is what makes
// single-use authoritative (spec 0006 Decision 8).
type nonceStore struct {
	mu   sync.Mutex
	seen map[string]time.Time // "payer\x00nonce" (lowercased) -> expiry
}

func newNonceStore() *nonceStore {
	return &nonceStore{seen: make(map[string]time.Time)}
}

// reserve records (payer, nonce) as seen as of now and reports whether it was
// already present and unexpired — true means the authorization is a replay
// and the caller must reject it. A fresh pair (or one whose prior entry has
// aged out past nonceTTL) is recorded and reserve returns false. Hex fields
// are compared case-insensitively (EIP-55 checksums only vary casing).
//
// Expired entries are swept on every call rather than via a background
// goroutine: the map only grows with priced-call volume within nonceTTL, a
// bound appropriate for the "best-effort" v1 guard.
func (s *nonceStore) reserve(payer, nonce string, now time.Time) bool {
	key := strings.ToLower(payer) + "\x00" + strings.ToLower(nonce)

	s.mu.Lock()
	defer s.mu.Unlock()

	for k, expiry := range s.seen {
		if now.After(expiry) {
			delete(s.seen, k)
		}
	}

	if expiry, ok := s.seen[key]; ok && now.Before(expiry) {
		return true
	}
	s.seen[key] = now.Add(nonceTTL)
	return false
}
