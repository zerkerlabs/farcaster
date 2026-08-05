package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
	"github.com/zerkerlabs/gateway/gateway/internal/settlement"
)

// TestHandleStream_SettleThenForward_Success is the acceptance test for the
// streaming happy path: a verified payment on a settlement-enabled route
// settles before the upstream stream is opened, and X-PAYMENT-RESPONSE rides
// the (synchronous, first-byte) response while the invocation settlement
// sub-record remains the canonical receipt (spec 0006 Decisions 2, 3).
func TestHandleStream_SettleThenForward_Success(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/stream")
	invStore := invocation.NewMemoryStore()
	settlementStore := settlement.NewMemoryStore()
	seedSettlementConfig(t, settlementStore)

	settler := &fakeSettler{outcomes: []fakeSettleOutcome{
		{result: &SettleResult{TxHash: "0xstreamed", Network: "base", Payer: "0x2222222222222222222222222222222222222222"}},
	}}

	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invStore).
		WithPaymentVerifier(passThroughVerifier{}).
		WithSettlement(settlementStore).
		WithSettler(settler, noAuthFacilitatorCred)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID+"/stream"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (settled, then streamed); body = %s", rec.Code, rec.Body.String())
	}
	if settler.callCount() != 1 {
		t.Errorf("settler called %d times, want 1", settler.callCount())
	}

	hdr := rec.Header().Get("X-PAYMENT-RESPONSE")
	if hdr == "" {
		t.Fatal("X-PAYMENT-RESPONSE missing on streaming response")
	}
	raw, err := base64.StdEncoding.DecodeString(hdr)
	if err != nil {
		t.Fatalf("X-PAYMENT-RESPONSE not valid base64: %v", err)
	}
	var got x402PaymentResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("X-PAYMENT-RESPONSE not valid JSON: %v", err)
	}
	if !got.Success || got.Transaction != "0xstreamed" || got.Network != "base" {
		t.Errorf("X-PAYMENT-RESPONSE = %+v, want success tx=0xstreamed network=base", got)
	}

	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	inv := waitInvocationTerminal(t, invStore, invID)
	if inv.Status != invocation.StatusSucceeded {
		t.Fatalf("Status = %q, want succeeded", inv.Status)
	}
	if inv.SettlementStatus == nil || *inv.SettlementStatus != invocation.SettlementStatusSettled {
		t.Fatalf("SettlementStatus = %v, want settled", inv.SettlementStatus)
	}
	if inv.SettlementTxHash == nil || *inv.SettlementTxHash != "0xstreamed" {
		t.Errorf("SettlementTxHash = %v, want 0xstreamed", inv.SettlementTxHash)
	}
}

// TestHandleStream_SettleFailure_NoStream is the acceptance test for the
// streaming fail-closed path: a terminal settle failure yields no stream at
// all — the upstream is never opened, and the invocation ends
// settlement_failed (spec 0006 acceptance bullet 1).
func TestHandleStream_SettleFailure_NoStream(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/stream")
	invStore := invocation.NewMemoryStore()
	settlementStore := settlement.NewMemoryStore()
	seedSettlementConfig(t, settlementStore)

	settler := &fakeSettler{outcomes: []fakeSettleOutcome{
		{err: &SettleError{Reason: "insufficient_funds", cause: ErrSettleRejected}},
	}}

	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invStore).
		WithPaymentVerifier(passThroughVerifier{}).
		WithSettlement(settlementStore).
		WithSettler(settler, noAuthFacilitatorCred)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID+"/stream"))

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (settlement failed, no stream); body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == `{"ok":true}` {
		t.Error("response body looks like the upstream stream leaked through despite settle failure")
	}
	if hdr := rec.Header().Get("X-PAYMENT-RESPONSE"); hdr != "" {
		t.Errorf("X-PAYMENT-RESPONSE = %q, want empty on settle failure", hdr)
	}

	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	if invID == "" {
		t.Fatal("X-Zerker-Invocation-ID missing even on settle failure")
	}
	inv := waitInvocationTerminal(t, invStore, invID)
	if inv.Status != invocation.StatusFailed {
		t.Fatalf("Status = %q, want failed", inv.Status)
	}
	if inv.SettlementStatus == nil || *inv.SettlementStatus != invocation.SettlementStatusSettlementFailed {
		t.Fatalf("SettlementStatus = %v, want settlement_failed", inv.SettlementStatus)
	}
	if inv.SettlementReason == nil || *inv.SettlementReason != "insufficient_funds" {
		t.Errorf("SettlementReason = %v, want insufficient_funds", inv.SettlementReason)
	}
	if inv.UpstreamStatus != nil {
		t.Errorf("UpstreamStatus = %v, want nil (upstream must never be called on settle failure)", inv.UpstreamStatus)
	}
}

// TestHandleStream_NoFacilitatorConfigured_GateOnlyUnchanged confirms that
// when a Settler is wired but the tenant has not configured a facilitator,
// the streaming route behaves exactly as gate-only surface-5: no settle
// attempt, no X-PAYMENT-RESPONSE header (spec 0006 Decision 1).
func TestHandleStream_NoFacilitatorConfigured_GateOnlyUnchanged(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/stream")
	invStore := invocation.NewMemoryStore()
	// settlementStore has no config for verifyTestTenant.
	settlementStore := settlement.NewMemoryStore()

	settler := &fakeSettler{outcomes: []fakeSettleOutcome{
		{err: errUnexpectedSettleCall},
	}}

	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invStore).
		WithPaymentVerifier(passThroughVerifier{}).
		WithSettlement(settlementStore).
		WithSettler(settler, noAuthFacilitatorCred)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID+"/stream"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (gate-only forward, no facilitator configured); body = %s", rec.Code, rec.Body.String())
	}
	if settler.callCount() != 0 {
		t.Errorf("settler called %d times, want 0 (no facilitator configured for this tenant)", settler.callCount())
	}
	if hdr := rec.Header().Get("X-PAYMENT-RESPONSE"); hdr != "" {
		t.Errorf("X-PAYMENT-RESPONSE = %q, want empty (settlement never attempted)", hdr)
	}
}
