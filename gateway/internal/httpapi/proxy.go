package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/auth"
	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
	"github.com/zerkerlabs/gateway/gateway/internal/policy"
	"github.com/zerkerlabs/gateway/gateway/internal/proxy"
	"github.com/zerkerlabs/gateway/gateway/internal/receipt"
	"github.com/zerkerlabs/gateway/x402types"
)

// ttftReader wraps an io.Reader and records the wall-clock time of the first
// successful (n > 0) Read call. Used on the streaming path to capture
// time-to-first-byte for the observability surface (spec 0003).
type ttftReader struct {
	src         io.Reader
	firstByteAt time.Time
}

func (r *ttftReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 && r.firstByteAt.IsZero() {
		r.firstByteAt = time.Now()
	}
	return n, err
}

// errToErrorClass maps a proxy or context error to the coarse error taxonomy
// used by the observability surface (spec 0003). Unmapped errors fall through
// to ErrorClassInternal.
func errToErrorClass(err error) invocation.ErrorClass {
	switch {
	case errors.Is(err, context.Canceled):
		return invocation.ErrorClassCancelled
	case errors.Is(err, context.DeadlineExceeded):
		// A deadline expiry (e.g. the invocation context's overall deadline
		// elapsing during retry backoff in proxy.Do) is a timeout, not an
		// internal error. Distinct from context.Canceled (caller/shutdown abort).
		return invocation.ErrorClassTimeout
	case errors.Is(err, proxy.ErrUpstreamTimeout):
		return invocation.ErrorClassTimeout
	case errors.Is(err, proxy.ErrSSRFBlocked):
		return invocation.ErrorClassSSRFBlocked
	case errors.Is(err, proxy.ErrCredentialError):
		return invocation.ErrorClassCredentialError
	case errors.Is(err, proxy.ErrUpstreamUnreachable), errors.Is(err, proxy.ErrCircuitOpen):
		return invocation.ErrorClassUpstream5xx
	default:
		return invocation.ErrorClassInternal
	}
}

// upstreamStatusErrorClass classifies an upstream HTTP status that the proxy
// returned as a *Result (the status was not retried or converted to a transport
// error). A 4xx or 5xx upstream response is a failure path for the observability
// surface (spec 0003): it must carry an error_class so analytics can tell it
// apart from a 2xx. Returns ("", false) for 2xx/3xx, which record as succeeded.
//
// Transient 5xx (502/503/504) never reach here — proxy.Do retries them and
// returns ErrUpstreamUnreachable, which errToErrorClass maps to upstream_5xx.
// Non-transient 5xx (e.g. 500/501/505) and all 4xx do reach here.
func upstreamStatusErrorClass(code int) (invocation.ErrorClass, bool) {
	switch {
	case code >= 400 && code < 500:
		return invocation.ErrorClassUpstream4xx, true
	case code >= 500:
		return invocation.ErrorClassUpstream5xx, true
	default:
		return "", false
	}
}

// proxyRetryAfterCap is the maximum value (seconds) written to the Retry-After
// response header on per-agent rate limit responses.
const proxyRetryAfterCap = 60

// ProxyForwarder is the subset of *proxy.Forwarder the proxy HTTP handlers
// require. *proxy.Forwarder satisfies this interface.
type ProxyForwarder interface {
	Do(ctx context.Context, tenantID, agentID, invocationID string, r *http.Request) (*proxy.Result, error)
	DoStream(ctx context.Context, tenantID, agentID, invocationID string, r *http.Request) (*proxy.Result, error)
}

// hopByHopResponseHeaders is the set of response headers that must not be
// forwarded from the upstream to the caller (RFC 7230 §6.1). These are
// transport-level headers meaningful only for a single hop.
var hopByHopResponseHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

type invocationResponse struct {
	InvocationID   string     `json:"invocation_id"`
	AgentID        string     `json:"agent_id"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at"`
	UpstreamStatus *int       `json:"upstream_status"`
	LatencyMS      *int64     `json:"latency_ms"`
	// MCPMethod / MCPTool are the parsed JSON-RPC method and (for tools/call)
	// tool name for MCP agents; null for non-MCP agents (spec 0004, Decision 7).
	MCPMethod *string `json:"mcp_method"`
	MCPTool   *string `json:"mcp_tool"`
	// Payment* fields are the x402 payment metadata captured on a priced call;
	// null for unpriced routes (spec 0005, Decision 7's additive pattern).
	PaymentNetwork *string `json:"payment_network"`
	PaymentAsset   *string `json:"payment_asset"`
	PaymentAmount  *string `json:"payment_amount"`
	PaymentPayer   *string `json:"payment_payer"`
	PaymentNonce   *string `json:"payment_nonce"`
	// ResponseBodyB64 is the captured upstream response body, base64-encoded.
	// A captured body can be arbitrary bytes (JSON, binary, an SSE stream), so
	// it cannot be inlined verbatim into JSON; the _b64 suffix documents the
	// encoding. Empty/omitted when body capture is off or no body was captured.
	ResponseBodyB64 string `json:"response_body_b64,omitempty"`
}

func toInvocationResponse(inv *invocation.Invocation) invocationResponse {
	resp := invocationResponse{
		InvocationID:   inv.ID,
		AgentID:        inv.AgentID,
		Status:         string(inv.Status),
		CreatedAt:      inv.CreatedAt,
		UpdatedAt:      inv.UpdatedAt,
		CompletedAt:    inv.CompletedAt,
		UpstreamStatus: inv.UpstreamStatus,
		LatencyMS:      inv.LatencyMS,
		MCPMethod:      inv.MCPMethod,
		MCPTool:        inv.MCPTool,
		PaymentNetwork: inv.PaymentNetwork,
		PaymentAsset:   inv.PaymentAsset,
		PaymentAmount:  inv.PaymentAmount,
		PaymentPayer:   inv.PaymentPayer,
		PaymentNonce:   inv.PaymentNonce,
	}
	if len(inv.ResponseBody) > 0 {
		resp.ResponseBodyB64 = base64.StdEncoding.EncodeToString(inv.ResponseBody)
	}
	return resp
}

// maxRequestBodyBytes caps the proxied request body the gateway will buffer.
// The body is read fully into memory (and re-read inside the forwarder), so an
// uncapped read is a memory-exhaustion DoS vector. 32 MiB is a safe v1 default;
// promote to config if a tenant needs larger payloads.
const maxRequestBodyBytes = 32 << 20 // 32 MiB

// maxModelNameBytes caps the X-Zerker-Model header the gateway will store on
// an invocation record. A model identifier is short (e.g. "claude-opus-4-8");
// without a cap, a caller could send up to Go's ~1 MiB header limit and bloat
// the invocations table. Reject oversize values at the boundary (AGENTS.md
// invariant #3 — validate all inbound payloads; spec 0003).
const maxModelNameBytes = 256

// handleTransact handles POST /v1/proxy/{id}. It creates a durable invocation
// record, returns 202 + inv_<id> immediately, then runs the upstream call
// server-side in a goroutine. The caller retrieves the result via handlePoll.
func (h *Handler) handleTransact(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	if tenant == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	agentID := r.PathValue("id")

	a, err := h.store.Get(r.Context(), tenant, agentID)
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.logger.Error("proxy transact: get agent", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Soft-deleted agents are preserved for audit but are not invocable.
	if a.Status == agent.StatusInactive {
		writeError(w, http.StatusUnprocessableEntity, "agent is inactive")
		return
	}
	if a.UpstreamURL == nil {
		writeError(w, http.StatusUnprocessableEntity, "missing upstream_url")
		return
	}
	// A suspended agent stays in the catalog but rejects invocations (spec 0002
	// Q11/Q12). Checked after inactive/pending so those take precedence.
	if a.Suspended {
		writeError(w, http.StatusUnprocessableEntity, "agent is suspended")
		return
	}
	// Per-agent rate-limit override: if the agent has a configured rate, check
	// its per-agent token bucket before forwarding (spec 0002 Q11/Q12).
	if h.agentLimiter != nil && a.InvocationRateLimit != nil {
		burst := 20 // default burst when only rate is configured
		if a.InvocationBurst != nil {
			burst = *a.InvocationBurst
		}
		if delay := h.agentLimiter.Allow(tenant, agentID, *a.InvocationRateLimit, burst); delay > 0 {
			retryAfter := int(math.Ceil(delay.Seconds()))
			if retryAfter > proxyRetryAfterCap {
				retryAfter = proxyRetryAfterCap
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
	}

	// Capture caller-supplied model name before buffering the body so the
	// invocation record carries it from the moment of creation (spec 0003).
	// Bound it at the boundary so an oversize header can't bloat the record.
	model := r.Header.Get("X-Zerker-Model")
	if len(model) > maxModelNameBytes {
		writeError(w, http.StatusBadRequest, "X-Zerker-Model too long")
		return
	}

	// Buffer the entire body before responding so the goroutine can replay it
	// after the HTTP handler (and thus r.Body) is no longer valid. Cap the read
	// so a caller can't exhaust gateway memory.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		h.logger.Error("proxy transact: read request body", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_ = r.Body.Close()
	bodySize := int64(len(body))

	// For MCP agents, parse the buffered JSON-RPC body to capture the method and
	// (for tools/call) the tool name (spec 0004, Decision 7). Best-effort: a
	// non-MCP body or a parse failure leaves both fields nil and never blocks
	// forwarding. The body is already fully buffered above, so the parse is
	// exact. The tool is also what per-tool x402 pricing keys on below.
	var mcpMethod, mcpTool *string
	if a.Protocol == "mcp" {
		mcpMethod, mcpTool = parseMCPRequest(body)
	}

	// Policy enforcement point (PEP, spec 0009 ticket T4): evaluated once the
	// MCP method/tool are known and before the x402 gate below, so a
	// policy-denied call is never told to pay. A tenant with no policy
	// configured (or no policy store wired at all) is unaffected.
	//
	// RatePerMin is the caller's observed request rate over the trailing
	// minute, from rateObserver (internal/ratelimit.ObservedRateTracker, spec
	// 0009 fast-follow #212). A nil rateObserver (not wired) leaves it at
	// zero, so a rate_per_min rule never matches — the pre-fast-follow gap.
	var ratePerMin int
	if h.rateObserver != nil {
		ratePerMin = h.rateObserver.Observe(tenant, agentID)
	}
	proceed, policyWarning := h.enforcePolicy(w, r, policy.RequestContext{
		TenantID:   tenant,
		Scopes:     auth.ScopesFromContext(r.Context()),
		AgentID:    agentID,
		Protocol:   a.Protocol,
		MCPMethod:  mcpMethod,
		MCPTool:    mcpTool,
		BodyBytes:  bodySize,
		RatePerMin: ratePerMin,
	})
	if !proceed {
		return
	}

	// x402 gate (spec 0005): a priced agent without X-PAYMENT gets a 402 challenge
	// instead of being forwarded — no invocation is created. When X-PAYMENT is
	// present, the payment authorization is verified (decode + parameter checks +
	// signature verification, T4a/T4b) before forwarding; any failure returns a
	// coarse 402 with no internal detail (invariant #3). The raw payload is never
	// logged (invariant #4). Unpriced agents (a.Pricing == nil) are unaffected.
	//
	// verifiedPayment/paymentReq carry the verified authorization into the
	// background goroutine so it can settle-then-forward on a settlement-enabled
	// route (spec 0006 Decisions 2, 4): settlement is a facilitator network call,
	// so — unlike the local verify here — it runs inside the async lifecycle
	// rather than adding synchronous latency to this 202 response.
	var paymentNetwork, paymentAsset, paymentAmount, paymentPayer, paymentNonce *string
	var verifiedPayment *x402types.Payment
	var paymentReq x402types.PaymentRequirements
	if a.Pricing != nil {
		if r.Header.Get("X-PAYMENT") == "" {
			writeJSON(w, http.StatusPaymentRequired, buildX402Challenge(a.Pricing, mcpTool, r.URL.Path))
			return
		}
		paymentReq = paymentRequirements(a.Pricing, mcpTool, r.URL.Path)
		pmt, err := h.verifyX402(r, paymentReq)
		if err != nil {
			writeError(w, http.StatusPaymentRequired, "payment verification failed")
			return
		}
		verifiedPayment = pmt
		paymentNetwork, paymentAsset, paymentAmount, paymentPayer, paymentNonce = paymentMetadata(pmt, a.Pricing.Asset)
	}

	inv := &invocation.Invocation{
		AgentID:        agentID,
		Mode:           invocation.ModeTransactional,
		Status:         invocation.StatusPending,
		RequestSize:    &bodySize,
		MCPMethod:      mcpMethod,
		MCPTool:        mcpTool,
		PaymentNetwork: paymentNetwork,
		PaymentAsset:   paymentAsset,
		PaymentAmount:  paymentAmount,
		PaymentPayer:   paymentPayer,
		PaymentNonce:   paymentNonce,
	}
	if model != "" {
		inv.Model = &model
	}
	if a.CaptureBody {
		inv.RequestBody = body
	}

	if err := h.invocations.Create(r.Context(), tenant, inv); err != nil {
		h.logger.Error("proxy transact: create invocation", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Clone the request for the goroutine, deriving context from the handler's
	// shutdown context so the upstream call is cancelled on graceful shutdown
	// (issue #53). The goroutine outlives the HTTP handler, so r.Context()
	// (the request context) must not be used here.
	cloned := r.Clone(h.shutdownCtx)
	cloned.Body = io.NopCloser(bytes.NewReader(body))

	// Register before spawning so Shutdown's WaitGroup.Wait() is guaranteed to
	// observe this goroutine even if it starts after Shutdown is called.
	h.invocWG.Add(1)
	go h.runTransact(tenant, agentID, inv.ID, cloned, a.CaptureBody, a.EmitReceipts, verifiedPayment, paymentReq) //nolint:gosec // G118: intentional — goroutine outlives the HTTP request; shutdownCtx replaces request context so Shutdown can cancel it

	w.Header().Set("X-Zerker-Invocation-ID", inv.ID)
	if policyWarning != "" {
		w.Header().Set(policyWarningHeader, policyWarning)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"invocation_id": inv.ID})
}

// runTransact executes the upstream call for a transactional invocation and
// updates the invocation record on completion. It runs in its own goroutine
// and signals completion via h.invocWG. pmt is the verified x402 payment for a
// priced route (nil for unpriced routes); when non-nil and a Settler is
// wired, it settles via the tenant's configured facilitator before forwarding
// (spec 0006 Decisions 2, 4) — never forwarding at all if settlement fails.
func (h *Handler) runTransact(tenantID, agentID, invocationID string, r *http.Request, captureBody, emitReceipts bool, pmt *x402types.Payment, paymentReq x402types.PaymentRequirements) {
	defer h.invocWG.Done()

	// ctx is the shutdown context: cancelled by Shutdown() to abort the upstream
	// call early. Store operations use context.Background() so they are never
	// interrupted — only the upstream HTTP call (and the facilitator settle
	// call, which is subject to the same shutdown-drain contract) is subject to
	// cancellation.
	ctx := r.Context()

	running := invocation.StatusRunning
	if _, err := h.invocations.Update(context.Background(), tenantID, invocationID, invocation.UpdateFields{
		Status: &running,
	}); err != nil {
		h.logger.Error("proxy transact: update to running",
			"invocation_id", invocationID, "err", err)
	}

	// Settle-then-forward (spec 0006 Decisions 2, 4, 8): on a priced route with a
	// Settler wired, settle via the facilitator before ever calling the
	// upstream. settleBeforeForward returns false only once it has already
	// recorded the invocation as terminal (settlement_failed) — the upstream
	// must not be called in that case ("served-but-unsettled" cannot occur).
	var settleResult *SettleResult
	if pmt != nil && h.settler != nil && h.settlementStore != nil && h.facilitatorCreds != nil {
		ok, result := h.settleBeforeForward(ctx, tenantID, invocationID, pmt, paymentReq)
		if !ok {
			return
		}
		settleResult = result
	}

	result, doErr := h.forwarder.Do(ctx, tenantID, agentID, invocationID, r)

	now := time.Now().UTC()
	fields := invocation.UpdateFields{CompletedAt: &now}

	if doErr != nil {
		failed := invocation.StatusFailed
		fields.Status = &failed
		ec := errToErrorClass(doErr)
		fields.ErrorClass = &ec
		h.logger.Warn("proxy transact: upstream error",
			"invocation_id", invocationID, "err", doErr)
	} else {
		// The upstream responded; record status/latency regardless of how the
		// body read goes.
		fields.UpstreamStatus = &result.StatusCode
		fields.LatencyMS = &result.LatencyMS

		respBody, readErr := io.ReadAll(result.Body)
		_ = result.Body.Close()
		if readErr != nil {
			// We got a response but couldn't read its body, so the record is
			// incomplete (no size, no captured body). Mark it failed rather than
			// reporting a misleading "succeeded".
			failed := invocation.StatusFailed
			fields.Status = &failed
			ec := invocation.ErrorClassInternal
			fields.ErrorClass = &ec
			h.logger.Error("proxy transact: read response body",
				"invocation_id", invocationID, "err", readErr)
		} else {
			// The upstream responded and we read its body — record size and
			// (if enabled) the captured body regardless of the status code.
			respSize := int64(len(respBody))
			fields.ResponseSize = &respSize
			if captureBody {
				captured := make([]byte, len(respBody))
				copy(captured, respBody)
				fields.ResponseBody = &captured
			}
			// A 4xx/non-transient-5xx upstream response is a failure path: mark
			// it failed + error_class so analytics can distinguish it from a 2xx
			// (spec 0003). Transient 5xx already arrived via the doErr branch.
			if ec, isErr := upstreamStatusErrorClass(result.StatusCode); isErr {
				failed := invocation.StatusFailed
				fields.Status = &failed
				fields.ErrorClass = &ec
			} else {
				succeeded := invocation.StatusSucceeded
				fields.Status = &succeeded
				// error_class stays nil: success leaves it NULL (spec 0003).
			}
		}
	}

	// Settlement succeeded but the upstream then failed terminally: the one
	// residual post-collection edge (spec 0006 Decision 5). No refund is
	// attempted; the settlement record's tx_hash/settled_amount are retained.
	applySettledUpstreamFailed(&fields, settleResult)

	// Terminal update always uses context.Background(): ctx may be cancelled
	// (shutdown signalled) by the time the upstream call finishes, but the
	// status write must succeed to leave the invocation in a terminal state.
	updated, err := h.invocations.Update(context.Background(), tenantID, invocationID, fields)
	if err != nil {
		h.logger.Error("proxy transact: update completion",
			"invocation_id", invocationID, "err", err)
		return
	}
	if emitReceipts {
		h.emitReceipt(updated)
	}
}

// receiptEmitTimeout bounds a single receipt emission so a slow Treeship can't
// pin a goroutine indefinitely.
const receiptEmitTimeout = 10 * time.Second

// emitReceipt sends a trust receipt for a completed invocation to the configured
// emitter. It is async and fail-open: a slow or failing Treeship never delays or
// fails the invocation (spec 0002 Q9), and it is a no-op when no emitter is
// attached. The receipt carries only invocation metadata — no bodies/secrets.
func (h *Handler) emitReceipt(inv *invocation.Invocation) {
	if h.emitter == nil {
		return
	}
	r := receipt.Receipt{
		InvocationID:   inv.ID,
		TenantID:       inv.TenantID,
		AgentID:        inv.AgentID,
		Mode:           string(inv.Mode),
		Status:         string(inv.Status),
		UpstreamStatus: inv.UpstreamStatus,
		LatencyMS:      inv.LatencyMS,
		RequestSize:    inv.RequestSize,
		ResponseSize:   inv.ResponseSize,
	}
	if inv.CompletedAt != nil {
		r.CompletedAt = *inv.CompletedAt
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), receiptEmitTimeout)
		defer cancel()
		if err := h.emitter.Emit(ctx, r); err != nil {
			h.logger.Warn("receipt emission failed (fail-open)",
				"invocation_id", r.InvocationID, "err", err)
		}
	}()
}

// handleStream handles POST /v1/proxy/{id}/stream. It creates a durable
// invocation record, sets X-Zerker-Invocation-ID immediately (even on
// error), then opens a long-lived connection to the upstream and streams the
// request and response verbatim. The handler blocks until the upstream closes
// its response or the caller disconnects — no goroutine is spawned.
func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	if tenant == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	agentID := r.PathValue("id")

	a, err := h.store.Get(r.Context(), tenant, agentID)
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.logger.Error("proxy stream: get agent", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if a.Status == agent.StatusInactive {
		writeError(w, http.StatusUnprocessableEntity, "agent is inactive")
		return
	}
	if a.UpstreamURL == nil {
		writeError(w, http.StatusUnprocessableEntity, "missing upstream_url")
		return
	}
	if a.Suspended {
		writeError(w, http.StatusUnprocessableEntity, "agent is suspended")
		return
	}
	if h.agentLimiter != nil && a.InvocationRateLimit != nil {
		burst := 20
		if a.InvocationBurst != nil {
			burst = *a.InvocationBurst
		}
		if delay := h.agentLimiter.Allow(tenant, agentID, *a.InvocationRateLimit, burst); delay > 0 {
			retryAfter := int(math.Ceil(delay.Seconds()))
			if retryAfter > proxyRetryAfterCap {
				retryAfter = proxyRetryAfterCap
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
	}

	// Capture caller-supplied model name before the upstream call so the
	// invocation record carries it from the moment of creation (spec 0003).
	// Bound it at the boundary so an oversize header can't bloat the record.
	model := r.Header.Get("X-Zerker-Model")
	if len(model) > maxModelNameBytes {
		writeError(w, http.StatusBadRequest, "X-Zerker-Model too long")
		return
	}

	// Emit a Treeship receipt once the invocation reaches a terminal state, if
	// the agent opted in. finalInv is set at each terminal Update below; the
	// emit is async + fail-open (spec 0002 Q9).
	var finalInv *invocation.Invocation
	defer func() {
		if finalInv != nil && a.EmitReceipts {
			h.emitReceipt(finalInv)
		}
	}()

	// For MCP agents, capture the JSON-RPC method/tool without breaking the
	// zero-copy stream (spec 0004, Decision 7). The streaming path forwards the
	// body live, so we peek only a bounded prefix, reconstruct r.Body from that
	// prefix plus the untouched remainder, and parse the prefix. Forwarding sees
	// the identical byte stream regardless of the parse outcome; a read error
	// leaves the fields nil and is surfaced by DoStream below exactly as today.
	// The tool is also what per-tool x402 pricing keys on below.
	var mcpMethod, mcpTool *string
	if a.Protocol == "mcp" {
		orig := r.Body
		peeked, readErr := io.ReadAll(io.LimitReader(orig, maxMCPParseBytes))
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(peeked), orig))
		if readErr == nil {
			mcpMethod, mcpTool = parseMCPRequest(peeked)
		}
	}

	// Policy enforcement point (PEP, spec 0009 ticket T4): same slot as
	// handleTransact — after the MCP method/tool are known, before the x402
	// gate below. A tenant with no policy configured (or no policy store wired
	// at all) is unaffected. BodyBytes uses r.ContentLength rather than
	// buffering: the streaming path intentionally never buffers the request
	// body (see the comment on the forward call below). ContentLength is -1
	// for a chunked/unknown-length body; treat that as "arbitrarily large"
	// (math.MaxInt64) so a max_body_bytes rule fails toward matching rather
	// than being silently bypassable by omitting Content-Length.
	//
	// RatePerMin is the caller's observed request rate over the trailing
	// minute, from rateObserver — same sourcing as handleTransact (spec 0009
	// fast-follow #212).
	bodyBytes := r.ContentLength
	if bodyBytes < 0 {
		bodyBytes = math.MaxInt64
	}
	var ratePerMin int
	if h.rateObserver != nil {
		ratePerMin = h.rateObserver.Observe(tenant, agentID)
	}
	proceed, policyWarning := h.enforcePolicy(w, r, policy.RequestContext{
		TenantID:   tenant,
		Scopes:     auth.ScopesFromContext(r.Context()),
		AgentID:    agentID,
		Protocol:   a.Protocol,
		MCPMethod:  mcpMethod,
		MCPTool:    mcpTool,
		BodyBytes:  bodyBytes,
		RatePerMin: ratePerMin,
	})
	if !proceed {
		return
	}

	// x402 gate (spec 0005): a priced agent without X-PAYMENT gets a 402 challenge
	// instead of being forwarded — no invocation is created. When X-PAYMENT is
	// present, the payment authorization is verified (decode + parameter checks +
	// signature verification, T4a/T4b) before forwarding; any failure returns a
	// coarse 402 with no internal detail (invariant #3). The raw payload is never
	// logged (invariant #4). This gate stays BEFORE the invocation is created and
	// the body is streamed. Unpriced agents (a.Pricing == nil) are unaffected.
	//
	// verifiedPayment/paymentReq carry the verified authorization forward so
	// settle-then-forward (below, spec 0006 Decisions 2, 4) can settle via the
	// tenant's facilitator before the upstream stream is opened.
	var paymentNetwork, paymentAsset, paymentAmount, paymentPayer, paymentNonce *string
	var verifiedPayment *x402types.Payment
	var paymentReq x402types.PaymentRequirements
	if a.Pricing != nil {
		if r.Header.Get("X-PAYMENT") == "" {
			writeJSON(w, http.StatusPaymentRequired, buildX402Challenge(a.Pricing, mcpTool, r.URL.Path))
			return
		}
		paymentReq = paymentRequirements(a.Pricing, mcpTool, r.URL.Path)
		pmt, err := h.verifyX402(r, paymentReq)
		if err != nil {
			writeError(w, http.StatusPaymentRequired, "payment verification failed")
			return
		}
		verifiedPayment = pmt
		paymentNetwork, paymentAsset, paymentAmount, paymentPayer, paymentNonce = paymentMetadata(pmt, a.Pricing.Asset)
	}

	inv := &invocation.Invocation{
		AgentID:        agentID,
		Mode:           invocation.ModeStreaming,
		Status:         invocation.StatusPending,
		MCPMethod:      mcpMethod,
		MCPTool:        mcpTool,
		PaymentNetwork: paymentNetwork,
		PaymentAsset:   paymentAsset,
		PaymentAmount:  paymentAmount,
		PaymentPayer:   paymentPayer,
		PaymentNonce:   paymentNonce,
	}
	if model != "" {
		inv.Model = &model
	}
	if err := h.invocations.Create(r.Context(), tenant, inv); err != nil {
		h.logger.Error("proxy stream: create invocation", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Set the invocation ID and no-buffering headers before any upstream call.
	// X-Zerker-Invocation-ID must appear in the response regardless of the
	// outcome (spec 0002: "every invocation is addressable … regardless of mode").
	w.Header().Set("X-Zerker-Invocation-ID", inv.ID)
	w.Header().Set("X-Accel-Buffering", "no")

	running := invocation.StatusRunning
	if _, err := h.invocations.Update(r.Context(), tenant, inv.ID, invocation.UpdateFields{
		Status: &running,
	}); err != nil {
		h.logger.Error("proxy stream: update to running",
			"invocation_id", inv.ID, "err", err)
	}

	// Settle-then-forward (spec 0006 Decisions 2, 3, 4, 8): on a priced route
	// with a Settler wired, settle via the tenant's facilitator before the
	// upstream stream is ever opened — settlement completes before the first
	// byte, mirroring runTransact (T4). settleBeforeForward returns false only
	// once it has already recorded the invocation as terminal
	// (settlement_failed); the stream must never open in that case
	// ("served-but-unsettled" cannot occur). A successful settle's receipt
	// rides X-PAYMENT-RESPONSE on the eventual response — the one synchronous
	// point on this path a header receipt fits (Decision 3); the invocation
	// settlement sub-record remains the canonical receipt.
	var paymentResponseHeader string
	var settleResult *SettleResult
	if verifiedPayment != nil && h.settler != nil && h.settlementStore != nil && h.facilitatorCreds != nil {
		ok, result := h.settleBeforeForward(r.Context(), tenant, inv.ID, verifiedPayment, paymentReq)
		if !ok {
			writeError(w, http.StatusPaymentRequired, "settlement failed")
			return
		}
		settleResult = result
		if settleResult != nil {
			hdr, err := buildPaymentResponseHeader(settleResult)
			if err != nil {
				h.logger.Error("proxy stream: build X-PAYMENT-RESPONSE", "invocation_id", inv.ID, "err", err)
			} else {
				paymentResponseHeader = hdr
			}
		}
	}

	// Unlike handleTransact, the streaming endpoint intentionally does NOT cap
	// the request body with http.MaxBytesReader: it forwards r.Body to the
	// upstream as a live stream without buffering (spec 0002 Q6 — streaming
	// "suits hefty payloads"; the zero-copy design means no per-request memory
	// blow-up from a large body). Egress/quota limits, not a buffer cap, are the
	// control for abusive stream sizes.
	//
	// proxyStart is recorded here so ttft_ms can be measured from the moment
	// the upstream call begins to the first response byte received (spec 0003).
	proxyStart := time.Now()
	result, doErr := h.forwarder.DoStream(r.Context(), tenant, agentID, inv.ID, r)

	now := time.Now().UTC()
	fields := invocation.UpdateFields{CompletedAt: &now}

	if doErr != nil {
		failed := invocation.StatusFailed
		fields.Status = &failed
		ec := errToErrorClass(doErr)
		fields.ErrorClass = &ec
		h.logger.Warn("proxy stream: upstream error",
			"invocation_id", inv.ID, "err", doErr)

		// Settlement succeeded but the upstream then failed terminally: the one
		// residual post-collection edge (spec 0006 Decision 5). No refund is
		// attempted; the settlement record's tx_hash/settled_amount are retained.
		applySettledUpstreamFailed(&fields, settleResult)

		switch {
		case errors.Is(doErr, context.Canceled):
			// Caller disconnected; don't attempt to write — there is no one
			// receiving. Use background context since r.Context() is cancelled.
			finalInv, err = h.invocations.Update(context.Background(), tenant, inv.ID, fields)
			if err != nil {
				h.logger.Error("proxy stream: update on disconnect",
					"invocation_id", inv.ID, "err", err)
			}
			return
		case errors.Is(doErr, proxy.ErrUpstreamTimeout):
			writeError(w, http.StatusGatewayTimeout, "upstream timeout")
		default:
			writeError(w, http.StatusBadGateway, "upstream unavailable")
		}

		finalInv, err = h.invocations.Update(r.Context(), tenant, inv.ID, fields)
		if err != nil {
			h.logger.Error("proxy stream: update failed",
				"invocation_id", inv.ID, "err", err)
		}
		return
	}

	// Copy upstream response headers (excluding hop-by-hop headers) before
	// committing the status code. Re-assert our invocation-ID header afterwards
	// in case the upstream response included the same header name.
	for k, vs := range result.Header {
		if hopByHopResponseHeaders[k] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Zerker-Invocation-ID", inv.ID)
	if paymentResponseHeader != "" {
		w.Header().Set("X-PAYMENT-RESPONSE", paymentResponseHeader)
	}
	if policyWarning != "" {
		w.Header().Set(policyWarningHeader, policyWarning)
	}

	w.WriteHeader(result.StatusCode)
	fields.UpstreamStatus = &result.StatusCode
	fields.LatencyMS = &result.LatencyMS

	// Wrap the response body to record the wall-clock time of the first byte
	// received. This gives ttft_ms from proxyStart to first data byte (spec 0003).
	tr := &ttftReader{src: result.Body}

	// Stream the upstream response body to the caller, flushing after every
	// write so bytes flow immediately (first-byte semantics for SSE/chunked).
	var written int64
	var copyErr error
	if flusher, ok := w.(http.Flusher); ok {
		written, copyErr = copyFlushing(w, tr, flusher)
	} else {
		written, copyErr = io.Copy(w, tr)
	}
	_ = result.Body.Close()

	respSize := written
	fields.ResponseSize = &respSize

	// Record ttft_ms if at least one byte was received (spec 0003: NULL when no
	// bytes were streamed).
	if !tr.firstByteAt.IsZero() {
		ttft := tr.firstByteAt.Sub(proxyStart).Milliseconds()
		fields.TTFTMS = &ttft
	}

	// Always use background context for the terminal update: r.Context() may
	// be cancelled (client disconnected) by the time streaming finishes.
	updateCtx := context.Background()
	if copyErr != nil {
		failed := invocation.StatusFailed
		fields.Status = &failed
		ec := invocation.ErrorClassInternal
		fields.ErrorClass = &ec
		h.logger.Warn("proxy stream: copy response body",
			"invocation_id", inv.ID, "err", copyErr)
	} else if ec, isErr := upstreamStatusErrorClass(result.StatusCode); isErr {
		// The body streamed cleanly, but the upstream status was a 4xx/5xx —
		// a failure path. The status line was already sent to the caller; the
		// record still reflects the failure + error_class for analytics (spec
		// 0003). Transient 5xx already arrived via the doErr branch above.
		failed := invocation.StatusFailed
		fields.Status = &failed
		fields.ErrorClass = &ec
	} else {
		succeeded := invocation.StatusSucceeded
		fields.Status = &succeeded
		// error_class stays nil: success leaves it NULL (spec 0003).
	}

	// Settlement succeeded but the upstream then failed terminally: the one
	// residual post-collection edge (spec 0006 Decision 5). No refund is
	// attempted; the settlement record's tx_hash/settled_amount are retained.
	applySettledUpstreamFailed(&fields, settleResult)

	finalInv, err = h.invocations.Update(updateCtx, tenant, inv.ID, fields)
	if err != nil {
		h.logger.Error("proxy stream: update completion",
			"invocation_id", inv.ID, "err", err)
	}
}

// copyFlushing copies from src to dst, calling flusher.Flush after each write
// so that streaming data is delivered to the caller without proxy buffering.
func copyFlushing(dst io.Writer, src io.Reader, flusher http.Flusher) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			total += int64(written)
			flusher.Flush()
			if writeErr != nil {
				return total, writeErr
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// handlePoll handles GET /v1/proxy/{id}/invocations/{inv_id}. It returns the
// current status and result of a transactional invocation.
func (h *Handler) handlePoll(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	if tenant == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	agentID := r.PathValue("id")
	invID := r.PathValue("inv_id")

	inv, err := h.invocations.Get(r.Context(), tenant, invID)
	if err != nil {
		if errors.Is(err, invocation.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.logger.Error("proxy poll: get invocation", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Confirm the invocation belongs to the agent in the path. Returning 404
	// for a mismatch avoids confirming invocation existence across agents.
	if inv.AgentID != agentID {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	writeJSON(w, http.StatusOK, toInvocationResponse(inv))
}
