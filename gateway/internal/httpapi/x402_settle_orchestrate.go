package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
	"github.com/zerkerlabs/farcaster/gateway/internal/settlement"
	"github.com/zerkerlabs/farcaster/x402types"
)

// settleMaxRetries bounds the number of additional settle attempts after the
// first failure on a transient facilitator/RPC error (spec 0006 Decision 4:
// "transient failures retry a bounded number of times, then terminal").
const settleMaxRetries = 2

// settleRetryBaseDelay is the base exponential-backoff delay between settle
// retries, mirroring the proxy forwarder's retry convention
// (internal/proxy.DefaultRetryBaseDelay).
const settleRetryBaseDelay = 100 * time.Millisecond

// isTransientSettleError reports whether err is worth a bounded retry: the
// facilitator was unreachable (network/transport failure) or returned a
// response that didn't parse — both categories that a later attempt might
// succeed at. A well-formed rejection (ErrSettleRejected — e.g. the nonce was
// already consumed, or the payer's balance is insufficient) is a deterministic
// business decision the facilitator already evaluated on-chain state for;
// retrying immediately would not change the outcome, so it goes terminal on
// the first attempt (spec 0006 Decision 4).
func isTransientSettleError(err error) bool {
	return errors.Is(err, ErrSettleUnreachable) || errors.Is(err, ErrSettleBadResponse)
}

// settleFailureReason extracts the coarse, persistable reason from a settle
// error (spec 0006: "reason" is coarse, never raw response bytes). Falls back
// to a generic message for an error that isn't a *SettleError (defensive; the
// only producer of settle errors is Settler.Settle, which always returns one).
func settleFailureReason(err error) string {
	var settleErr *SettleError
	if errors.As(err, &settleErr) {
		return settleErr.Reason
	}
	return "settlement failed"
}

// zeroBytes overwrites b with zeros to limit the window during which
// plaintext credential material resides in memory (invariant #4, AGENTS.md),
// mirroring internal/proxy's zeroBytes for the analogous upstream-credential path.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// computeOperatorAmount returns settledAmount minus facilitatorFee as a
// smallest-unit decimal string (spec 0006: "operator_amount: what reached
// pay_to (settled_amount - facilitator_fee)"). facilitatorFee empty (the
// OSS/self-host path — no fee reported) yields operatorAmount == settledAmount.
// A malformed amount (shouldn't happen: settledAmount is a verified authz
// value already parsed as a big.Int by checkPaymentParams) falls back to
// settledAmount rather than panicking or fabricating a number.
func computeOperatorAmount(settledAmount, facilitatorFee string) string {
	if facilitatorFee == "" {
		return settledAmount
	}
	settled, ok := new(big.Int).SetString(settledAmount, 10)
	if !ok {
		return settledAmount
	}
	fee, ok := new(big.Int).SetString(facilitatorFee, 10)
	if !ok {
		return settledAmount
	}
	return new(big.Int).Sub(settled, fee).String()
}

// settleBeforeForward runs the settlement step of the invocation lifecycle:
// verify -> settle -> forward (spec 0006 Decisions 2, 4, 8). It is shared by
// both the transactional (runTransact) and streaming (handleStream) paths —
// the caller has already verified the payment locally (surface-5,
// synchronous) before calling this; this step is the one that reaches the
// network.
//
// It returns (true, result) when the caller should proceed to forward to the
// upstream: result is the facilitator's settle result when settlement ran and
// succeeded, or nil when the tenant has no facilitator configured
// (settlement.ErrNotFound — gate-only behavior, spec 0006 Decision 1: absent
// config means unpriced/unconfigured routes are unchanged). It returns
// (false, nil) when settlement failed: the invocation has already been
// updated to a terminal settlement_failed state, and the caller must never
// call the upstream (Decision 4 — "served but unsettled" cannot occur).
//
// Keeping this as the single, isolated entry point into settlement is what
// lets the later async (forward-then-settle) flip stay an orchestration
// change rather than a contract change (spec 0006 Decision 2).
func (h *Handler) settleBeforeForward(ctx context.Context, tenantID, invocationID string, pmt *x402types.Payment, req x402types.PaymentRequirements) (bool, *SettleResult) {
	facilitatorURL, authType, credPlaintext, err := resolveFacilitator(ctx, h.settlementStore, h.facilitatorCreds, tenantID)
	if err != nil {
		if errors.Is(err, settlement.ErrNotFound) {
			// No facilitator configured for this tenant: settlement is opt-in
			// (spec 0006 Decision 1). The priced route stays gate-only.
			return true, nil
		}
		h.logger.Error("settle: resolve facilitator config", "invocation_id", invocationID, "err", err)
		h.recordSettlementFailed(tenantID, invocationID, "facilitator configuration error", 0)
		return false, nil
	}
	defer zeroBytes(credPlaintext)

	maxAttempts := 1 + settleMaxRetries
	var lastErr error
	attempts := 0
	for attempts < maxAttempts {
		if attempts > 0 {
			backoff := settleRetryBaseDelay * time.Duration(1<<(attempts-1))
			select {
			case <-ctx.Done():
				h.recordSettlementFailed(tenantID, invocationID, "facilitator unreachable", attempts)
				return false, nil
			case <-time.After(backoff):
			}
		}
		attempts++

		result, sErr := h.settler.Settle(ctx, facilitatorURL, authType, credPlaintext, pmt, req)
		if sErr == nil {
			h.recordSettlementSucceeded(tenantID, invocationID, result, attempts, pmt.Payload.Authorization.Value)
			return true, result
		}
		lastErr = sErr
		if !isTransientSettleError(sErr) {
			break
		}
		h.logger.Warn("settle: retrying transient facilitator error",
			"invocation_id", invocationID, "attempt", attempts, "max_attempts", maxAttempts, "err", sErr)
	}

	h.recordSettlementFailed(tenantID, invocationID, settleFailureReason(lastErr), attempts)
	return false, nil
}

// x402PaymentResponse is the X-PAYMENT-RESPONSE header body: the
// facilitator's settle receipt, base64-JSON-encoded onto the response the
// same way X-PAYMENT itself is encoded on the request (spec 0006 Decision 3).
// Field names mirror x402types.SettleResponse (x402_settle.go, the coinbase
// x402 "typical flow" settle response shape) — pinned here as the header
// encoding per Decision 3 ("pin exact x402 field names/encoding at
// implementation").
type x402PaymentResponse struct {
	Success     bool   `json:"success"`
	Transaction string `json:"transaction,omitempty"`
	Network     string `json:"network,omitempty"`
	Payer       string `json:"payer,omitempty"`
}

// buildPaymentResponseHeader base64-JSON-encodes a successful settle result
// into the X-PAYMENT-RESPONSE header value (spec 0006 Decision 3: the
// streaming first byte is the one place a header receipt fits — the
// invocation settlement sub-record, not this header, remains the canonical
// receipt).
func buildPaymentResponseHeader(result *SettleResult) (string, error) {
	raw, err := json.Marshal(x402PaymentResponse{
		Success:     true,
		Transaction: result.TxHash,
		Network:     result.Network,
		Payer:       result.Payer,
	})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// applySettledUpstreamFailed upgrades a terminal-failed invocation's
// settlement status to settled_upstream_failed when the facilitator already
// settled payment for this invocation (spec 0006 Decision 5: the one residual
// post-collection edge — settlement succeeded, then the upstream call failed
// terminally, after surface-2's retries/circuit-breaking are exhausted). It is
// a no-op when settlement never ran or the invocation did not end failed, so
// callers can call it unconditionally right before every terminal Update on
// the forward path.
//
// It only sets SettlementStatus: tx_hash/settled_amount/etc. were already
// written by recordSettlementSucceeded, and UpdateFields' nil-means-unchanged
// semantics leave them as-is here — the receipt is retained, exactly as spec
// 0006 requires ("keep the tx_hash/settled_amount — the money did move"). No
// refund is attempted: the gateway holds no operator key.
func applySettledUpstreamFailed(fields *invocation.UpdateFields, settleResult *SettleResult) {
	if settleResult == nil || fields.Status == nil || *fields.Status != invocation.StatusFailed {
		return
	}
	status := invocation.SettlementStatusSettledUpstreamFailed
	fields.SettlementStatus = &status
}

// recordSettlementSucceeded writes the settlement sub-record for a successful
// facilitator settle (spec 0006 T3 fields). It never touches the invocation's
// top-level Status/CompletedAt — the caller still has to forward to the
// upstream, so the invocation stays Running until that completes.
func (h *Handler) recordSettlementSucceeded(tenantID, invocationID string, result *SettleResult, attempts int, settledAmount string) {
	status := invocation.SettlementStatusSettled
	now := time.Now().UTC()
	operatorAmount := computeOperatorAmount(settledAmount, result.FacilitatorFee)

	fields := invocation.UpdateFields{
		SettlementStatus:   &status,
		SettlementTxHash:   &result.TxHash,
		SettledAmount:      &settledAmount,
		OperatorAmount:     &operatorAmount,
		SettlementAttempts: &attempts,
		SettledAt:          &now,
	}
	if result.FacilitatorFee != "" {
		fields.FacilitatorFee = &result.FacilitatorFee
	}

	if _, err := h.invocations.Update(context.Background(), tenantID, invocationID, fields); err != nil {
		h.logger.Error("settle: record success", "invocation_id", invocationID, "err", err)
	}
}

// recordSettlementFailed writes the settlement sub-record for a failed settle
// attempt and ends the invocation terminally: Status becomes failed and
// CompletedAt is set, exactly as the upstream-failure path does further down
// runTransact — because on this path the upstream is never called, this is
// the only terminal write the invocation gets (spec 0006 Decision 4).
func (h *Handler) recordSettlementFailed(tenantID, invocationID, reason string, attempts int) {
	settlementStatus := invocation.SettlementStatusSettlementFailed
	failedStatus := invocation.StatusFailed
	now := time.Now().UTC()

	fields := invocation.UpdateFields{
		Status:             &failedStatus,
		CompletedAt:        &now,
		SettlementStatus:   &settlementStatus,
		SettlementReason:   &reason,
		SettlementAttempts: &attempts,
	}
	if _, err := h.invocations.Update(context.Background(), tenantID, invocationID, fields); err != nil {
		h.logger.Error("settle: record failure", "invocation_id", invocationID, "err", err)
	}
}
