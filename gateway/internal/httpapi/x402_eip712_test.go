package httpapi

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"github.com/zerkerlabs/farcaster/x402types"
)

// ethAddressOf derives the Ethereum address (0x + 40 hex) for a secp256k1 key,
// mirroring recoverEthAddress: keccak256(uncompressed pubkey sans 0x04)[12:].
func ethAddressOf(t *testing.T, priv *secp256k1.PrivateKey) string {
	t.Helper()
	addr := keccak256(priv.PubKey().SerializeUncompressed()[1:])[12:]
	return "0x" + hex.EncodeToString(addr)
}

// signAuthz signs the EIP-3009 digest for authz with priv and returns the
// Ethereum-format signature r||s||v (v = 27/28) as a 0x-hex string.
func signAuthz(t *testing.T, priv *secp256k1.PrivateKey, authz x402types.Authorization) string {
	t.Helper()
	digest, err := eip3009Digest(authz)
	if err != nil {
		t.Fatalf("eip3009Digest: %v", err)
	}
	// SignCompact returns [27+recID][R][S]; convert to Ethereum r||s||v.
	compact := ecdsa.SignCompact(priv, digest, false)
	sig := make([]byte, 65)
	copy(sig[0:32], compact[1:33])   // R
	copy(sig[32:64], compact[33:65]) // S
	sig[64] = compact[0]             // v (27/28)
	return "0x" + hex.EncodeToString(sig)
}

// validAuthz returns a well-formed authorization paying `to`, signed by a fresh
// key; it returns the key so tests can tamper or mis-sign.
func validAuthz(t *testing.T) (*secp256k1.PrivateKey, x402types.Authorization) {
	t.Helper()
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	authz := x402types.Authorization{
		From:        ethAddressOf(t, priv),
		To:          testPayTo,
		Value:       "10000",
		ValidAfter:  "0",
		ValidBefore: "9999999999",
		Nonce:       "0x" + hex.EncodeToString(make([]byte, 32)),
	}
	return priv, authz
}

const testPayTo = "0x00000000000000000000000000000000000000ff"

func TestBaseUSDCVerifier_VerifySignature(t *testing.T) {
	t.Parallel()

	req := x402types.PaymentRequirements{Scheme: x402Scheme, Network: pricingNetwork, PayTo: testPayTo}

	t.Run("valid signature verifies", func(t *testing.T) {
		t.Parallel()
		priv, authz := validAuthz(t)
		sig := signAuthz(t, priv, authz)
		pmt := &x402types.Payment{Payload: x402types.PaymentPayload{Signature: sig, Authorization: authz}}
		if err := (baseUSDCVerifier{}).VerifySignature(pmt, req); err != nil {
			t.Errorf("VerifySignature = %v, want nil", err)
		}
	})

	t.Run("wrong signer is rejected", func(t *testing.T) {
		t.Parallel()
		_, authz := validAuthz(t)
		other, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			t.Fatalf("GeneratePrivateKey: %v", err)
		}
		// authz.From is the first key's address, but a DIFFERENT key signs.
		sig := signAuthz(t, other, authz)
		pmt := &x402types.Payment{Payload: x402types.PaymentPayload{Signature: sig, Authorization: authz}}
		if err := (baseUSDCVerifier{}).VerifySignature(pmt, req); !errors.Is(err, errSigMismatch) {
			t.Errorf("VerifySignature = %v, want errSigMismatch", err)
		}
	})

	t.Run("tampered amount is rejected", func(t *testing.T) {
		t.Parallel()
		priv, authz := validAuthz(t)
		sig := signAuthz(t, priv, authz)
		// Raise the value AFTER signing: the recomputed digest no longer matches
		// the signature, so a different address is recovered.
		authz.Value = "999999999"
		pmt := &x402types.Payment{Payload: x402types.PaymentPayload{Signature: sig, Authorization: authz}}
		if err := (baseUSDCVerifier{}).VerifySignature(pmt, req); !errors.Is(err, errSigMismatch) {
			t.Errorf("VerifySignature = %v, want errSigMismatch", err)
		}
	})

	t.Run("malformed signature is rejected", func(t *testing.T) {
		t.Parallel()
		_, authz := validAuthz(t)
		pmt := &x402types.Payment{Payload: x402types.PaymentPayload{Signature: "0xdeadbeef", Authorization: authz}}
		if err := (baseUSDCVerifier{}).VerifySignature(pmt, req); !errors.Is(err, errSigFormat) {
			t.Errorf("VerifySignature = %v, want errSigFormat", err)
		}
	})

	t.Run("malformed authorization field is rejected", func(t *testing.T) {
		t.Parallel()
		priv, authz := validAuthz(t)
		sig := signAuthz(t, priv, authz)
		authz.From = "not-an-address"
		pmt := &x402types.Payment{Payload: x402types.PaymentPayload{Signature: sig, Authorization: authz}}
		if err := (baseUSDCVerifier{}).VerifySignature(pmt, req); !errors.Is(err, errAuthzField) {
			t.Errorf("VerifySignature = %v, want errAuthzField", err)
		}
	})
}

// TestEIP3009Digest_Deterministic guards against accidental changes to the
// domain/type encoding: the digest for a fixed authorization must not drift.
func TestEIP3009Digest_Deterministic(t *testing.T) {
	t.Parallel()
	authz := x402types.Authorization{
		From:        "0x1111111111111111111111111111111111111111",
		To:          testPayTo,
		Value:       "10000",
		ValidAfter:  "0",
		ValidBefore: "9999999999",
		Nonce:       "0x" + hex.EncodeToString(make([]byte, 32)),
	}
	d1, err := eip3009Digest(authz)
	if err != nil {
		t.Fatalf("eip3009Digest: %v", err)
	}
	d2, _ := eip3009Digest(authz)
	if len(d1) != 32 {
		t.Errorf("digest length = %d, want 32", len(d1))
	}
	if hex.EncodeToString(d1) != hex.EncodeToString(d2) {
		t.Error("digest is not deterministic")
	}
}
