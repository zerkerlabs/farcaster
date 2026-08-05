package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/credential"
	"github.com/zerkerlabs/gateway/gateway/internal/settlement"
	"github.com/zerkerlabs/gateway/gateway/internal/ssrf"
	"github.com/zerkerlabs/gateway/x402types"
)

// Sentinel error categories a *SettleError wraps. They let a caller (the
// settle-then-forward orchestration, spec 0006 T4) distinguish a transient
// failure worth a bounded retry from a terminal one, without parsing Reason
// strings (spec 0006 Decision 4: "transient failures retry a bounded number
// of times, then terminal").
var (
	// ErrSettleUnreachable covers a transport-level failure reaching the
	// facilitator: SSRF-blocked, DNS failure, connection refused, timeout.
	ErrSettleUnreachable = errors.New("x402: facilitator unreachable")
	// ErrSettleBadResponse covers a non-2xx status or a response body that does
	// not parse as the expected settle response shape.
	ErrSettleBadResponse = errors.New("x402: facilitator returned an unexpected response")
	// ErrSettleRejected covers a well-formed facilitator response reporting the
	// settle as unsuccessful (e.g. payer balance insufficient at settle time,
	// nonce already consumed).
	ErrSettleRejected = errors.New("x402: facilitator declined to settle")
)

// SettleError describes why a facilitator settle attempt failed to collect
// payment. Reason is deliberately coarse (spec 0006: "a settlement failure is
// never a 5xx leaking internal detail") — it is either a fixed category string
// or the facilitator's own structured errorReason, never raw response bytes,
// the payment payload, or the credential.
type SettleError struct {
	// Reason is the coarse, human-readable failure reason safe to persist on
	// the invocation's settlement sub-record (spec 0006 "reason" field).
	Reason string
	cause  error // one of the Err* sentinels above; wrapped so errors.Is works
}

func (e *SettleError) Error() string {
	return fmt.Sprintf("x402: settle failed: %s", e.Reason)
}

// Unwrap exposes the sentinel category so callers can use errors.Is(err,
// ErrSettleUnreachable) etc. to decide retry eligibility.
func (e *SettleError) Unwrap() error { return e.cause }

// SettleResult is what a successful facilitator settle call returns: the
// on-chain reference and reported parties, mapped onto the invocation's
// settlement sub-record by the orchestration layer (spec 0006 T4).
type SettleResult struct {
	// TxHash is the on-chain transaction reference the facilitator submitted.
	TxHash string
	// Network is the network the facilitator settled on (echoed back so it can
	// be cross-checked against the requirement).
	Network string
	// Payer is the address the facilitator observed as the paying party.
	Payer string
	// FacilitatorFee is the facilitator-reported fee, if any (spec 0006
	// Decision 6: the gateway never sets or takes one; it only represents what
	// the facilitator reports). Empty when the facilitator does not report one
	// (the OSS/self-host path).
	FacilitatorFee string
}

// The POST /settle request/response wire types are x402types.SettleRequest and
// x402types.SettleResponse (shared, generated from the OpenAPI contract —
// ADR-0010). facilitatorFee is an optional extension a fee-recovering
// facilitator (e.g. a Zerker-hosted one) may report (spec 0006 Decision 6),
// absent on a self-hosted facilitator.

// defaultSettleTimeout bounds a single facilitator settle round-trip. The
// caller (T4) applies its own bounded-retry policy on top of this.
const defaultSettleTimeout = 15 * time.Second

// maxSettleResponseBytes caps how much of a facilitator's response body is
// read, so a misbehaving or malicious facilitator cannot exhaust memory.
const maxSettleResponseBytes = 1 << 20 // 1 MiB

// facilitatorSettler is the production Settler: it POSTs the verified
// authorization to the tenant-configured facilitator's `/settle` and parses
// the response. It makes no direct chain RPC (spec 0006 Decision 8) — the
// facilitator does all chain interaction.
type facilitatorSettler struct {
	client *http.Client
}

// NewFacilitatorSettler returns a Settler that calls real facilitators over
// HTTPS through the SSRF guard (internal/ssrf), exactly as the proxy's
// upstream calls do (spec 0006: "the outbound call goes through the existing
// guard exactly as upstream_url does"). If client is nil, a client with the
// SSRF-safe dialer and defaultSettleTimeout is constructed; tests may inject
// an override (e.g. a stubbed facilitator's own client) the same way
// proxy.Config.HTTPClient does.
func NewFacilitatorSettler(client *http.Client) Settler {
	if client == nil {
		t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // http.DefaultTransport is always *http.Transport
		t.DialContext = ssrf.SafeDialContext
		client = &http.Client{Transport: t, Timeout: defaultSettleTimeout}
	}
	return &facilitatorSettler{client: client}
}

// Settle implements Settler.
func (s *facilitatorSettler) Settle(ctx context.Context, facilitatorURL string, authType credential.AuthType, credPlaintext []byte, pmt *x402types.Payment, req x402types.PaymentRequirements) (*SettleResult, error) {
	body, err := json.Marshal(x402types.SettleRequest{
		X402Version:         x402types.Version,
		PaymentPayload:      *pmt,
		PaymentRequirements: req,
	})
	if err != nil {
		return nil, &SettleError{Reason: "internal error building settle request", cause: ErrSettleUnreachable}
	}

	settleURL := strings.TrimRight(facilitatorURL, "/") + "/settle"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, settleURL, bytes.NewReader(body))
	if err != nil {
		return nil, &SettleError{Reason: "internal error building settle request", cause: ErrSettleUnreachable}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setFacilitatorAuth(httpReq.Header, authType, credPlaintext)

	resp, err := s.client.Do(httpReq) //nolint:gosec // facilitator_url validated at CRUD boundary (validateFacilitatorURL); DNS-rebinding re-checked at dial time via ssrf.SafeDialContext
	if err != nil {
		return nil, &SettleError{Reason: "facilitator unreachable", cause: ErrSettleUnreachable}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSettleResponseBytes))
	if err != nil {
		return nil, &SettleError{Reason: "facilitator returned an unexpected response", cause: ErrSettleBadResponse}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &SettleError{Reason: "facilitator returned an unexpected response", cause: ErrSettleBadResponse}
	}

	var settleResp x402types.SettleResponse
	if err := json.Unmarshal(raw, &settleResp); err != nil {
		return nil, &SettleError{Reason: "facilitator returned an unexpected response", cause: ErrSettleBadResponse}
	}

	if !settleResp.Success {
		reason := settleResp.ErrorReason
		if reason == "" {
			reason = "facilitator declined to settle"
		}
		return nil, &SettleError{Reason: reason, cause: ErrSettleRejected}
	}

	return &SettleResult{
		TxHash:         settleResp.Transaction,
		Network:        settleResp.Network,
		Payer:          settleResp.Payer,
		FacilitatorFee: settleResp.FacilitatorFee,
	}, nil
}

// setFacilitatorAuth sets the outbound authentication header for the
// facilitator call based on the resolved credential's AuthType, mirroring
// proxy.injectCredential's convention for the upstream credential. No header
// is set for AuthTypeNone or an empty plaintext. The plaintext is read but
// never logged or echoed (AGENTS.md invariant #4).
func setFacilitatorAuth(h http.Header, authType credential.AuthType, plaintext []byte) {
	if len(plaintext) == 0 || authType == credential.AuthTypeNone {
		return
	}
	switch authType {
	case credential.AuthTypeBearer:
		h.Set("Authorization", "Bearer "+string(plaintext))
	case credential.AuthTypeAPIKey:
		h.Set("X-Api-Key", string(plaintext))
	}
}

// FacilitatorCredentialResolver is the subset of *credential.Service the
// facilitator client needs to resolve a facilitator_credential_ref to its
// AuthType and plaintext secret. It mirrors proxy.CredentialService (the
// analogous seam for upstream credentials) and is kept separate from
// httpapi.CredentialService (the /v1/credentials CRUD surface), which has no
// need for Decrypt.
type FacilitatorCredentialResolver interface {
	Get(ctx context.Context, tenantID, id string) (*credential.Credential, error)
	Decrypt(ctx context.Context, tenantID, id string) ([]byte, error)
}

// resolveFacilitator resolves a tenant's settlement config and facilitator
// credential to the inputs Settler.Settle needs: the facilitator URL, the
// credential's AuthType, and its decrypted plaintext (empty for
// AuthTypeNone). Returns settlement.ErrNotFound unchanged when the tenant has
// not configured settlement (spec 0006 Decision 1: absent config means
// gate-only, no settlement attempted).
//
// The caller owns the returned plaintext and must zero it once the settle
// call completes (as the proxy forwarder does for the upstream credential,
// AGENTS.md invariant #4).
func resolveFacilitator(ctx context.Context, store settlement.Store, credSvc FacilitatorCredentialResolver, tenantID string) (facilitatorURL string, authType credential.AuthType, credPlaintext []byte, err error) {
	cfg, err := store.Get(ctx, tenantID)
	if err != nil {
		return "", "", nil, err
	}

	cred, err := credSvc.Get(ctx, tenantID, cfg.FacilitatorCredentialRef)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve facilitator credential: %w", err)
	}

	var plaintext []byte
	if cred.AuthType != credential.AuthTypeNone {
		plaintext, err = credSvc.Decrypt(ctx, tenantID, cfg.FacilitatorCredentialRef)
		if err != nil {
			return "", "", nil, fmt.Errorf("decrypt facilitator credential: %w", err)
		}
	}

	return cfg.FacilitatorURL, cred.AuthType, plaintext, nil
}
