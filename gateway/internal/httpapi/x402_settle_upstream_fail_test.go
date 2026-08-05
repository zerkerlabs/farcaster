package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
	"github.com/zerkerlabs/gateway/gateway/internal/proxy"
	"github.com/zerkerlabs/gateway/gateway/internal/settlement"
)

// verifyFwdErr is a minimal ProxyForwarder that always fails, simulating a
// terminal upstream failure after surface-2's retries/circuit-breaking are
// already exhausted (spec 0006 Decision 5) — proxy.Do/DoStream return
// ErrUpstreamUnreachable in exactly that situation.
type verifyFwdErr struct {
	err   error
	calls atomic.Int32
}

func (f *verifyFwdErr) Do(context.Context, string, string, string, *http.Request) (*proxy.Result, error) {
	f.calls.Add(1)
	return nil, f.err
}

func (f *verifyFwdErr) DoStream(context.Context, string, string, string, *http.Request) (*proxy.Result, error) {
	f.calls.Add(1)
	return nil, f.err
}

// TestHandleTransact_SettleForwardMatrix is the acceptance test for the
// settle/forward ordering + fail-closed matrix (spec 0006 T6): settle-fail
// never forwards, a settled-then-terminal-upstream-failure retains the
// receipt under settled_upstream_failed, and the happy path ends settled.
func TestHandleTransact_SettleForwardMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		settler            *fakeSettler
		fwd                ProxyForwarder
		wantInvocationOK   bool
		wantStatus         invocation.Status
		wantSettlement     invocation.SettlementStatus
		wantForwarderCalls int32
		wantTxHashRetained bool
	}{
		{
			name: "settle_fail_no_forward",
			settler: &fakeSettler{outcomes: []fakeSettleOutcome{
				{err: &SettleError{Reason: "insufficient_funds", cause: ErrSettleRejected}},
			}},
			fwd:                &countingForwarder{verifyFwd: verifyFwd{status: 200}},
			wantInvocationOK:   true,
			wantStatus:         invocation.StatusFailed,
			wantSettlement:     invocation.SettlementStatusSettlementFailed,
			wantForwarderCalls: 0,
		},
		{
			name: "settled_then_upstream_terminal_fail",
			settler: &fakeSettler{outcomes: []fakeSettleOutcome{
				{result: &SettleResult{TxHash: "0xupstreamfail"}},
			}},
			fwd:                &verifyFwdErr{err: proxy.ErrUpstreamUnreachable},
			wantInvocationOK:   true,
			wantStatus:         invocation.StatusFailed,
			wantSettlement:     invocation.SettlementStatusSettledUpstreamFailed,
			wantForwarderCalls: 1,
			wantTxHashRetained: true,
		},
		{
			name: "happy_path_settled",
			settler: &fakeSettler{outcomes: []fakeSettleOutcome{
				{result: &SettleResult{TxHash: "0xhappy"}},
			}},
			fwd:                &countingForwarder{verifyFwd: verifyFwd{status: 200}},
			wantInvocationOK:   true,
			wantStatus:         invocation.StatusSucceeded,
			wantSettlement:     invocation.SettlementStatusSettled,
			wantForwarderCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := agent.NewMemoryStore()
			agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
			invStore := invocation.NewMemoryStore()
			settlementStore := settlement.NewMemoryStore()
			seedSettlementConfig(t, settlementStore)

			h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
				WithProxy(tt.fwd, invStore).
				WithPaymentVerifier(passThroughVerifier{}).
				WithSettlement(settlementStore).
				WithSettler(tt.settler, noAuthFacilitatorCred)

			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
			}
			invID := rec.Header().Get("X-Zerker-Invocation-ID")

			inv := waitInvocationTerminal(t, invStore, invID)
			if inv.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", inv.Status, tt.wantStatus)
			}
			if inv.SettlementStatus == nil || *inv.SettlementStatus != tt.wantSettlement {
				t.Errorf("SettlementStatus = %v, want %q", inv.SettlementStatus, tt.wantSettlement)
			}

			var gotCalls int32
			switch fwd := tt.fwd.(type) {
			case *countingForwarder:
				gotCalls = fwd.calls.Load()
			case *verifyFwdErr:
				gotCalls = fwd.calls.Load()
			}
			if gotCalls != tt.wantForwarderCalls {
				t.Errorf("forwarder called %d times, want %d", gotCalls, tt.wantForwarderCalls)
			}

			if tt.wantTxHashRetained {
				if inv.SettlementTxHash == nil || *inv.SettlementTxHash != "0xupstreamfail" {
					t.Errorf("SettlementTxHash = %v, want retained 0xupstreamfail (money already moved — no refund, spec 0006 Decision 5)", inv.SettlementTxHash)
				}
				want := samplePayment()
				if inv.SettledAmount == nil || *inv.SettledAmount != want.Payload.Authorization.Value {
					t.Errorf("SettledAmount = %v, want retained %q", inv.SettledAmount, want.Payload.Authorization.Value)
				}
			}
		})
	}
}

// TestHandleStream_SettleThenForward_UpstreamTerminalFail_SettledUpstreamFailed
// covers the same settled-then-upstream-terminal-fail edge on the streaming
// endpoint (spec 0006 Decision 2: settle-then-forward applies to both
// endpoints).
func TestHandleStream_SettleThenForward_UpstreamTerminalFail_SettledUpstreamFailed(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/stream")
	invStore := invocation.NewMemoryStore()
	settlementStore := settlement.NewMemoryStore()
	seedSettlementConfig(t, settlementStore)

	settler := &fakeSettler{outcomes: []fakeSettleOutcome{
		{result: &SettleResult{TxHash: "0xstreamupstreamfail"}},
	}}
	fwd := &verifyFwdErr{err: proxy.ErrUpstreamUnreachable}

	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore).
		WithPaymentVerifier(passThroughVerifier{}).
		WithSettlement(settlementStore).
		WithSettler(settler, noAuthFacilitatorCred)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID+"/stream"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (settled, then upstream unreachable); body = %s", rec.Code, rec.Body.String())
	}
	if fwd.calls.Load() != 1 {
		t.Errorf("forwarder called %d times, want 1 (settlement must precede the forward attempt)", fwd.calls.Load())
	}

	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	inv := waitInvocationTerminal(t, invStore, invID)
	if inv.Status != invocation.StatusFailed {
		t.Fatalf("Status = %q, want failed", inv.Status)
	}
	if inv.SettlementStatus == nil || *inv.SettlementStatus != invocation.SettlementStatusSettledUpstreamFailed {
		t.Fatalf("SettlementStatus = %v, want settled_upstream_failed", inv.SettlementStatus)
	}
	if inv.SettlementTxHash == nil || *inv.SettlementTxHash != "0xstreamupstreamfail" {
		t.Errorf("SettlementTxHash = %v, want retained 0xstreamupstreamfail (no refund, spec 0006 Decision 5)", inv.SettlementTxHash)
	}
}
