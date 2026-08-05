package chain

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/zerkerlabs/gateway/x402types"
)

func sampleAuthorization() x402types.Authorization {
	return x402types.Authorization{
		From:        "0x1111111111111111111111111111111111111111",
		To:          "0x2222222222222222222222222222222222222222",
		Value:       "10000",
		ValidAfter:  "0",
		ValidBefore: "1793000000",
		Nonce:       "0x" + strings.Repeat("ab", 32),
	}
}

// a 65-byte payer signature with recovery byte v=1 (the {0,1} form).
func sampleSignature(v byte) string {
	raw := make([]byte, 65)
	for i := range raw[:64] {
		raw[i] = byte(i + 1)
	}
	raw[64] = v
	return "0x" + hex.EncodeToString(raw)
}

func TestEncodeTransferWithAuthorization_Layout(t *testing.T) {
	authz := sampleAuthorization()
	calldata, err := encodeTransferWithAuthorization(authz, sampleSignature(1))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	if got, want := len(calldata), 4+9*32; got != want {
		t.Fatalf("calldata length = %d, want %d", got, want)
	}

	wantSelector := ethcrypto.Keccak256([]byte(
		"transferWithAuthorization(address,address,uint256,uint256,uint256,bytes32,uint8,bytes32,bytes32)",
	))[:4]
	if !bytes.Equal(calldata[:4], wantSelector) {
		t.Fatalf("selector = %x, want %x", calldata[:4], wantSelector)
	}

	word := func(i int) []byte { return calldata[4+i*32 : 4+(i+1)*32] }

	// from occupies the low 20 bytes of word 0.
	if got := "0x" + hex.EncodeToString(word(0)[12:]); !strings.EqualFold(got, authz.From) {
		t.Fatalf("from word = %s, want %s", got, authz.From)
	}
	// value (word 2) = 10000 encoded big-endian.
	if got := new(big.Int).SetBytes(word(2)).String(); got != "10000" {
		t.Fatalf("value word = %s, want 10000", got)
	}
	// v (word 6) normalized 1 -> 28 in the last byte.
	if got := word(6)[31]; got != 28 {
		t.Fatalf("v byte = %d, want 28", got)
	}
	// r (word 7) and s (word 8) are the raw signature halves.
	sigRaw, _ := hex.DecodeString(strings.TrimPrefix(sampleSignature(1), "0x"))
	if !bytes.Equal(word(7), sigRaw[0:32]) {
		t.Fatalf("r word mismatch")
	}
	if !bytes.Equal(word(8), sigRaw[32:64]) {
		t.Fatalf("s word mismatch")
	}
}

// v already in the 27/28 convention is passed through unchanged.
func TestEncodeTransferWithAuthorization_VPassthrough(t *testing.T) {
	calldata, err := encodeTransferWithAuthorization(sampleAuthorization(), sampleSignature(27))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := calldata[4+6*32+31]; got != 27 {
		t.Fatalf("v byte = %d, want 27", got)
	}
}

func TestEncodeTransferWithAuthorization_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(a *x402types.Authorization)
		sig     string
		wantErr error
	}{
		{"bad from", func(a *x402types.Authorization) { a.From = "0xnothex" }, sampleSignature(1), ErrMalformedAuthorization},
		{"short address", func(a *x402types.Authorization) { a.To = "0x1234" }, sampleSignature(1), ErrMalformedAuthorization},
		{"negative value", func(a *x402types.Authorization) { a.Value = "-1" }, sampleSignature(1), ErrMalformedAuthorization},
		{"non-integer value", func(a *x402types.Authorization) { a.Value = "1.5" }, sampleSignature(1), ErrMalformedAuthorization},
		{"bad nonce length", func(a *x402types.Authorization) { a.Nonce = "0xabcd" }, sampleSignature(1), ErrMalformedAuthorization},
		{"short signature", func(_ *x402types.Authorization) {}, "0xabcd", ErrMalformedSignature},
		{"bad v", func(_ *x402types.Authorization) {}, sampleSignature(9), ErrMalformedSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authz := sampleAuthorization()
			tt.mutate(&authz)
			if _, err := encodeTransferWithAuthorization(authz, tt.sig); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
