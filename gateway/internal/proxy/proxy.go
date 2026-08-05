// Package proxy implements the upstream forwarding engine for the Zerker
// routing/proxy surface (spec 0002, implementation item #4). It is shared by
// both the transactional and streaming proxy endpoints.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/credential"
	"github.com/zerkerlabs/farcaster/gateway/internal/resource"
)

// Public sentinel errors returned by Forwarder.Do. Endpoint handlers map these
// to HTTP status codes; the proxy core never performs that mapping itself.
var (
	// ErrMissingUpstreamURL is returned when the agent has no upstream_url set.
	// This is a caller-induced error (spec 0002 §Q1).
	ErrMissingUpstreamURL = errors.New("missing upstream_url")

	// ErrCircuitOpen is returned when the per-upstream circuit breaker is open.
	// Maps to 502 at the handler layer.
	ErrCircuitOpen = errors.New("circuit breaker open")

	// ErrUpstreamTimeout is returned when the upstream HTTP round-trip times out.
	// Maps to 504 at the handler layer.
	ErrUpstreamTimeout = errors.New("upstream timeout")

	// ErrUpstreamUnreachable is returned on network-level failures or when all
	// retries of a transient upstream status (502/503/504) are exhausted.
	// Maps to 502 at the handler layer.
	ErrUpstreamUnreachable = errors.New("upstream unavailable")

	// ErrSSRFBlocked is returned when the upstream address is rejected by the
	// SSRF-safe dialer. Distinct from ErrUpstreamUnreachable so callers can
	// classify the error as ssrf_blocked in the observability taxonomy (spec 0003).
	ErrSSRFBlocked = errors.New("upstream address blocked by SSRF protection")

	// ErrCredentialError is returned when the upstream credential cannot be
	// resolved or decrypted. The underlying cause is intentionally not exposed
	// to callers — credentials must never appear in error messages (invariant #4,
	// AGENTS.md). Maps to credential_error in the observability taxonomy (spec 0003).
	ErrCredentialError = errors.New("upstream credential error") //nolint:gosec // taxonomy sentinel, not a credential value
)

// errTransientStatus is an unexported sentinel wrapping transient HTTP status
// codes (502, 503, 504) returned by the upstream. It is used internally for
// retry detection and is always converted to ErrUpstreamUnreachable before
// being returned from Do.
var errTransientStatus = errors.New("transient upstream status")

// errBlocked is an unexported sentinel for an SSRF-blocked dial (safeDialContext
// rejected the address). The block is deterministic — re-resolving and re-dialing
// would just block again — so it is NOT retriable and must NOT count as an
// upstream-health failure (it would otherwise inflate the circuit breaker). Like
// errTransientStatus it is converted to ErrUpstreamUnreachable before return.
var errBlocked = errors.New("upstream address in blocked range")

// CredentialService is the subset of *credential.Service the forwarder needs.
// *credential.Service satisfies this interface.
type CredentialService interface {
	Get(ctx context.Context, tenantID, id string) (*credential.Credential, error)
	Decrypt(ctx context.Context, tenantID, id string) ([]byte, error)
}

// Config holds tunable knobs for a Forwarder. Zero or negative values use the
// package defaults, so a zero-value Config is safe to pass.
type Config struct {
	// MaxRetries is the number of additional attempts after the first failure
	// on retriable errors. 0 or negative → DefaultMaxRetries.
	MaxRetries int

	// RetryBaseDelay is the base duration for exponential back-off between
	// retry attempts. 0 or negative → DefaultRetryBaseDelay.
	RetryBaseDelay time.Duration

	// UpstreamTimeout is the per-attempt deadline for the upstream round-trip.
	// 0 or negative → DefaultUpstreamTimeout.
	UpstreamTimeout time.Duration

	// StreamFirstByteTimeout is the deadline for the upstream to start sending
	// the response on a streaming invocation (i.e. until response headers
	// arrive). After headers are received the stream runs until the upstream
	// closes it or the context is cancelled. 0 or negative → UpstreamTimeout.
	StreamFirstByteTimeout time.Duration

	// BreakerThreshold is the number of consecutive Do failures required to
	// open the circuit for an upstream URL. 0 or negative → DefaultBreakerThreshold.
	BreakerThreshold int

	// BreakerCooldown is how long the circuit stays open before a single probe
	// is allowed through. 0 or negative → DefaultBreakerCooldown.
	BreakerCooldown time.Duration

	// HTTPClient overrides the upstream http.Client. Intended for tests.
	// If nil, a client with UpstreamTimeout as its Timeout is constructed.
	// The same override is used for the streaming client.
	HTTPClient *http.Client
}

// Package-level defaults. All are tunable at construction time via Config.
const (
	DefaultMaxRetries       = 2
	DefaultRetryBaseDelay   = 100 * time.Millisecond
	DefaultUpstreamTimeout  = 30 * time.Second
	DefaultBreakerThreshold = 5
	DefaultBreakerCooldown  = 30 * time.Second
)

func (c *Config) applyDefaults() {
	if c.MaxRetries <= 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.RetryBaseDelay <= 0 {
		c.RetryBaseDelay = DefaultRetryBaseDelay
	}
	if c.UpstreamTimeout <= 0 {
		c.UpstreamTimeout = DefaultUpstreamTimeout
	}
	if c.StreamFirstByteTimeout <= 0 {
		c.StreamFirstByteTimeout = c.UpstreamTimeout
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = DefaultBreakerThreshold
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = DefaultBreakerCooldown
	}
}

// Result holds the upstream response from a successful Do call. The caller is
// responsible for closing Body.
type Result struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
	LatencyMS  int64
}

// Forwarder is the shared upstream forwarding engine for the routing proxy
// surface. Construct it with New; use Do to forward a transactional invocation
// and DoStream to forward a streaming invocation.
type Forwarder struct {
	agents       agent.AgentStore
	credSvc      CredentialService
	client       *http.Client
	streamClient *http.Client
	logger       *slog.Logger

	maxRetries     int
	retryBaseDelay time.Duration
	breakers       *breakerRegistry
}

// New constructs a Forwarder backed by the given stores and configuration.
// If logger is nil, slog.Default() is used.
func New(agents agent.AgentStore, credSvc CredentialService, cfg Config, logger *slog.Logger) *Forwarder {
	cfg.applyDefaults()

	client := cfg.HTTPClient
	if client == nil {
		// Clone the default transport so we inherit its TLS/connection-pool
		// settings, then attach the SSRF-safe dialer (issue #51).
		t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // http.DefaultTransport is always *http.Transport
		t.DialContext = safeDialContext
		client = &http.Client{Transport: t, Timeout: cfg.UpstreamTimeout}
	}

	// The streaming client uses ResponseHeaderTimeout (first-byte timeout) with
	// no global Timeout so long-lived streams are not killed mid-flight. When
	// HTTPClient is set (test mode), use it for both to avoid test-server
	// compatibility issues.
	streamClient := cfg.HTTPClient
	if streamClient == nil {
		t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // http.DefaultTransport is always *http.Transport
		t.DialContext = safeDialContext
		t.ResponseHeaderTimeout = cfg.StreamFirstByteTimeout
		streamClient = &http.Client{Transport: t}
	}

	if logger == nil {
		logger = slog.Default()
	}
	return &Forwarder{
		agents:         agents,
		credSvc:        credSvc,
		client:         client,
		streamClient:   streamClient,
		logger:         logger,
		maxRetries:     cfg.MaxRetries,
		retryBaseDelay: cfg.RetryBaseDelay,
		breakers:       newBreakerRegistry(cfg.BreakerThreshold, cfg.BreakerCooldown),
	}
}

// Do resolves the agent's upstream URL and credential, applies the header
// policy, and forwards r to the upstream. It retries retriable errors up to
// MaxRetries times and enforces the per-upstream circuit breaker.
//
// invocationID is an inv_<uuid> forwarded as X-Zerker-Invocation-ID; pass
// an empty string if the record has not yet been created.
//
// Do buffers r.Body so retries can replay it — r.Body is fully consumed on
// return. On success the caller owns and must close Result.Body.
func (f *Forwarder) Do(ctx context.Context, tenantID, agentID, invocationID string, r *http.Request) (*Result, error) {
	a, err := f.agents.Get(ctx, tenantID, agentID)
	if err != nil {
		return nil, err
	}
	if a.UpstreamURL == nil {
		return nil, ErrMissingUpstreamURL
	}
	upstreamURL := *a.UpstreamURL

	// Resolve upstream credential metadata.
	credAuthType := credential.AuthTypeNone
	var credRef string
	if a.CredentialRef != nil {
		credRef = *a.CredentialRef
		cred, getErr := f.credSvc.Get(ctx, tenantID, credRef)
		if getErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrCredentialError, getErr)
		}
		credAuthType = cred.AuthType
	}

	// Decrypt plaintext only if a credential with a non-None auth type is set.
	// The plaintext is zeroed on return to limit the window of key material in
	// memory (invariant #4, AGENTS.md).
	var credPlaintext []byte
	if credRef != "" && credAuthType != credential.AuthTypeNone {
		credPlaintext, err = f.credSvc.Decrypt(ctx, tenantID, credRef)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCredentialError, err)
		}
		defer zeroBytes(credPlaintext)
	}

	// Buffer the body once so each retry can replay it from the start.
	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		_ = r.Body.Close()
	}

	// Honour the caller's X-Request-ID; inject one if absent.
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID, err = resource.New("req")
		if err != nil {
			return nil, fmt.Errorf("generate request id: %w", err)
		}
	}

	// Gate on the circuit breaker before any attempt.
	br := f.breakers.get(upstreamURL)
	if !br.allow() {
		return nil, ErrCircuitOpen
	}

	// Retry safety: for a Protocol=mcp agent, only known-idempotent JSON-RPC
	// methods may be retried after a transient upstream error — tools/call (or
	// any unrecognised method) must never be automatically retried, since a
	// retry could double-execute a side-effecting tool call (spec 0004,
	// Decision 6). Protocol=http agents are unaffected: retryAllowed stays true.
	retryAllowed := true
	if a.Protocol == "mcp" {
		retryAllowed = mcpRetryAllowed(body)
	}

	var result *Result
	var lastErr error
	for attempt := 0; attempt <= f.maxRetries; attempt++ {
		if attempt > 0 {
			f.logger.Warn(
				"proxy: retrying transient upstream error",
				"attempt", attempt,
				"max_retries", f.maxRetries,
				"agent_id", agentID,
				"invocation_id", invocationID,
				"err", lastErr,
			)
			backoff := f.retryBaseDelay * (1 << (attempt - 1))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		result, lastErr = f.doOnce(ctx, upstreamURL, r, body, requestID, credAuthType, credPlaintext, tenantID, agentID, invocationID)
		if lastErr == nil {
			br.success()
			return result, nil
		}
		if !isRetriable(lastErr) || !retryAllowed {
			break
		}
	}

	// Count upstream failures toward the circuit breaker. Context cancellations
	// and build errors are not upstream failures and must not open the circuit.
	if isUpstreamFailure(lastErr) {
		br.failure()
	}
	// Convert internal sentinels to the public errors so callers see only the
	// exported sentinels. SSRF blocks are distinct from generic unreachability
	// so the observability layer can classify them separately (spec 0003).
	if errors.Is(lastErr, errBlocked) {
		return nil, ErrSSRFBlocked
	}
	if errors.Is(lastErr, errTransientStatus) {
		return nil, ErrUpstreamUnreachable
	}
	return nil, lastErr
}

// DoStream resolves the agent's upstream URL and credential, then opens a
// streaming connection to the upstream. Unlike Do, DoStream:
//   - does not retry on transient upstream errors (retrying is not safe for a
//     live streaming connection);
//   - uses a transport-level first-byte timeout (ResponseHeaderTimeout) rather
//     than a full round-trip Timeout, so long-lived streams are not killed.
//
// On success the caller owns Result.Body and must close it when done. The
// context should be the HTTP request context so the upstream connection is
// cancelled when the caller disconnects.
func (f *Forwarder) DoStream(ctx context.Context, tenantID, agentID, invocationID string, r *http.Request) (*Result, error) {
	a, err := f.agents.Get(ctx, tenantID, agentID)
	if err != nil {
		return nil, err
	}
	if a.UpstreamURL == nil {
		return nil, ErrMissingUpstreamURL
	}
	upstreamURL := *a.UpstreamURL

	credAuthType := credential.AuthTypeNone
	var credRef string
	if a.CredentialRef != nil {
		credRef = *a.CredentialRef
		cred, getErr := f.credSvc.Get(ctx, tenantID, credRef)
		if getErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrCredentialError, getErr)
		}
		credAuthType = cred.AuthType
	}

	var credPlaintext []byte
	if credRef != "" && credAuthType != credential.AuthTypeNone {
		credPlaintext, err = f.credSvc.Decrypt(ctx, tenantID, credRef)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCredentialError, err)
		}
		defer zeroBytes(credPlaintext)
	}

	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID, err = resource.New("req")
		if err != nil {
			return nil, fmt.Errorf("generate request id: %w", err)
		}
	}

	br := f.breakers.get(upstreamURL)
	if !br.allow() {
		return nil, ErrCircuitOpen
	}

	// Build the upstream request. Passing nil body here so that buildUpstreamRequest
	// sets up headers without buffering; we attach the live body stream below.
	req, err := buildUpstreamRequest(ctx, upstreamURL, r, nil, requestID, credAuthType, credPlaintext, tenantID, agentID, invocationID)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	// Forward the caller's request body as a live stream (no buffering; no retries).
	req.Body = r.Body
	req.ContentLength = r.ContentLength

	start := time.Now()
	resp, err := f.streamClient.Do(req) //nolint:gosec,bodyclose // upstream URL validated at CRUD boundary; DNS-rebinding re-checked via safeDialContext on the transport (issue #51); body returned to caller via Result.Body
	latency := time.Since(start).Milliseconds()
	if err != nil {
		cerr := classifyNetworkError(err)
		// An SSRF block is our deterministic rejection, not an upstream-health
		// signal: don't trip the breaker. Return ErrSSRFBlocked so the
		// observability layer can classify it separately (spec 0003).
		if errors.Is(cerr, errBlocked) {
			return nil, ErrSSRFBlocked
		}
		br.failure()
		return nil, cerr
	}

	// Count a transient upstream status (502/503/504) toward the shared circuit
	// breaker, exactly as the transactional path does. Calling br.success()
	// unconditionally here would reset the failure counter on every 5xx and keep
	// the breaker from ever opening, even while concurrent Do() calls accumulate
	// failures against the same upstream. The response body is still streamed
	// back to the caller — streaming mode never retries or buffers.
	if isTransientStatus(resp.StatusCode) {
		br.failure()
	} else {
		br.success()
	}
	return &Result{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
		LatencyMS:  latency,
	}, nil
}

// doOnce performs a single upstream HTTP round-trip and returns either a Result
// or a classified error (ErrUpstreamUnreachable, ErrUpstreamTimeout, or an
// errTransientStatus wrap). It never retries.
func (f *Forwarder) doOnce(ctx context.Context, upstreamURL string, orig *http.Request, body []byte, requestID string, credAuthType credential.AuthType, credPlaintext []byte, tenantID, agentID, invocationID string) (*Result, error) {
	req, err := buildUpstreamRequest(ctx, upstreamURL, orig, body, requestID, credAuthType, credPlaintext, tenantID, agentID, invocationID)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	start := time.Now()
	resp, err := f.client.Do(req) //nolint:gosec // upstream URL validated at CRUD boundary (httpapi.validateUpstreamURL) for IP literals; DNS-rebinding re-checked at dial time via safeDialContext on the transport (issue #51)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return nil, classifyNetworkError(err)
	}

	if isTransientStatus(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w %d", errTransientStatus, resp.StatusCode)
	}

	return &Result{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
		LatencyMS:  latency,
	}, nil
}

// buildUpstreamRequest constructs the outbound request: copies allowed caller
// headers, sets X-Zerker-* metadata, ensures X-Request-ID, and injects the
// upstream credential.
func buildUpstreamRequest(ctx context.Context, upstreamURL string, orig *http.Request, body []byte, requestID string, credAuthType credential.AuthType, credPlaintext []byte, tenantID, agentID, invocationID string) (*http.Request, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, orig.Method, upstreamURL, bodyReader)
	if err != nil {
		return nil, err
	}

	copyHeaders(orig.Header, req.Header)
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("X-Zerker-Agent-ID", agentID)
	req.Header.Set("X-Zerker-Tenant-ID", tenantID)
	if invocationID != "" {
		req.Header.Set("X-Zerker-Invocation-ID", invocationID)
	}
	injectCredential(req.Header, credAuthType, credPlaintext)

	return req, nil
}

// isTransientStatus reports whether an upstream HTTP status code should trigger
// a retry. 502, 503, and 504 indicate temporary upstream unavailability.
func isTransientStatus(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

// classifyNetworkError maps a Go network/transport error to an appropriate
// proxy sentinel. Context cancellations are propagated as-is so the caller can
// detect them separately from upstream failures.
func classifyNetworkError(err error) error {
	// An SSRF-blocked dial is our own deterministic rejection, not an upstream
	// health signal: preserve the marker so the retry/breaker logic can skip it.
	if errors.Is(err, errBlocked) {
		return errBlocked
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrUpstreamTimeout
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrUpstreamTimeout
	}
	return ErrUpstreamUnreachable
}

// isRetriable reports whether the error warrants another upstream attempt.
// Timeouts and context errors are not retried; only network errors and transient
// HTTP status codes qualify.
func isRetriable(err error) bool {
	return errors.Is(err, ErrUpstreamUnreachable) || errors.Is(err, errTransientStatus)
}

// isUpstreamFailure reports whether the error should be counted toward the
// circuit breaker's failure threshold. Context cancellations and internal build
// errors are not upstream failures.
func isUpstreamFailure(err error) bool {
	return errors.Is(err, ErrUpstreamUnreachable) ||
		errors.Is(err, ErrUpstreamTimeout) ||
		errors.Is(err, errTransientStatus)
}

// zeroBytes overwrites b with zeros to limit the window during which plaintext
// key material resides in memory (invariant #4, AGENTS.md).
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
