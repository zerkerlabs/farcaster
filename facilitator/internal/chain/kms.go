package chain

import (
	"context"
	"crypto/ecdsa"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// KMSAPI is the minimal AWS KMS surface [KMSSigner] needs, narrowed to stdlib
// types so the signer's cryptographic logic (DER parsing, low-S normalization,
// recovery-id search) is unit-testable against a fake and the real AWS SDK
// stays out of this package's imports — it lives only in the awskms adapter,
// which satisfies this interface. All KMS key material stays behind it: the
// interface returns signatures and the public key, never the private key.
type KMSAPI interface {
	// GetPublicKey returns the DER-encoded SubjectPublicKeyInfo (X.509 SPKI)
	// for the key — for an ECC_SECG_P256K1 key, a secp256k1 public point.
	GetPublicKey(ctx context.Context) (derSPKI []byte, err error)

	// Sign returns the DER-encoded ECDSA signature (ASN.1 SEQUENCE { r, s })
	// over the given 32-byte digest. The digest is signed as-is (KMS message
	// type DIGEST); no additional hashing happens inside KMS.
	Sign(ctx context.Context, digest []byte) (derSig []byte, err error)
}

// KMSSigner is the managed [Signer]: it signs through AWS KMS, where a
// secp256k1 (ECC_SECG_P256K1) gas key lives HSM-backed and never leaves the
// service boundary (spec 0007 Decision 2). KMS returns a DER ECDSA signature
// with no recovery id and no low-S guarantee, so this signer does the work
// Ethereum needs on top of raw KMS: canonicalize s to low-S (EIP-2) and search
// the two candidate recovery ids for the one that recovers the key's own
// address.
type KMSSigner struct {
	api  KMSAPI
	pub  *ecdsa.PublicKey
	addr common.Address
}

// secp256k1HalfN is N/2 for the secp256k1 group order; a signature's s value
// above it is non-canonical and is folded to N-s (EIP-2).
var (
	secp256k1N     = ethcrypto.S256().Params().N
	secp256k1HalfN = new(big.Int).Rsh(secp256k1N, 1)
)

// NewKMSSigner fetches the KMS key's public key once, derives the gas-wallet
// address from it, and returns a signer bound to that key. The public key is
// cached so every subsequent sign only makes the single KMS Sign call.
func NewKMSSigner(ctx context.Context, api KMSAPI) (*KMSSigner, error) {
	der, err := api.GetPublicKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("chain: kms get public key: %w", err)
	}
	pub, err := parseSECP256K1PublicKey(der)
	if err != nil {
		return nil, err
	}
	return &KMSSigner{
		api:  api,
		pub:  pub,
		addr: ethcrypto.PubkeyToAddress(*pub),
	}, nil
}

// Address returns the gas-wallet address derived from the KMS public key.
func (s *KMSSigner) Address() common.Address { return s.addr }

// SignHash signs a 32-byte hash via KMS and returns the 65-byte Ethereum
// signature r||s||v with canonical low-S and the recovery id in {0, 1}. The
// private key never leaves KMS; only the DER signature crosses the boundary.
func (s *KMSSigner) SignHash(ctx context.Context, hash []byte) ([]byte, error) {
	if len(hash) != 32 {
		return nil, ErrSignHashLength
	}
	der, err := s.api.Sign(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("chain: kms sign: %w", err)
	}
	r, sVal, err := parseDERSignature(der)
	if err != nil {
		return nil, err
	}
	// EIP-2: normalize to the low-S form KMS does not guarantee.
	if sVal.Cmp(secp256k1HalfN) > 0 {
		sVal = new(big.Int).Sub(secp256k1N, sVal)
	}

	sig := make([]byte, 65)
	r.FillBytes(sig[0:32])
	sVal.FillBytes(sig[32:64])

	// KMS gives no recovery id; find the one that recovers this key's address.
	for v := byte(0); v < 2; v++ {
		sig[64] = v
		recovered, err := ethcrypto.SigToPub(hash, sig)
		if err != nil {
			continue
		}
		if ethcrypto.PubkeyToAddress(*recovered) == s.addr {
			return sig, nil
		}
	}
	return nil, errors.New("chain: kms signature recovery id not found")
}

// parseSECP256K1PublicKey extracts the uncompressed secp256k1 point from a DER
// SubjectPublicKeyInfo. Go's crypto/x509 rejects secp256k1 (it is not a NIST
// curve), so the SPKI is parsed structurally: the BIT STRING holds the
// 65-byte 0x04||X||Y point that go-ethereum can unmarshal.
func parseSECP256K1PublicKey(der []byte) (*ecdsa.PublicKey, error) {
	var spki struct {
		Algorithm        asn1.RawValue
		SubjectPublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(der, &spki); err != nil {
		return nil, fmt.Errorf("chain: parse kms public key: %w", err)
	}
	pub, err := ethcrypto.UnmarshalPubkey(spki.SubjectPublicKey.Bytes)
	if err != nil {
		return nil, fmt.Errorf("chain: unmarshal secp256k1 public key: %w", err)
	}
	return pub, nil
}

// parseDERSignature decodes an ASN.1 DER ECDSA signature (SEQUENCE { r, s })
// into its two integers, rejecting non-positive components.
func parseDERSignature(der []byte) (r, s *big.Int, err error) {
	var sig struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &sig); err != nil {
		return nil, nil, fmt.Errorf("chain: parse kms signature: %w", err)
	}
	if sig.R == nil || sig.S == nil || sig.R.Sign() <= 0 || sig.S.Sign() <= 0 {
		return nil, nil, errors.New("chain: kms signature has non-positive component")
	}
	return sig.R, sig.S, nil
}
