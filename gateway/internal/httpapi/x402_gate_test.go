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

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
	"github.com/zerkerlabs/gateway/x402types"
)

// tamperedX402Header signs a well-formed authorization, then mutates its value
// AFTER signing (spec 0005: "A malformed X-PAYMENT is a 402, never a 5xx").
// The recomputed EIP-712 digest no longer matches the signature, so the
// default verifier rejects it as a signer mismatch — the "tampered" case
// (distinct from "replayed", which reuses a payload byte-for-byte).
func tamperedX402Header(t *testing.T, priv *secp256k1.PrivateKey, payTo string) string {
	t.Helper()
	now := time.Now().UTC().Unix()
	authz := x402types.Authorization{
		From:        ethAddressOf(t, priv),
		To:          payTo,
		Value:       "10000",
		ValidAfter:  strconv.FormatInt(now-60, 10),
		ValidBefore: strconv.FormatInt(now+3600, 10),
		Nonce:       "0x" + strings.Repeat("ab", 32),
	}
	sig := signAuthz(t, priv, authz)
	authz.Value = "999999" // tamper post-signature; still >= required amount
	pmt := &x402types.Payment{
		X402Version: x402types.Version,
		Scheme:      x402Scheme,
		Network:     pricingNetwork,
		Payload:     x402types.PaymentPayload{Signature: sig, Authorization: authz},
	}
	return encodePayment(t, pmt)
}

// gatePostRequest builds an authenticated POST to path, optionally carrying an
// X-PAYMENT header.
func gatePostRequest(path, xPayment string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if xPayment != "" {
		req.Header.Set("X-PAYMENT", xPayment)
	}
	return req.WithContext(authtest.WithIdentity(req.Context(), verifyTestTenant, verifyTestUser))
}

// TestX402Gate_FullFlow is the consolidated integration test for the full x402
// gate on a priced agent (spec 0005 Behavior, surface-5 T6): an unpaid call is
// challenged, a validly-signed payment is verified and forwarded, replaying
// that exact payment is rejected (the best-effort replay guard, T6), and a
// tampered payment is rejected — all against the real default PaymentVerifier
// (baseUSDCVerifier), exercised on both the transactional and streaming
// endpoints.
func TestX402Gate_FullFlow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		path       func(agentID string) string
		wantStatus int
	}{
		{"transactional", func(id string) string { return "/v1/proxy/" + id }, http.StatusAccepted},
		{"streaming", func(id string) string { return "/v1/proxy/" + id + "/stream" }, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := agent.NewMemoryStore()
			agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
			invStore := invocation.NewMemoryStore()
			h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
				WithProxy(verifyFwd{status: 200}, invStore)

			mux := http.NewServeMux()
			h.RegisterRoutes(mux)

			path := tc.path(agentID)

			// 1. No X-PAYMENT -> 402 challenge, no invocation created.
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, gatePostRequest(path, ""))
			if rec.Code != http.StatusPaymentRequired {
				t.Fatalf("no-payment status = %d, want 402; body = %s", rec.Code, rec.Body.String())
			}

			// 2. A validly-signed payment -> verified + forwarded.
			priv, err := secp256k1.GeneratePrivateKey()
			if err != nil {
				t.Fatalf("GeneratePrivateKey: %v", err)
			}
			validHeader := signedX402Header(t, priv, verifyTestPayTo, "10000")

			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, gatePostRequest(path, validHeader))
			if rec.Code != tc.wantStatus {
				t.Fatalf("valid payment status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}

			// 3. Replaying the identical authorization -> rejected (best-effort
			// nonce guard, spec 0005 T6): the chain never consumed this nonce (v1
			// never settles), but Zerker's local guard catches the re-presented
			// payload.
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, gatePostRequest(path, validHeader))
			if rec.Code != http.StatusPaymentRequired {
				t.Fatalf("replayed payment status = %d, want 402; body = %s", rec.Code, rec.Body.String())
			}

			// 4. A tampered payment (signed, then mutated) -> rejected.
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, gatePostRequest(path, tamperedX402Header(t, priv, verifyTestPayTo)))
			if rec.Code != http.StatusPaymentRequired {
				t.Fatalf("tampered payment status = %d, want 402; body = %s", rec.Code, rec.Body.String())
			}

			// Exactly one invocation was created — from the single valid call.
			if _, total, err := invStore.List(context.Background(), verifyTestTenant, agentID, 1, 50); err != nil || total != 1 {
				t.Errorf("invocations created = %d, want 1 (err=%v)", total, err)
			}
		})
	}
}

// TestX402Gate_FreshAuthorizationAfterReplayIsAccepted confirms the replay
// guard rejects only the exact re-presented (payer, nonce) pair — a fresh
// authorization (new nonce) from the same payer still passes (spec 0005
// Acceptance: "A replayed authorization is rejected; a fresh one passes.").
func TestX402Gate_FreshAuthorizationAfterReplayIsAccepted(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
	invStore := invocation.NewMemoryStore()
	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invStore)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	path := "/v1/proxy/" + agentID

	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}

	first := signedX402Header(t, priv, verifyTestPayTo, "10000")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, gatePostRequest(path, first))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first call status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}

	// Replay of the first authorization is rejected.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, gatePostRequest(path, first))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("replayed call status = %d, want 402; body = %s", rec.Code, rec.Body.String())
	}

	// A second authorization from the same payer, with a distinct nonce, is a
	// fresh (payer, nonce) pair and must pass.
	second := signedX402HeaderWithNonce(t, priv, verifyTestPayTo, "10000", "0x"+hex.EncodeToString(bytesOf(1)))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, gatePostRequest(path, second))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("fresh authorization status = %d, want 202 (new nonce must not be blocked); body = %s", rec.Code, rec.Body.String())
	}

	if _, total, err := invStore.List(context.Background(), verifyTestTenant, agentID, 1, 50); err != nil || total != 2 {
		t.Errorf("invocations created = %d, want 2 (err=%v)", total, err)
	}
}

// bytesOf returns a 32-byte slice filled with b, used to build a distinct
// nonce for signedX402HeaderWithNonce.
func bytesOf(b byte) []byte {
	buf := make([]byte, 32)
	for i := range buf {
		buf[i] = b
	}
	return buf
}

// signedX402HeaderWithNonce is signedX402Header with an explicit nonce, so a
// test can construct a second, distinct authorization from the same key.
func signedX402HeaderWithNonce(t *testing.T, priv *secp256k1.PrivateKey, payTo, amount, nonce string) string {
	t.Helper()
	now := time.Now().UTC().Unix()
	authz := x402types.Authorization{
		From:        ethAddressOf(t, priv),
		To:          payTo,
		Value:       amount,
		ValidAfter:  strconv.FormatInt(now-60, 10),
		ValidBefore: strconv.FormatInt(now+3600, 10),
		Nonce:       nonce,
	}
	pmt := &x402types.Payment{
		X402Version: x402types.Version,
		Scheme:      x402Scheme,
		Network:     pricingNetwork,
		Payload:     x402types.PaymentPayload{Signature: signAuthz(t, priv, authz), Authorization: authz},
	}
	return encodePayment(t, pmt)
}
