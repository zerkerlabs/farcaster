package verify

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"github.com/zerkerlabs/farcaster/x402types"
)

// testKind is a fixture (network, asset) — values are arbitrary for a unit
// test; only the base facilitator/gateway wiring cares whether they match the
// real deployed USDC contract.
var testKind = Kind{
	Network:      "base",
	Asset:        "0x1111111111111111111111111111111111111111",
	ChainID:      8453,
	AssetName:    "USD Coin",
	AssetVersion: "2",
}

// testPayTo carries mixed-case hex letters (like a real EIP-55 checksummed
// address) so tests can exercise case-insensitive comparison meaningfully.
const testPayTo = "0xAbCdEf1234567890AbCdEf1234567890AbCdEf12"

// ethAddressOf derives the Ethereum address (0x + 40 hex) for a secp256k1
// key, mirroring recoverEthAddress: keccak256(uncompressed pubkey sans
// 0x04)[12:].
func ethAddressOf(t *testing.T, priv *secp256k1.PrivateKey) string {
	t.Helper()
	addr := keccak256(priv.PubKey().SerializeUncompressed()[1:])[12:]
	return "0x" + hex.EncodeToString(addr)
}

// signAuthz signs the EIP-3009 digest for authz (over kind's domain) with
// priv and returns the Ethereum-format signature r||s||v (v = 27/28) as a
// 0x-hex string.
func signAuthz(t *testing.T, priv *secp256k1.PrivateKey, authz x402types.Authorization, kind Kind) string {
	t.Helper()
	digest, err := eip3009Digest(authz, kind)
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

// validAuthz returns a well-formed authorization paying testPayTo, signed by
// a fresh key; it returns the key so tests can tamper or mis-sign.
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

func TestVerifySignature(t *testing.T) {
	t.Parallel()

	t.Run("valid signature verifies", func(t *testing.T) {
		t.Parallel()
		priv, authz := validAuthz(t)
		sig := signAuthz(t, priv, authz, testKind)
		if err := verifySignature(authz, sig, testKind); err != nil {
			t.Errorf("verifySignature = %v, want nil", err)
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
		sig := signAuthz(t, other, authz, testKind)
		if err := verifySignature(authz, sig, testKind); !errors.Is(err, ErrSignatureMismatch) {
			t.Errorf("verifySignature = %v, want ErrSignatureMismatch", err)
		}
	})

	t.Run("tampered amount is rejected", func(t *testing.T) {
		t.Parallel()
		priv, authz := validAuthz(t)
		sig := signAuthz(t, priv, authz, testKind)
		// Raise the value AFTER signing: the recomputed digest no longer
		// matches the signature, so a different address is recovered.
		authz.Value = "999999999"
		if err := verifySignature(authz, sig, testKind); !errors.Is(err, ErrSignatureMismatch) {
			t.Errorf("verifySignature = %v, want ErrSignatureMismatch", err)
		}
	})

	t.Run("wrong chain ID is rejected", func(t *testing.T) {
		t.Parallel()
		priv, authz := validAuthz(t)
		sig := signAuthz(t, priv, authz, testKind)
		// A signature valid over one chain/asset domain must not verify over
		// another — proves the domain separator is actually bound in.
		otherKind := testKind
		otherKind.ChainID = 84532
		if err := verifySignature(authz, sig, otherKind); !errors.Is(err, ErrSignatureMismatch) {
			t.Errorf("verifySignature = %v, want ErrSignatureMismatch", err)
		}
	})

	t.Run("malformed signature is rejected", func(t *testing.T) {
		t.Parallel()
		_, authz := validAuthz(t)
		if err := verifySignature(authz, "0xdeadbeef", testKind); !errors.Is(err, ErrSignatureFormat) {
			t.Errorf("verifySignature = %v, want ErrSignatureFormat", err)
		}
	})

	t.Run("signature that fails curve recovery is rejected", func(t *testing.T) {
		t.Parallel()
		_, authz := validAuthz(t)
		// 65 bytes with a valid length and v byte (27), but S = 0 — a
		// syntactically well-formed signature that RecoverCompact rejects as
		// cryptographically invalid before any address is recovered.
		sig := make([]byte, 65)
		sig[31] = 1 // R = 1
		sig[64] = 27
		hexSig := "0x" + hex.EncodeToString(sig)
		if err := verifySignature(authz, hexSig, testKind); !errors.Is(err, ErrSignatureRecovery) {
			t.Errorf("verifySignature = %v, want ErrSignatureRecovery", err)
		}
	})

	t.Run("malformed authorization field is rejected", func(t *testing.T) {
		t.Parallel()
		priv, authz := validAuthz(t)
		sig := signAuthz(t, priv, authz, testKind)
		authz.From = "not-an-address"
		if err := verifySignature(authz, sig, testKind); !errors.Is(err, ErrMalformedAuthorization) {
			t.Errorf("verifySignature = %v, want ErrMalformedAuthorization", err)
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
	d1, err := eip3009Digest(authz, testKind)
	if err != nil {
		t.Fatalf("eip3009Digest: %v", err)
	}
	d2, _ := eip3009Digest(authz, testKind)
	if len(d1) != 32 {
		t.Errorf("digest length = %d, want 32", len(d1))
	}
	if hex.EncodeToString(d1) != hex.EncodeToString(d2) {
		t.Error("digest is not deterministic")
	}
}
