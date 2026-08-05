// Package httpapi implements the Zerker HTTP API surface (spec 0001 and
// beyond). Each endpoint lives in its own file; RegisterRoutes is the single
// place to add new route registrations so future handlers can be added with
// minimal merge conflict.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
	"github.com/zerkerlabs/gateway/gateway/internal/policy"
	"github.com/zerkerlabs/gateway/gateway/internal/receipt"
	"github.com/zerkerlabs/gateway/gateway/internal/settlement"
)

// AgentRateLimiter checks per-agent invocation rate limits.
// *ratelimit.AgentLimiter satisfies this interface.
type AgentRateLimiter interface {
	// Allow returns 0 if the request is within the limit, or the delay until
	// the next token is available. r is the rate in req/s; burst is the burst depth.
	Allow(tenantID, agentID string, r float64, burst int) time.Duration
}

// RateObserver records each proxied request and reports the caller's
// observed request rate over a trailing window, feeding the policy
// enforcement point's rate_per_min matching (spec 0009 fast-follow, #212).
// *ratelimit.ObservedRateTracker satisfies this.
type RateObserver interface {
	// Observe records a request for (tenantID, agentID) and returns the
	// number of requests observed for that caller within the trailing
	// window, including this one.
	Observe(tenantID, agentID string) int
}

// CallerRateLimiter enforces a per-caller rate limit for a single endpoint.
// *ratelimit.Middleware satisfies this interface. Used by GET /v1/analytics for
// a tighter bound than the global per-caller limiter (spec 0003).
type CallerRateLimiter interface {
	// Allow returns 0 if a request from the caller identified by key is within
	// the limit, or the delay until the next token is available.
	Allow(key string) time.Duration
}

// Handler holds the shared dependencies for all Zerker HTTP handlers.
type Handler struct {
	store           agent.AgentStore
	credSvc         CredentialService
	forwarder       ProxyForwarder
	invocations     invocation.Store
	settlementStore settlement.Store
	policyStore     policy.PolicyStore
	// evaluator resolves a tenant's Policy + the in-flight call's RequestContext
	// to a Decision (spec 0009, ticket T3). Defaulted to policy.NewBuiltinEvaluator
	// in NewHandler; never nil, so the PEP (enforcePolicy) can call it
	// unconditionally once h.policyStore is non-nil.
	evaluator policy.Evaluator
	// decisionRecorder captures each policy Decision made inline (spec 0009,
	// ticket T5's capture seam). nil disables capture — the default until a
	// persisted implementation is wired via WithPolicyDecisions (or a fake via
	// WithPolicyDecisionRecorder).
	decisionRecorder policy.DecisionRecorder
	// decisionStore is the tenant-scoped read side of policy decision capture
	// (spec 0009, ticket T5). Non-nil enables GET /v1/policy/decisions. Attached
	// by WithPolicyDecisions, which also derives decisionRecorder from it.
	decisionStore policy.DecisionStore
	// classifierClient calls a matched rule's opt-in classifier webhook (spec
	// 0009 Decision 4, ticket T6). Defaulted to NewHTTPClassifierClient(nil)
	// in NewHandler; only ever invoked when a matched rule sets Classifier, so
	// a tenant with no such rule makes zero external calls regardless of this
	// default. Never nil; tests override via WithClassifierClient.
	classifierClient policy.ClassifierClient
	agentLimiter     AgentRateLimiter
	// rateObserver supplies policy.RequestContext.RatePerMin at the PEP (spec
	// 0009 fast-follow, #212). nil leaves RatePerMin at zero, matching the
	// pre-fast-follow behavior (a rate_per_min rule never matches).
	rateObserver     RateObserver
	analyticsLimiter CallerRateLimiter // nil → no tighter limit on /v1/analytics
	emitter          receipt.Emitter   // nil → receipts disabled (no-op)
	// paymentVerifier verifies the signature on an x402 payment authorization for
	// priced routes (spec 0005). Defaulted to the real Base/USDC EIP-712 verifier
	// in NewHandler (baseUSDCVerifier); tests inject a permissive or fail-closed
	// one via WithPaymentVerifier. Never nil.
	paymentVerifier PaymentVerifier
	// replayGuard is the best-effort in-memory (payer, nonce) replay tracker for
	// verified x402 payments (spec 0005 T6). On a settlement-enabled route it is
	// demoted to a cheap pre-filter ahead of the facilitator call; on-chain
	// settlement is what makes single-use authoritative (spec 0006 Decision 8).
	// Never nil; not overridable — it has no external dependency to fake, unlike
	// paymentVerifier.
	replayGuard *nonceStore
	// settler settles a verified payment via the tenant's configured facilitator
	// (spec 0006 Decisions 1, 2). nil disables settle-then-forward orchestration
	// entirely — priced routes stay gate-only (surface-5 behavior) even if a
	// tenant has configured settlement, which matches "no settler wired" being a
	// deployment choice, not a per-tenant one.
	settler Settler
	// facilitatorCreds resolves a tenant's facilitator_credential_ref to its
	// AuthType and decrypted plaintext for the settle call. Required alongside
	// settler for settle-then-forward to run; see WithSettler.
	facilitatorCreds FacilitatorCredentialResolver
	logger           *slog.Logger

	// invocWG tracks in-flight transactional invocation goroutines so Shutdown
	// can drain them before the process exits (issue #53).
	invocWG sync.WaitGroup
	// shutdownCtx is cancelled by Shutdown to signal goroutines to abort their
	// upstream calls; shutdownCancel is idempotent and safe to call multiple times.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

// NewHandler returns a Handler backed by store, logging to logger.
func NewHandler(store agent.AgentStore, logger *slog.Logger) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Handler{
		store:            store,
		logger:           logger,
		paymentVerifier:  baseUSDCVerifier{},
		evaluator:        policy.NewBuiltinEvaluator(),
		classifierClient: NewHTTPClassifierClient(nil),
		replayGuard:      newNonceStore(),
		shutdownCtx:      ctx,
		shutdownCancel:   cancel,
	}
}

// Shutdown signals all in-flight transactional invocation goroutines to abort
// their upstream calls and waits for them to record a terminal invocation
// status. ctx bounds the wait; if the deadline expires Shutdown returns
// ctx.Err(). Cancelled goroutines mark their invocations failed rather than
// leaving them in the running state (issue #53).
func (h *Handler) Shutdown(ctx context.Context) error {
	h.shutdownCancel()
	done := make(chan struct{})
	go func() {
		h.invocWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WithCredentials attaches the credential service used by the /v1/credentials
// routes. Credential routes are registered by RegisterRoutes only when a
// service has been attached. Returns h to allow method chaining.
func (h *Handler) WithCredentials(svc CredentialService) *Handler {
	h.credSvc = svc
	return h
}

// WithProxy attaches the forwarder and invocation store used by the
// /v1/proxy routes. Proxy routes are registered by RegisterRoutes only when
// both are non-nil. Returns h to allow method chaining.
func (h *Handler) WithProxy(f ProxyForwarder, invStore invocation.Store) *Handler {
	h.forwarder = f
	h.invocations = invStore
	return h
}

// WithSettlement attaches the settlement config store used by the
// /v1/settlement/config routes (spec 0006, ticket T1). Those routes are
// registered by RegisterRoutes only when both this store and a credential
// service (via WithCredentials) have been attached, since PATCH validates
// facilitator_credential_ref against the credential service. Returns h to
// allow method chaining.
func (h *Handler) WithSettlement(store settlement.Store) *Handler {
	h.settlementStore = store
	return h
}

// WithPolicy attaches the tenant-scoped policy store used by the
// GET/PUT /v1/policy authoring routes (spec 0009, ticket T2). Those routes are
// registered by RegisterRoutes only when a store has been attached. Returns h
// to allow method chaining.
func (h *Handler) WithPolicy(store policy.PolicyStore) *Handler {
	h.policyStore = store
	return h
}

// WithPolicyEvaluator overrides the policy evaluation engine used by the
// inline PEP (spec 0009, ticket T4). The default (set by NewHandler) is
// policy.NewBuiltinEvaluator; tests inject a different Evaluator to exercise
// the engine-error branch of the on_error posture (Decision 3). Returns h to
// allow method chaining.
func (h *Handler) WithPolicyEvaluator(e policy.Evaluator) *Handler {
	h.evaluator = e
	return h
}

// WithPolicyDecisionRecorder attaches the capture sink each inline policy
// Decision is (async, fail-open) handed to (spec 0009, ticket T5's seam). A
// nil recorder (the default) disables capture. WithPolicyDecisions is the
// production wiring; this override lets tests inject a fake recorder to observe
// what the enforcement point emits. Call it after WithPolicyDecisions to take
// effect. Returns h to allow method chaining.
func (h *Handler) WithPolicyDecisionRecorder(r policy.DecisionRecorder) *Handler {
	h.decisionRecorder = r
	return h
}

// WithPolicyDecisions attaches the tenant-scoped policy decision store (spec
// 0009, ticket T5). It wires both sides of capture from one store: the read
// endpoint GET /v1/policy/decisions (registered by RegisterRoutes only when the
// store is attached), and the write side — the enforcement point's async,
// fail-open DecisionRecorder, derived here as a StoreRecorder so a capture-write
// failure is logged, never surfaced on the proxied call. Returns h to allow
// method chaining.
func (h *Handler) WithPolicyDecisions(store policy.DecisionStore) *Handler {
	h.decisionStore = store
	h.decisionRecorder = policy.NewStoreRecorder(store, h.logger)
	return h
}

// WithClassifierClient overrides the classifier webhook client used by the
// inline PEP's optional semantic rail (spec 0009 Decision 4, ticket T6). The
// default (set by NewHandler) is NewHTTPClassifierClient(nil); tests inject a
// client pointed at an httptest.Server, or one that errors/times out, to
// exercise the best-effort fallback to a policy's on_error. Returns h to
// allow method chaining.
func (h *Handler) WithClassifierClient(c policy.ClassifierClient) *Handler {
	h.classifierClient = c
	return h
}

// WithSettler attaches the Settler and facilitator credential resolver used by
// the transactional settle-then-forward orchestration (spec 0006 T4). Both
// must be non-nil, and WithSettlement must also be attached, for settlement to
// run on a priced route; when any is missing, priced calls stay gate-only
// (verify-then-forward, surface-5 behavior). Returns h to allow method chaining.
func (h *Handler) WithSettler(s Settler, creds FacilitatorCredentialResolver) *Handler {
	h.settler = s
	h.facilitatorCreds = creds
	return h
}

// WithReceipts attaches the Treeship receipt emitter. When set, a receipt is
// emitted (async, fail-open) after each proxied invocation completes for agents
// that have EmitReceipts enabled. A nil emitter (the default) disables receipts.
// Returns h to allow method chaining.
func (h *Handler) WithReceipts(e receipt.Emitter) *Handler {
	h.emitter = e
	return h
}

// WithPaymentVerifier overrides the x402 payment signature verifier used on
// priced routes (spec 0005). The default is the real Base/USDC EIP-712 verifier
// (baseUSDCVerifier); tests inject a permissive verifier to exercise the forward
// path, or a rejecting one to exercise the fail-closed path. Returns h to allow
// method chaining.
func (h *Handler) WithPaymentVerifier(v PaymentVerifier) *Handler {
	h.paymentVerifier = v
	return h
}

// WithAgentLimiter attaches a per-agent invocation rate limiter. When non-nil
// and an agent has InvocationRateLimit set, proxy handlers enforce the per-agent
// bucket before forwarding (spec 0002 Q11/Q12). Returns h for method chaining.
func (h *Handler) WithAgentLimiter(lim AgentRateLimiter) *Handler {
	h.agentLimiter = lim
	return h
}

// WithRateObserver attaches the observed-rate tracker used to populate
// policy.RequestContext.RatePerMin at the PEP (spec 0009 fast-follow, #212).
// When nil (the default), RatePerMin stays at zero and a rate_per_min policy
// rule never matches. Returns h for method chaining.
func (h *Handler) WithRateObserver(o RateObserver) *Handler {
	h.rateObserver = o
	return h
}

// WithAnalyticsLimiter attaches a tighter per-caller rate limiter applied only
// to GET /v1/analytics, whose percentile aggregation is heavier than a row
// fetch (spec 0003). When nil, the endpoint relies solely on the global
// per-caller limiter. Returns h for method chaining.
func (h *Handler) WithAnalyticsLimiter(lim CallerRateLimiter) *Handler {
	h.analyticsLimiter = lim
	return h
}

// RegisterRoutes mounts all routes onto mux. Agent Catalog routes are always
// registered; credential routes are registered only when a credential service
// has been attached via WithCredentials.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/agents", h.handleCreate)
	mux.HandleFunc("GET /v1/agents", h.handleList)
	mux.HandleFunc("GET /v1/agents/{id}", h.handleGet)
	mux.HandleFunc("PATCH /v1/agents/{id}", h.handleUpdate)
	mux.HandleFunc("DELETE /v1/agents/{id}", h.handleDelete)

	if h.credSvc != nil {
		mux.HandleFunc("POST /v1/credentials", h.handleCreateCredential)
		mux.HandleFunc("GET /v1/credentials", h.handleListCredentials)
		mux.HandleFunc("GET /v1/credentials/{id}", h.handleGetCredential)
		mux.HandleFunc("PUT /v1/credentials/{id}", h.handlePutCredential)
		mux.HandleFunc("DELETE /v1/credentials/{id}", h.handleDeleteCredential)
	}

	if h.invocations != nil {
		mux.HandleFunc("GET /v1/invocations", h.handleListInvocations)
		mux.HandleFunc("GET /v1/invocations/{id}", h.handleGetInvocation)
		mux.HandleFunc("GET /v1/analytics", h.handleAnalytics)
	}

	if h.settlementStore != nil && h.credSvc != nil {
		mux.HandleFunc("PATCH /v1/settlement/config", h.handlePatchSettlementConfig)
		mux.HandleFunc("GET /v1/settlement/config", h.handleGetSettlementConfig)
	}

	if h.policyStore != nil {
		mux.HandleFunc("GET /v1/policy", h.handleGetPolicy)
		mux.HandleFunc("PUT /v1/policy", h.handlePutPolicy)
	}

	if h.decisionStore != nil {
		mux.HandleFunc("GET /v1/policy/decisions", h.handleListPolicyDecisions)
	}

	if h.forwarder != nil && h.invocations != nil {
		mux.HandleFunc("POST /v1/proxy/{id}", h.handleTransact)
		mux.HandleFunc("POST /v1/proxy/{id}/stream", h.handleStream)
		mux.HandleFunc("GET /v1/proxy/{id}/invocations/{inv_id}", h.handlePoll)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
