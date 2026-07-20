// Package health implements the facilitator's operational endpoints (spec
// 0007 T7): GET /supported (capability advertisement) and GET /healthz
// (liveness + readiness). Neither requires auth (AGENTS.md invariant #1
// exempts /healthz; /supported is a discovery endpoint that reveals no
// account or tenant data). Readiness also gates /settle: a request arriving
// while the facilitator isn't ready gets 503 before it reaches settlement
// logic, per spec 0007's protocol-failure taxonomy (RPC down / gas tank
// empty ⇒ 4xx/5xx, not a silent revert).
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/zerkerlabs/farcaster/facilitator/internal/chain"
)

// Kind is one settle-able (scheme, network, asset) combination this
// facilitator advertises via GET /supported.
type Kind struct {
	Scheme  string `json:"scheme"`
	Network string `json:"network"`
	Asset   string `json:"asset"`
}

// Checker reports whether the facilitator is ready to settle: RPC reachable
// and the gas wallet holds at least its configured low-water mark. Backed by
// *chain.ReadinessChecker in production; tests use a stub.
type Checker interface {
	Ready(ctx context.Context) error
}

// Supported returns the GET /supported handler advertising kinds — the set
// of (scheme, network, asset) triples this facilitator will settle, so a
// caller can discover capability without a failed settle.
func Supported(kinds []Kind) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, struct {
			Kinds []Kind `json:"kinds"`
		}{Kinds: kinds})
	})
}

// Healthz returns the GET /healthz handler: liveness always succeeds (the
// process answered), and readiness additionally runs checker.Ready,
// reporting 503 when it fails. Per AGENTS.md invariant #9, the body carries a
// coarse status/reason only — never an RPC URL, address, or other config
// value.
func Healthz(checker Checker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := checker.Ready(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unavailable",
				"reason": reasonSlug(err),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

// GateSettle wraps the /settle handler so a request is rejected with 503
// before it runs when the facilitator isn't ready. A drained gas tank or
// unreachable RPC must fail closed rather than let /settle attempt (and
// silently revert or hang on) a submission it cannot pay for or broadcast.
func GateSettle(checker Checker, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := checker.Ready(r.Context()); err != nil {
			reason := reasonSlug(err)
			logger.Warn("settle: facilitator not ready", "reason", reason)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "facilitator not ready: " + reason})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// reasonSlug collapses a Checker error to a coarse, safe-to-return slug —
// never the wrapped error text, which could otherwise grow to embed RPC
// detail as the underlying checks evolve.
func reasonSlug(err error) string {
	switch {
	case errors.Is(err, chain.ErrRPCUnreachable):
		return "rpc_unreachable"
	case errors.Is(err, chain.ErrGasBalanceLow):
		return "gas_balance_low"
	default:
		return "not_ready"
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
