package account

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// contextKey is an unexported type for the request-context key so it cannot
// collide with keys set by other packages.
type contextKey int

const keyAccount contextKey = iota

// FromContext returns the facilitator account the mTLS middleware resolved
// for this request. ok is false if the middleware has not run (it always has
// for any request that reaches a handler wrapped by Middleware).
func FromContext(ctx context.Context) (*Account, bool) {
	a, ok := ctx.Value(keyAccount).(*Account)
	return a, ok
}

// Middleware authenticates the caller from the already-verified TLS client
// certificate (the handshake itself requires and verifies the certificate
// against the configured client CA — see the mtls package; this middleware
// never re-checks the chain) and maps its identity to a facilitator account.
//
// No peer certificate on the connection, an unknown certificate fingerprint,
// or a fingerprint mapping to an inactive account all yield an identical
// 403 "unknown facilitator account" response — the caller cannot distinguish
// "no such account" from "deactivated account". No chain is touched on any of
// these paths. A resolved, active account is placed in the request context
// (retrieve with FromContext) and next is called.
func Middleware(store Store, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A request reaching this handler over a listener configured with
			// tls.RequireAndVerifyClientCert always carries a verified peer
			// certificate. This nil/empty check is defensive fail-closed
			// behaviour (e.g. a misconfigured listener), not the primary
			// enforcement point — that is the TLS handshake.
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				logger.Warn("account: request has no TLS client certificate", "path", r.URL.Path)
				writeUnknownAccount(w)
				return
			}

			fp := Fingerprint(r.TLS.PeerCertificates[0])
			a, err := store.Lookup(r.Context(), fp)
			if err != nil || !a.Active {
				logger.Warn("account: certificate does not map to an active account", "path", r.URL.Path)
				writeUnknownAccount(w)
				return
			}

			ctx := context.WithValue(r.Context(), keyAccount, a)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeUnknownAccount(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown facilitator account"})
}
