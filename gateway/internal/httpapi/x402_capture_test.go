package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
)

// verifyGetRequest returns an authenticated GET request under verifyTestTenant
// (mirrors verifyRequest in x402_verify_test.go, for polling).
func verifyGetRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	return req.WithContext(authtest.WithIdentity(req.Context(), verifyTestTenant, verifyTestUser))
}

// TestHandleTransact_X402_CapturesPaymentMetadata verifies a call with a valid,
// verified payment records network/asset/amount/payer/nonce on the invocation
// and exposes them on the poll read (spec 0005 T5, mirroring surface-4's
// mcp_method/mcp_tool additive capture, issue #93).
func TestHandleTransact_X402_CapturesPaymentMetadata(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
	invStore := invocation.NewMemoryStore()
	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invStore).
		WithPaymentVerifier(passThroughVerifier{})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (forwarded with permissive verifier); body = %s", rec.Code, rec.Body.String())
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")

	want := samplePayment()
	inv, err := invStore.Get(context.Background(), verifyTestTenant, invID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if inv.PaymentNetwork == nil || *inv.PaymentNetwork != want.Network {
		t.Errorf("PaymentNetwork = %v, want %q", inv.PaymentNetwork, want.Network)
	}
	if inv.PaymentAsset == nil || *inv.PaymentAsset != "USDC" {
		t.Errorf("PaymentAsset = %v, want %q (configured Pricing.Asset)", inv.PaymentAsset, "USDC")
	}
	if inv.PaymentAmount == nil || *inv.PaymentAmount != want.Payload.Authorization.Value {
		t.Errorf("PaymentAmount = %v, want %q", inv.PaymentAmount, want.Payload.Authorization.Value)
	}
	if inv.PaymentPayer == nil || *inv.PaymentPayer != want.Payload.Authorization.From {
		t.Errorf("PaymentPayer = %v, want %q", inv.PaymentPayer, want.Payload.Authorization.From)
	}
	if inv.PaymentNonce == nil || *inv.PaymentNonce != want.Payload.Authorization.Nonce {
		t.Errorf("PaymentNonce = %v, want %q", inv.PaymentNonce, want.Payload.Authorization.Nonce)
	}

	// Exposed on the poll read too (GET /v1/proxy/{id}/invocations/{inv_id}).
	pollRec := httptest.NewRecorder()
	mux.ServeHTTP(pollRec, verifyGetRequest(t, "/v1/proxy/"+agentID+"/invocations/"+invID))
	if pollRec.Code != http.StatusOK {
		t.Fatalf("poll status = %d, want 200; body = %s", pollRec.Code, pollRec.Body.String())
	}
	var polled map[string]any
	if err := json.Unmarshal(pollRec.Body.Bytes(), &polled); err != nil {
		t.Fatalf("decode poll response: %v", err)
	}
	if polled["payment_network"] != want.Network {
		t.Errorf("poll payment_network = %v, want %q", polled["payment_network"], want.Network)
	}
	if polled["payment_asset"] != "USDC" {
		t.Errorf("poll payment_asset = %v, want %q", polled["payment_asset"], "USDC")
	}
	if polled["payment_amount"] != want.Payload.Authorization.Value {
		t.Errorf("poll payment_amount = %v, want %q", polled["payment_amount"], want.Payload.Authorization.Value)
	}
	if polled["payment_payer"] != want.Payload.Authorization.From {
		t.Errorf("poll payment_payer = %v, want %q", polled["payment_payer"], want.Payload.Authorization.From)
	}
	if polled["payment_nonce"] != want.Payload.Authorization.Nonce {
		t.Errorf("poll payment_nonce = %v, want %q", polled["payment_nonce"], want.Payload.Authorization.Nonce)
	}
}

// TestHandleStream_X402_CapturesPaymentMetadata is the streaming-endpoint
// counterpart: the same gate + capture applies on POST /v1/proxy/{id}/stream
// (spec 0005 T5 — both endpoints).
func TestHandleStream_X402_CapturesPaymentMetadata(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/stream")
	invStore := invocation.NewMemoryStore()
	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invStore).
		WithPaymentVerifier(passThroughVerifier{})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID+"/stream"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (forwarded with permissive verifier); body = %s", rec.Code, rec.Body.String())
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")

	want := samplePayment()
	inv, err := invStore.Get(context.Background(), verifyTestTenant, invID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if inv.PaymentNetwork == nil || *inv.PaymentNetwork != want.Network {
		t.Errorf("PaymentNetwork = %v, want %q", inv.PaymentNetwork, want.Network)
	}
	if inv.PaymentPayer == nil || *inv.PaymentPayer != want.Payload.Authorization.From {
		t.Errorf("PaymentPayer = %v, want %q", inv.PaymentPayer, want.Payload.Authorization.From)
	}
}
