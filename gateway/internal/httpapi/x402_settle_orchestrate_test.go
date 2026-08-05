package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/credential"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
	"github.com/zerkerlabs/farcaster/gateway/internal/proxy"
	"github.com/zerkerlabs/farcaster/gateway/internal/settlement"
	"github.com/zerkerlabs/farcaster/x402types"
)

// fakeSettleOutcome is one scripted result for fakeSettler.Settle.
type fakeSettleOutcome struct {
	result *SettleResult
	err    error
}

// fakeSettler is a scripted, in-process Settler test double: it returns
// outcomes[i] on its (i+1)th call, repeating the last outcome once the script
// is exhausted. It lets tests exercise the bounded-retry loop deterministically
// (spec 0006 Decision 4) without a real facilitator.
type fakeSettler struct {
	outcomes []fakeSettleOutcome
	calls    atomic.Int32
}

func (f *fakeSettler) Settle(_ context.Context, _ string, _ credential.AuthType, _ []byte, _ *x402types.Payment, _ x402types.PaymentRequirements) (*SettleResult, error) {
	i := int(f.calls.Add(1)) - 1
	if i >= len(f.outcomes) {
		i = len(f.outcomes) - 1
	}
	o := f.outcomes[i]
	return o.result, o.err
}

func (f *fakeSettler) callCount() int { return int(f.calls.Load()) }

// countingForwarder wraps verifyFwd and records whether Do was ever invoked,
// so tests can assert the upstream was never called on a settle failure
// (spec 0006 Decision 4: "served but unsettled" cannot occur).
type countingForwarder struct {
	verifyFwd
	calls atomic.Int32
}

func (f *countingForwarder) Do(ctx context.Context, tenantID, agentID, invocationID string, r *http.Request) (*proxy.Result, error) {
	f.calls.Add(1)
	return f.verifyFwd.Do(ctx, tenantID, agentID, invocationID, r)
}

// seedSettlementConfig configures a facilitator for verifyTestTenant so
// settle-then-forward runs on priced calls (spec 0006 Decision 1). Scoped to
// verifyTestTenant by design, mirroring proxy_test.go's waitForStatus — every
// current caller uses seedVerifyAgent's tenant, so the parameter was dropped
// rather than left as an always-constant argument (unparam).
func seedSettlementConfig(t *testing.T, store settlement.Store) {
	t.Helper()
	url := "https://facilitator.example.com"
	ref := "cred_facilitator"
	if _, err := store.Upsert(context.Background(), verifyTestTenant, settlement.UpdateFields{
		FacilitatorURL:           &url,
		FacilitatorCredentialRef: &ref,
	}); err != nil {
		t.Fatalf("seed settlement config: %v", err)
	}
}

// noAuthFacilitatorCred is a FacilitatorCredentialResolver stub returning a
// no-auth credential — settle tests don't exercise facilitator auth headers
// (that's x402_settle_test.go's job), only the orchestration around Settle.
var noAuthFacilitatorCred = &stubFacilitatorCred{
	cred: &credential.Credential{ID: "cred_facilitator", AuthType: credential.AuthTypeNone},
}

// waitInvocationTerminal polls invStore until invocationID reaches a terminal
// (succeeded/failed) status or the deadline elapses. Settlement runs in
// runTransact's background goroutine, so callers must poll rather than assert
// immediately after the 202 response. Scoped to verifyTestTenant for the same
// unparam reason as seedSettlementConfig.
func waitInvocationTerminal(t *testing.T, invStore invocation.Store, invocationID string) *invocation.Invocation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		inv, err := invStore.Get(context.Background(), verifyTestTenant, invocationID)
		if err == nil && (inv.Status == invocation.StatusSucceeded || inv.Status == invocation.StatusFailed) {
			return inv
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("invocation %s did not reach a terminal status within 2s", invocationID)
	return nil
}

// TestHandleTransact_SettleThenForward_Success is the acceptance test for the
// happy path: a verified payment on a settlement-enabled route settles via the
// facilitator before the upstream is called, and the settlement sub-record is
// written (spec 0006 acceptance bullet 1).
func TestHandleTransact_SettleThenForward_Success(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
	invStore := invocation.NewMemoryStore()
	settlementStore := settlement.NewMemoryStore()
	seedSettlementConfig(t, settlementStore)

	settler := &fakeSettler{outcomes: []fakeSettleOutcome{
		{result: &SettleResult{TxHash: "0xsettled", Network: "base", Payer: "0x2222222222222222222222222222222222222222"}},
	}}

	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invStore).
		WithPaymentVerifier(passThroughVerifier{}).
		WithSettlement(settlementStore).
		WithSettler(settler, noAuthFacilitatorCred)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")

	inv := waitInvocationTerminal(t, invStore, invID)
	if inv.Status != invocation.StatusSucceeded {
		t.Fatalf("Status = %q, want succeeded (settle succeeded, upstream forwarded)", inv.Status)
	}
	if inv.UpstreamStatus == nil || *inv.UpstreamStatus != 200 {
		t.Errorf("UpstreamStatus = %v, want 200 (upstream must be called after a settled response)", inv.UpstreamStatus)
	}
	if inv.SettlementStatus == nil || *inv.SettlementStatus != invocation.SettlementStatusSettled {
		t.Fatalf("SettlementStatus = %v, want settled", inv.SettlementStatus)
	}
	if inv.SettlementTxHash == nil || *inv.SettlementTxHash != "0xsettled" {
		t.Errorf("SettlementTxHash = %v, want 0xsettled", inv.SettlementTxHash)
	}
	want := samplePayment()
	if inv.SettledAmount == nil || *inv.SettledAmount != want.Payload.Authorization.Value {
		t.Errorf("SettledAmount = %v, want %q", inv.SettledAmount, want.Payload.Authorization.Value)
	}
	if inv.OperatorAmount == nil || *inv.OperatorAmount != want.Payload.Authorization.Value {
		t.Errorf("OperatorAmount = %v, want %q (no facilitator fee reported)", inv.OperatorAmount, want.Payload.Authorization.Value)
	}
	if inv.SettlementAttempts == nil || *inv.SettlementAttempts != 1 {
		t.Errorf("SettlementAttempts = %v, want 1", inv.SettlementAttempts)
	}
	if inv.SettledAt == nil {
		t.Error("SettledAt = nil, want set")
	}
	if settler.callCount() != 1 {
		t.Errorf("settler called %d times, want 1", settler.callCount())
	}
}

// TestHandleTransact_SettleThenForward_FeeSplit confirms operator_amount
// reflects a facilitator-reported fee (settled_amount - facilitator_fee, spec
// 0006 settlement sub-record).
func TestHandleTransact_SettleThenForward_FeeSplit(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
	invStore := invocation.NewMemoryStore()
	settlementStore := settlement.NewMemoryStore()
	seedSettlementConfig(t, settlementStore)

	settler := &fakeSettler{outcomes: []fakeSettleOutcome{
		{result: &SettleResult{TxHash: "0xfee", FacilitatorFee: "300"}},
	}}

	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invStore).
		WithPaymentVerifier(passThroughVerifier{}).
		WithSettlement(settlementStore).
		WithSettler(settler, noAuthFacilitatorCred)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID))
	invID := rec.Header().Get("X-Zerker-Invocation-ID")

	inv := waitInvocationTerminal(t, invStore, invID)
	if inv.FacilitatorFee == nil || *inv.FacilitatorFee != "300" {
		t.Errorf("FacilitatorFee = %v, want 300", inv.FacilitatorFee)
	}
	if inv.OperatorAmount == nil || *inv.OperatorAmount != "9700" {
		t.Errorf("OperatorAmount = %v, want 9700 (10000 - 300)", inv.OperatorAmount)
	}
}

// TestHandleTransact_SettleFailure_NoForward is the acceptance test for the
// fail-closed path: a terminal (non-transient) settle failure ends the
// invocation settlement_failed and the upstream is never called (spec 0006
// acceptance bullet 2, Decision 4).
func TestHandleTransact_SettleFailure_NoForward(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
	invStore := invocation.NewMemoryStore()
	settlementStore := settlement.NewMemoryStore()
	seedSettlementConfig(t, settlementStore)

	settler := &fakeSettler{outcomes: []fakeSettleOutcome{
		{err: &SettleError{Reason: "insufficient_funds", cause: ErrSettleRejected}},
	}}
	fwd := &countingForwarder{verifyFwd: verifyFwd{status: 200}}

	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore).
		WithPaymentVerifier(passThroughVerifier{}).
		WithSettlement(settlementStore).
		WithSettler(settler, noAuthFacilitatorCred)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (invocation still created; settlement happens async); body = %s", rec.Code, rec.Body.String())
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")

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
	if inv.SettlementAttempts == nil || *inv.SettlementAttempts != 1 {
		t.Errorf("SettlementAttempts = %v, want 1 (non-transient failure is not retried)", inv.SettlementAttempts)
	}
	if inv.UpstreamStatus != nil {
		t.Errorf("UpstreamStatus = %v, want nil (upstream must never be called on settle failure)", inv.UpstreamStatus)
	}
	if fwd.calls.Load() != 0 {
		t.Errorf("forwarder.Do called %d times, want 0", fwd.calls.Load())
	}
}

// TestHandleTransact_SettleTransientFailure_RetriesThenSucceeds confirms a
// transient facilitator error (unreachable/bad response) is retried up to the
// bound and can still succeed within it (spec 0006 acceptance bullet 2:
// "transient failures retry a bounded number of times").
func TestHandleTransact_SettleTransientFailure_RetriesThenSucceeds(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
	invStore := invocation.NewMemoryStore()
	settlementStore := settlement.NewMemoryStore()
	seedSettlementConfig(t, settlementStore)

	settler := &fakeSettler{outcomes: []fakeSettleOutcome{
		{err: &SettleError{Reason: "facilitator unreachable", cause: ErrSettleUnreachable}},
		{err: &SettleError{Reason: "facilitator unreachable", cause: ErrSettleUnreachable}},
		{result: &SettleResult{TxHash: "0xretried"}},
	}}

	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invStore).
		WithPaymentVerifier(passThroughVerifier{}).
		WithSettlement(settlementStore).
		WithSettler(settler, noAuthFacilitatorCred)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID))
	invID := rec.Header().Get("X-Zerker-Invocation-ID")

	inv := waitInvocationTerminal(t, invStore, invID)
	if inv.Status != invocation.StatusSucceeded {
		t.Fatalf("Status = %q, want succeeded (transient failures recovered within bound)", inv.Status)
	}
	if inv.SettlementStatus == nil || *inv.SettlementStatus != invocation.SettlementStatusSettled {
		t.Fatalf("SettlementStatus = %v, want settled", inv.SettlementStatus)
	}
	if inv.SettlementAttempts == nil || *inv.SettlementAttempts != 3 {
		t.Errorf("SettlementAttempts = %v, want 3", inv.SettlementAttempts)
	}
	if settler.callCount() != 3 {
		t.Errorf("settler called %d times, want 3", settler.callCount())
	}
}

// TestHandleTransact_SettleTransientFailure_ExhaustsRetries confirms a settle
// attempt that stays transiently broken goes terminal once the retry bound is
// exhausted, still never calling the upstream.
func TestHandleTransact_SettleTransientFailure_ExhaustsRetries(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
	invStore := invocation.NewMemoryStore()
	settlementStore := settlement.NewMemoryStore()
	seedSettlementConfig(t, settlementStore)

	settler := &fakeSettler{outcomes: []fakeSettleOutcome{
		{err: &SettleError{Reason: "facilitator unreachable", cause: ErrSettleUnreachable}},
	}}
	fwd := &countingForwarder{verifyFwd: verifyFwd{status: 200}}

	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore).
		WithPaymentVerifier(passThroughVerifier{}).
		WithSettlement(settlementStore).
		WithSettler(settler, noAuthFacilitatorCred)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID))
	invID := rec.Header().Get("X-Zerker-Invocation-ID")

	inv := waitInvocationTerminal(t, invStore, invID)
	if inv.Status != invocation.StatusFailed {
		t.Fatalf("Status = %q, want failed", inv.Status)
	}
	if inv.SettlementStatus == nil || *inv.SettlementStatus != invocation.SettlementStatusSettlementFailed {
		t.Fatalf("SettlementStatus = %v, want settlement_failed", inv.SettlementStatus)
	}
	if inv.SettlementAttempts == nil || *inv.SettlementAttempts != 1+settleMaxRetries {
		t.Errorf("SettlementAttempts = %v, want %d (bounded retry exhausted)", inv.SettlementAttempts, 1+settleMaxRetries)
	}
	if fwd.calls.Load() != 0 {
		t.Errorf("forwarder.Do called %d times, want 0", fwd.calls.Load())
	}
}

// TestHandleTransact_NoFacilitatorConfigured_GateOnlyUnchanged confirms that
// when a Settler is wired but the calling tenant has not configured a
// facilitator, settlement is skipped entirely and the call behaves exactly as
// gate-only surface-5 (spec 0006 Decision 1, acceptance bullet 3).
func TestHandleTransact_NoFacilitatorConfigured_GateOnlyUnchanged(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
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
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")

	inv := waitInvocationTerminal(t, invStore, invID)
	if inv.Status != invocation.StatusSucceeded {
		t.Fatalf("Status = %q, want succeeded (gate-only forward, no facilitator configured)", inv.Status)
	}
	if inv.SettlementStatus != nil {
		t.Errorf("SettlementStatus = %v, want nil (settlement never attempted)", inv.SettlementStatus)
	}
	if settler.callCount() != 0 {
		t.Errorf("settler called %d times, want 0 (no facilitator configured for this tenant)", settler.callCount())
	}
}

// errUnexpectedSettleCall is a sentinel returned by a fakeSettler that a test
// asserts should never be invoked at all.
var errUnexpectedSettleCall = &SettleError{Reason: "settle should not have been called", cause: ErrSettleRejected}

// TestHandleTransact_SettleEnabled_ReplayPreFilterStillRejects confirms the
// surface-5 local nonce store still catches an obvious replay before the
// facilitator is ever called, even on a settlement-enabled route (spec 0006
// acceptance bullet 3, Decision 8: the local store is a pre-filter, not
// replaced, by settlement).
func TestHandleTransact_SettleEnabled_ReplayPreFilterStillRejects(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
	invStore := invocation.NewMemoryStore()
	settlementStore := settlement.NewMemoryStore()
	seedSettlementConfig(t, settlementStore)

	settler := &fakeSettler{outcomes: []fakeSettleOutcome{
		{result: &SettleResult{TxHash: "0xfirst"}},
	}}

	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invStore).
		WithPaymentVerifier(passThroughVerifier{}).
		WithSettlement(settlementStore).
		WithSettler(settler, noAuthFacilitatorCred)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	path := "/v1/proxy/" + agentID
	header := wellFormedHeader(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, gatePostRequest(path, header))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first call status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	waitInvocationTerminal(t, invStore, invID)

	// Replay of the same (payer, nonce): rejected by the local pre-filter before
	// the facilitator is ever contacted.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, gatePostRequest(path, header))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("replayed call status = %d, want 402; body = %s", rec.Code, rec.Body.String())
	}
	if settler.callCount() != 1 {
		t.Errorf("settler called %d times, want 1 (replay must be rejected before reaching the facilitator)", settler.callCount())
	}
}
