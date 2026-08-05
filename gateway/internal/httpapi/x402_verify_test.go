package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
	"github.com/zerkerlabs/farcaster/gateway/internal/proxy"
	"github.com/zerkerlabs/farcaster/x402types"
)

// These are white-box tests (package httpapi): PaymentVerifier's method
// signature names unexported types (x402types.Payment / x402types.PaymentRequirements), so
// only this package can construct payment values or implement a verifier.

const (
	verifyTestTenant = "tnt_verify"
	verifyTestUser   = "usr_verify"
	verifyTestPayTo  = "0x1111111111111111111111111111111111111111"
)

// samplePayment returns a payment that passes checkPaymentParams against
// sampleReq at refTime; individual tests mutate one field to exercise a failure.
func samplePayment() *x402types.Payment {
	return &x402types.Payment{
		X402Version: 1,
		Scheme:      "exact",
		Network:     "base",
		Payload: x402types.PaymentPayload{
			Signature: "0xdeadbeef",
			Authorization: x402types.Authorization{
				From:        "0x2222222222222222222222222222222222222222",
				To:          verifyTestPayTo,
				Value:       "10000",
				ValidAfter:  "1700000000",
				ValidBefore: "1700000600",
				Nonce:       "0x00",
			},
		},
	}
}

func sampleReq() x402types.PaymentRequirements {
	return x402types.PaymentRequirements{
		Scheme:            "exact",
		Network:           "base",
		MaxAmountRequired: "10000",
		Asset:             x402USDCBaseAsset,
		// Mixed case on purpose: the recipient check must be case-insensitive.
		PayTo:             "0x1111111111111111111111111111111111111111",
		Resource:          "/v1/proxy/agt_x",
		MaxTimeoutSeconds: x402MaxTimeoutSeconds,
	}
}

// refTime sits inside [validAfter, validBefore) of samplePayment.
var refTime = time.Unix(1700000300, 0).UTC()

func encodePayment(t *testing.T, pmt *x402types.Payment) string {
	t.Helper()
	raw, err := json.Marshal(pmt)
	if err != nil {
		t.Fatalf("marshal payment: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestParseX402Payment(t *testing.T) {
	t.Parallel()

	t.Run("good", func(t *testing.T) {
		t.Parallel()
		want := samplePayment()
		got, err := parseX402Payment(encodePayment(t, want))
		if err != nil {
			t.Fatalf("parseX402Payment: unexpected error %v", err)
		}
		if got.Scheme != want.Scheme || got.Network != want.Network ||
			got.Payload.Authorization.To != want.Payload.Authorization.To ||
			got.Payload.Authorization.Value != want.Payload.Authorization.Value ||
			got.Payload.Signature != want.Payload.Signature {
			t.Errorf("parsed payment = %+v, want %+v", got, want)
		}
	})

	t.Run("bad base64", func(t *testing.T) {
		t.Parallel()
		_, err := parseX402Payment("!!! not base64 !!!")
		if !errors.Is(err, errPaymentDecode) {
			t.Errorf("err = %v, want errPaymentDecode", err)
		}
	})

	t.Run("bad json", func(t *testing.T) {
		t.Parallel()
		_, err := parseX402Payment(base64.StdEncoding.EncodeToString([]byte("{not json")))
		if !errors.Is(err, errPaymentDecode) {
			t.Errorf("err = %v, want errPaymentDecode", err)
		}
	})
}

func TestCheckPaymentParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*x402types.Payment)
		wantErr error // nil = accept
	}{
		{
			name:    "accept",
			mutate:  func(*x402types.Payment) {},
			wantErr: nil,
		},
		{
			name:    "recipient case-insensitive",
			mutate:  func(p *x402types.Payment) { p.Payload.Authorization.To = "0X1111111111111111111111111111111111111111" },
			wantErr: nil,
		},
		{
			name:    "value above required accepts",
			mutate:  func(p *x402types.Payment) { p.Payload.Authorization.Value = "20000" },
			wantErr: nil,
		},
		{
			name:    "wrong scheme",
			mutate:  func(p *x402types.Payment) { p.Scheme = "upto" },
			wantErr: errPaymentScheme,
		},
		{
			name:    "wrong network",
			mutate:  func(p *x402types.Payment) { p.Network = "solana" },
			wantErr: errPaymentNetwork,
		},
		{
			name:    "wrong recipient",
			mutate:  func(p *x402types.Payment) { p.Payload.Authorization.To = "0x9999999999999999999999999999999999999999" },
			wantErr: errPaymentRecipient,
		},
		{
			name:    "insufficient value",
			mutate:  func(p *x402types.Payment) { p.Payload.Authorization.Value = "9999" },
			wantErr: errPaymentAmount,
		},
		{
			name:    "non-integer value",
			mutate:  func(p *x402types.Payment) { p.Payload.Authorization.Value = "10000.5" },
			wantErr: errPaymentAmountFormat,
		},
		{
			name:    "non-numeric validity",
			mutate:  func(p *x402types.Payment) { p.Payload.Authorization.ValidBefore = "soon" },
			wantErr: errPaymentValidityFormat,
		},
		{
			name: "not yet valid",
			mutate: func(p *x402types.Payment) {
				p.Payload.Authorization.ValidAfter = strconv.FormatInt(refTime.Unix()+1, 10)
			},
			wantErr: errPaymentNotYetValid,
		},
		{
			name: "expired",
			mutate: func(p *x402types.Payment) {
				p.Payload.Authorization.ValidBefore = strconv.FormatInt(refTime.Unix(), 10)
			},
			wantErr: errPaymentExpired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pmt := samplePayment()
			tt.mutate(pmt)
			err := checkPaymentParams(pmt, sampleReq(), refTime)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("checkPaymentParams err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// passThroughVerifier accepts every signature, exercising the forward path
// without a real EIP-712 signature (the default verifier is baseUSDCVerifier).
type passThroughVerifier struct{}

func (passThroughVerifier) VerifySignature(*x402types.Payment, x402types.PaymentRequirements) error {
	return nil
}

// verifyFwd is a minimal ProxyForwarder returning a canned result.
type verifyFwd struct{ status int }

func (f verifyFwd) result() *proxy.Result {
	return &proxy.Result{
		StatusCode: f.status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		LatencyMS:  1,
	}
}

func (f verifyFwd) Do(context.Context, string, string, string, *http.Request) (*proxy.Result, error) {
	return f.result(), nil
}

func (f verifyFwd) DoStream(context.Context, string, string, string, *http.Request) (*proxy.Result, error) {
	return f.result(), nil
}

// seedVerifyAgent creates an active priced agent for the verifier handler tests.
func seedVerifyAgent(t *testing.T, store agent.AgentStore, upstream string) string {
	t.Helper()
	a := &agent.Agent{
		Name:        "verify-agent",
		CreatedBy:   verifyTestUser,
		UpstreamURL: &upstream,
		Protocol:    "http",
		Pricing: &agent.Pricing{
			Amount:  "10000",
			Asset:   "USDC",
			Network: "base",
			PayTo:   verifyTestPayTo,
		},
	}
	if err := store.Create(context.Background(), verifyTestTenant, a); err != nil {
		t.Fatalf("seedVerifyAgent: %v", err)
	}
	return a.ID
}

// wellFormedHeader builds a base64 X-PAYMENT that passes the parameter checks
// for seedVerifyAgent (valid for the next hour, correct recipient/amount).
func wellFormedHeader(t *testing.T) string {
	t.Helper()
	pmt := samplePayment()
	now := time.Now().UTC().Unix()
	pmt.Payload.Authorization.ValidAfter = strconv.FormatInt(now-60, 10)
	pmt.Payload.Authorization.ValidBefore = strconv.FormatInt(now+3600, 10)
	return encodePayment(t, pmt)
}

func verifyRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-PAYMENT", wellFormedHeader(t))
	return req.WithContext(authtest.WithIdentity(req.Context(), verifyTestTenant, verifyTestUser))
}

func TestHandleTransact_X402_ForwardsWithInjectedVerifier(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/invoke")
	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invocation.NewMemoryStore()).
		WithPaymentVerifier(passThroughVerifier{})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (forwarded with permissive verifier); body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Zerker-Invocation-ID"); got == "" {
		t.Error("X-Zerker-Invocation-ID missing on forwarded call")
	}
}

func TestHandleStream_X402_ForwardsWithInjectedVerifier(t *testing.T) {
	t.Parallel()

	store := agent.NewMemoryStore()
	agentID := seedVerifyAgent(t, store, "https://upstream.example.com/stream")
	h := NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(verifyFwd{status: 200}, invocation.NewMemoryStore()).
		WithPaymentVerifier(passThroughVerifier{})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifyRequest(t, "/v1/proxy/"+agentID+"/stream"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (forwarded with permissive verifier); body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Zerker-Invocation-ID"); got == "" {
		t.Error("X-Zerker-Invocation-ID missing on forwarded call")
	}
}
