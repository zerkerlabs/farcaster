// Package server wires the Farcaster gateway's HTTP surface.
//
// Operational endpoints (/healthz, /version) are always registered. Agent
// Catalog routes (/v1/agents) are registered when cfg.Store is non-nil, and
// wrapped with cfg.AuthMiddleware when that is also non-nil.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/httpapi"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
	"github.com/zerkerlabs/farcaster/gateway/internal/receipt"
	"github.com/zerkerlabs/farcaster/gateway/internal/settlement"
	"github.com/zerkerlabs/farcaster/gateway/internal/version"
)

// Config holds the optional runtime dependencies for the HTTP server.
// A zero-value Config is valid: it produces a server with operational routes
// only (suitable for tests and local dev without OIDC).
type Config struct {
	// APIHandler, when non-nil, is used directly as the httpapi.Handler for
	// all /v1/ routes. When set the component fields (Store, CredentialService,
	// Forwarder, InvocationStore, ReceiptEmitter, AgentLimiter) are ignored —
	// the caller is responsible for wiring them onto APIHandler before passing
	// it in. AuthMiddleware, RateLimit, and Logger are still applied.
	//
	// Use this when you need a reference to the Handler to call Shutdown() on
	// graceful stop (issue #53). If nil, New builds a Handler from the
	// component fields below (backward-compatible behaviour).
	APIHandler *httpapi.Handler

	// Store backs the /v1/agents routes. If nil (and APIHandler is also nil),
	// those routes are not registered.
	Store agent.AgentStore

	// CredentialService backs the /v1/credentials routes. If nil, those routes
	// are not registered. Requires Store to be non-nil (credentials reference
	// agents via the FK on agents.credential_ref). Typed as an interface so the
	// server depends on the behaviour, not credential's concrete type;
	// *credential.Service satisfies it.
	CredentialService httpapi.CredentialService

	// Forwarder backs the POST /v1/proxy/{id} (transactional) and
	// GET /v1/proxy/{id}/invocations/{inv_id} (polling) routes. Both Forwarder
	// and InvocationStore must be non-nil for the proxy routes to be registered.
	Forwarder httpapi.ProxyForwarder

	// InvocationStore backs the proxy routes. See Forwarder for registration
	// conditions. *invocation.MemoryStore and *invocation.PostgresStore both
	// satisfy invocation.Store.
	InvocationStore invocation.Store

	// ReceiptEmitter, when non-nil, emits a Treeship trust receipt after each
	// proxied invocation completes for agents with EmitReceipts enabled. nil
	// disables receipts (the default until a real Treeship emitter is wired).
	ReceiptEmitter receipt.Emitter

	// SettlementStore backs the PATCH/GET /v1/settlement/config routes (spec
	// 0006, ticket T1). If nil, those routes are not registered. Requires
	// CredentialService to be non-nil (PATCH validates facilitator_credential_ref
	// against it).
	SettlementStore settlement.Store

	// Settler and FacilitatorCredentials back the transactional settle-then-
	// forward orchestration (spec 0006 T4): verify -> settle -> forward on a
	// settlement-enabled priced route. Both must be non-nil, alongside
	// SettlementStore, for settlement to run; otherwise priced routes stay
	// gate-only (surface-5 behavior). *credential.Service satisfies
	// FacilitatorCredentials the same way it satisfies CredentialService.
	Settler                httpapi.Settler
	FacilitatorCredentials httpapi.FacilitatorCredentialResolver

	// AuthMiddleware wraps the entire mux with bearer-token validation.
	// The middleware already exempts /healthz and /version internally.
	// Required for production; if nil, agent routes are served without auth
	// (acceptable only for local development where OIDC is not configured).
	AuthMiddleware func(http.Handler) http.Handler

	// RateLimit wraps the mux with per-caller rate limiting. It runs inside
	// AuthMiddleware so the authenticated identity is populated when the limiter
	// keys on it. If nil, no rate limiting is applied.
	RateLimit func(http.Handler) http.Handler

	// AgentLimiter enforces per-agent invocation rate-limit overrides at the
	// proxy layer (spec 0002 Q11/Q12). If nil, per-agent rate limiting is
	// skipped but the global per-caller limit still applies.
	AgentLimiter httpapi.AgentRateLimiter

	// AnalyticsLimiter enforces a tighter per-caller limit on GET /v1/analytics
	// (spec 0003). If nil, the endpoint relies on the global per-caller limiter.
	AnalyticsLimiter httpapi.CallerRateLimiter

	// RateObserver supplies policy.RequestContext.RatePerMin at the proxy
	// layer's policy enforcement point (spec 0009 fast-follow, #212). If nil,
	// RatePerMin stays at zero and a rate_per_min policy rule never matches.
	RateObserver httpapi.RateObserver

	// Logger is used by route handlers. Defaults to slog.Default().
	Logger *slog.Logger
}

// New returns the gateway's HTTP handler with all configured routes registered.
func New(cfg Config) http.Handler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /version", handleVersion)

	switch {
	case cfg.APIHandler != nil:
		// Caller pre-built the handler (e.g., to hold a reference for Shutdown).
		cfg.APIHandler.RegisterRoutes(mux)
	case cfg.Store != nil:
		h := httpapi.NewHandler(cfg.Store, logger)
		if cfg.CredentialService != nil {
			h.WithCredentials(cfg.CredentialService)
		}
		if cfg.AgentLimiter != nil {
			h.WithAgentLimiter(cfg.AgentLimiter)
		}
		if cfg.AnalyticsLimiter != nil {
			h.WithAnalyticsLimiter(cfg.AnalyticsLimiter)
		}
		if cfg.RateObserver != nil {
			h.WithRateObserver(cfg.RateObserver)
		}
		if cfg.Forwarder != nil && cfg.InvocationStore != nil {
			h.WithProxy(cfg.Forwarder, cfg.InvocationStore)
		}
		if cfg.ReceiptEmitter != nil {
			h.WithReceipts(cfg.ReceiptEmitter)
		}
		if cfg.SettlementStore != nil {
			h.WithSettlement(cfg.SettlementStore)
		}
		if cfg.Settler != nil && cfg.FacilitatorCredentials != nil {
			h.WithSettler(cfg.Settler, cfg.FacilitatorCredentials)
		}
		h.RegisterRoutes(mux)
	}

	var handler http.Handler = mux
	// Rate limiting runs inside auth so the caller identity is already set.
	if cfg.RateLimit != nil {
		handler = cfg.RateLimit(handler)
	}
	if cfg.AuthMiddleware != nil {
		handler = cfg.AuthMiddleware(handler)
	}
	return handler
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": version.Version,
		"commit":  version.Commit,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encoding a map[string]string never fails; ignore the error deliberately.
	_ = json.NewEncoder(w).Encode(body)
}
