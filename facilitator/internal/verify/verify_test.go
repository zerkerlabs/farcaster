package verify

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zerkerlabs/gateway/x402types"
)

// refTime sits inside [validAfter, validBefore) of validSettleRequest.
var refTime = time.Unix(1700000300, 0).UTC()

var testPolicy = Policy{Kinds: []Kind{testKind}}

// validSettleRequest returns a settle request that passes Reverify against
// testPolicy at refTime; individual test cases mutate one field to exercise a
// single rejection.
func validSettleRequest(t *testing.T) x402types.SettleRequest {
	t.Helper()
	priv, authz := validAuthz(t)
	authz.ValidAfter = "1700000000"
	authz.ValidBefore = "1700000600"
	sig := signAuthz(t, priv, authz, testKind)

	return x402types.SettleRequest{
		X402Version: x402types.Version,
		PaymentPayload: x402types.Payment{
			X402Version: x402types.Version,
			Scheme:      "exact",
			Network:     testKind.Network,
			Payload: x402types.PaymentPayload{
				Signature:     sig,
				Authorization: authz,
			},
		},
		PaymentRequirements: x402types.PaymentRequirements{
			Scheme:            "exact",
			Network:           testKind.Network,
			MaxAmountRequired: "10000",
			Asset:             testKind.Asset,
			PayTo:             testPayTo,
			Resource:          "https://example.com/resource",
			MaxTimeoutSeconds: 60,
		},
	}
}

func TestReverify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*x402types.SettleRequest)
		wantErr error // nil = accept
	}{
		{
			name:    "valid request is accepted",
			mutate:  func(*x402types.SettleRequest) {},
			wantErr: nil,
		},
		{
			name: "recipient case-insensitive",
			mutate: func(r *x402types.SettleRequest) {
				// A different case rendering of the same address decodes to
				// identical bytes, so both the recipient check and the
				// signature (computed over the decoded bytes) still pass.
				r.PaymentPayload.Payload.Authorization.To = strings.ToLower(testPayTo)
			},
			wantErr: nil,
		},
		{
			name: "value below max accepts",
			mutate: func(r *x402types.SettleRequest) {
				// A lower value still passes signature recovery since it was
				// signed with this exact value.
				priv, authz := validAuthz(t)
				authz.To = testPayTo
				authz.Value = "5000"
				authz.ValidAfter = "1700000000"
				authz.ValidBefore = "1700000600"
				sig := signAuthz(t, priv, authz, testKind)
				r.PaymentPayload.Payload.Authorization = authz
				r.PaymentPayload.Payload.Signature = sig
			},
			wantErr: nil,
		},
		{
			name: "network/asset not in policy",
			mutate: func(r *x402types.SettleRequest) {
				r.PaymentRequirements.Network = "solana"
			},
			wantErr: ErrUnsupportedKind,
		},
		{
			name: "asset not in policy",
			mutate: func(r *x402types.SettleRequest) {
				r.PaymentRequirements.Asset = "0x9999999999999999999999999999999999999999"
			},
			wantErr: ErrUnsupportedKind,
		},
		{
			name: "wrong recipient",
			mutate: func(r *x402types.SettleRequest) {
				r.PaymentPayload.Payload.Authorization.To = "0x9999999999999999999999999999999999999999"
			},
			wantErr: ErrRecipientMismatch,
		},
		{
			name: "value exceeds max",
			mutate: func(r *x402types.SettleRequest) {
				r.PaymentRequirements.MaxAmountRequired = "1"
			},
			wantErr: ErrAmountExceedsMax,
		},
		{
			name: "non-integer value",
			mutate: func(r *x402types.SettleRequest) {
				r.PaymentPayload.Payload.Authorization.Value = "10000.5"
			},
			wantErr: ErrAmountFormat,
		},
		{
			name: "non-integer maxAmountRequired",
			mutate: func(r *x402types.SettleRequest) {
				r.PaymentRequirements.MaxAmountRequired = "abc"
			},
			wantErr: ErrAmountFormat,
		},
		{
			name: "negative value rejected on its own terms",
			mutate: func(r *x402types.SettleRequest) {
				// A negative value parses fine as a big.Int and is always <=
				// a non-negative maxAmountRequired, so without an explicit
				// sign check this would slip past the amount-bound check.
				r.PaymentPayload.Payload.Authorization.Value = "-1"
			},
			wantErr: ErrAmountFormat,
		},
		{
			name: "non-numeric validity",
			mutate: func(r *x402types.SettleRequest) {
				r.PaymentPayload.Payload.Authorization.ValidBefore = "soon"
			},
			wantErr: ErrValidityFormat,
		},
		{
			name: "not yet valid",
			mutate: func(r *x402types.SettleRequest) {
				r.PaymentPayload.Payload.Authorization.ValidAfter = strconv.FormatInt(refTime.Unix()+1, 10)
			},
			wantErr: ErrNotYetValid,
		},
		{
			name: "expired",
			mutate: func(r *x402types.SettleRequest) {
				r.PaymentPayload.Payload.Authorization.ValidBefore = strconv.FormatInt(refTime.Unix(), 10)
			},
			wantErr: ErrExpired,
		},
		{
			name: "signature does not recover to from",
			mutate: func(r *x402types.SettleRequest) {
				r.PaymentPayload.Payload.Authorization.Value = "9999" // breaks the signed digest
			},
			wantErr: ErrSignatureMismatch,
		},
		{
			name: "malformed signature",
			mutate: func(r *x402types.SettleRequest) {
				r.PaymentPayload.Payload.Signature = "0xdeadbeef"
			},
			wantErr: ErrSignatureFormat,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := validSettleRequest(t)
			tt.mutate(&req)
			err := Reverify(req, testPolicy, refTime)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Reverify err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestReverify_NoSideEffects documents (and, via -race, partially exercises)
// that Reverify is a pure function: calling it repeatedly with the same
// input, including a rejecting one, is deterministic and never mutates the
// request.
func TestReverify_NoSideEffects(t *testing.T) {
	t.Parallel()
	req := validSettleRequest(t)
	req.PaymentRequirements.Network = "solana" // force a rejection

	before := req
	for range 3 {
		err := Reverify(req, testPolicy, refTime)
		if !errors.Is(err, ErrUnsupportedKind) {
			t.Fatalf("Reverify err = %v, want ErrUnsupportedKind", err)
		}
	}
	if req != before {
		t.Error("Reverify mutated its input request")
	}
}
