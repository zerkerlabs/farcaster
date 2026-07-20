package httpapi

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
	"github.com/zerkerlabs/farcaster/x402types"
)

// signedX402Header builds a base64 X-PAYMENT whose EIP-3009 authorization is
// really signed by priv (To=payTo, Value=amount, valid for the next hour). It
// goes through the same JSON wire encoding a caller would send, so it exercises
// parseX402Payment's struct tags in addition to the crypto.
func signedX402Header(t *testing.T, priv *secp256k1.PrivateKey, payTo, amount string) string {
	t.Helper()
	now := time.Now().UTC().Unix()
	authz := x402types.Authorization{
		From:        ethAddressOf(t, priv),
		To:          payTo,
		Value:       amount,
		ValidAfter:  strconv.FormatInt(now-60, 10),
		ValidBefore: strconv.FormatInt(now+3600, 10),
		Nonce:       "0x" + hex.EncodeToString(make([]byte, 32)),
	}
	pmt := &x402types.Payment{
		X402Version: x402types.Version,
		Scheme:      x402Scheme,
		Network:     pricingNetwork,
		Payload:     x402types.PaymentPayload{Signature: signAuthz(t, priv, authz), Authorization: authz},
	}
	return encodePayment(t, pmt)
}

// realSignedRequest builds a proxy request carrying a genuinely-signed X-PAYMENT
// for seedVerifyAgent (PayTo=verifyTestPayTo, price 10000), signed by a fresh key.
func realSignedRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-PAYMENT", signedX402Header(t, priv, verifyTestPayTo, "10000"))
	return req.WithContext(authtest.WithIdentity(req.Context(), verifyTestTenant, verifyTestUser))
}

// TestHandleTransact_X402_ForwardsRealSignedPayment is the end-to-end proof of
// the ticket's acceptance criterion "a correctly-signed EIP-3009 authorization
// for the right domain verifies and is forwarded": a real signature goes through
// the DEFAULT (production) baseUSDCVerifier — via parseX402Payment +
// checkPaymentParams + the mux, with NO injected verifier — and is forwarded.
// This catches wiring regressions (default not applied, or a wire-JSON field
// name that no longer matches the internal struct) that the unit tests cannot.
func TestHandleTransact_X402_ForwardsRealSignedPayment(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
	invStore := invocation.NewMemoryStore()
	// No WithPaymentVerifier: exercise the real default verifier.
	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invStore)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, realSignedRequest(t, "/v1/proxy/"+agentID))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (real signature verified + forwarded); body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Farcaster-Invocation-ID"); got == "" {
		t.Error("X-Farcaster-Invocation-ID missing on forwarded call")
	}
	if _, total, err := invStore.List(context.Background(), verifyTestTenant, agentID, 1, 50); err != nil || total != 1 {
		t.Errorf("invocations created = %d, want 1 (err=%v)", total, err)
	}
}

// TestHandleStream_X402_ForwardsRealSignedPayment is the streaming counterpart.
func TestHandleStream_X402_ForwardsRealSignedPayment(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/stream")
	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invocation.NewMemoryStore())

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, realSignedRequest(t, "/v1/proxy/"+agentID+"/stream"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (real signature verified + forwarded); body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Farcaster-Invocation-ID"); got == "" {
		t.Error("X-Farcaster-Invocation-ID missing on forwarded call")
	}
}
