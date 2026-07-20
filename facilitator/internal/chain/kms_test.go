package chain

import (
	"context"
	"crypto/ecdsa"
	"encoding/asn1"
	"errors"
	"math/big"
	"testing"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// fakeKMS emulates an AWS KMS ECC_SECG_P256K1 key: it holds a secp256k1 key,
// serves its DER SubjectPublicKeyInfo, and signs digests as DER (r, s) — never
// exposing the private key, exactly as KMS does. highS forces the non-canonical
// s form so the signer's low-S normalization is exercised.
type fakeKMS struct {
	key       *ecdsa.PrivateKey
	highS     bool
	getPubErr error
	signErr   error
}

var (
	oidECPublicKey = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	oidSECP256K1   = asn1.ObjectIdentifier{1, 3, 132, 0, 10}
)

func (f *fakeKMS) GetPublicKey(context.Context) ([]byte, error) {
	if f.getPubErr != nil {
		return nil, f.getPubErr
	}
	type algo struct {
		Algorithm  asn1.ObjectIdentifier
		Parameters asn1.ObjectIdentifier
	}
	type spki struct {
		Algorithm algo
		PublicKey asn1.BitString
	}
	point := ethcrypto.FromECDSAPub(&f.key.PublicKey) // 0x04 || X || Y
	der, err := asn1.Marshal(spki{
		Algorithm: algo{Algorithm: oidECPublicKey, Parameters: oidSECP256K1},
		PublicKey: asn1.BitString{Bytes: point, BitLength: len(point) * 8},
	})
	if err != nil {
		return nil, err
	}
	return der, nil
}

func (f *fakeKMS) Sign(_ context.Context, digest []byte) ([]byte, error) {
	if f.signErr != nil {
		return nil, f.signErr
	}
	// crypto.Sign yields a compact r||s||v with canonical low-S; take r, s.
	compact, err := ethcrypto.Sign(digest, f.key)
	if err != nil {
		return nil, err
	}
	r := new(big.Int).SetBytes(compact[0:32])
	s := new(big.Int).SetBytes(compact[32:64])
	if f.highS {
		s = new(big.Int).Sub(secp256k1N, s) // flip to the non-canonical half
	}
	return asn1.Marshal(struct{ R, S *big.Int }{r, s})
}

func TestKMSSigner_AddressFromPublicKey(t *testing.T) {
	key, _ := ethcrypto.GenerateKey()
	signer, err := NewKMSSigner(context.Background(), &fakeKMS{key: key})
	if err != nil {
		t.Fatalf("NewKMSSigner: %v", err)
	}
	if got, want := signer.Address(), ethcrypto.PubkeyToAddress(key.PublicKey); got != want {
		t.Fatalf("Address() = %s, want %s", got, want)
	}
}

func TestKMSSigner_SignHashRecoversToAddress(t *testing.T) {
	key, _ := ethcrypto.GenerateKey()
	signer, err := NewKMSSigner(context.Background(), &fakeKMS{key: key})
	if err != nil {
		t.Fatalf("NewKMSSigner: %v", err)
	}
	hash := ethcrypto.Keccak256([]byte("a digest for the kms key to sign"))
	sig, err := signer.SignHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("SignHash: %v", err)
	}
	assertRecoverable(t, hash, sig, signer.Address().Hex())
}

// A KMS-returned high-S signature must be normalized to low-S and still recover.
func TestKMSSigner_NormalizesHighS(t *testing.T) {
	key, _ := ethcrypto.GenerateKey()
	signer, err := NewKMSSigner(context.Background(), &fakeKMS{key: key, highS: true})
	if err != nil {
		t.Fatalf("NewKMSSigner: %v", err)
	}
	hash := ethcrypto.Keccak256([]byte("high-s digest"))
	sig, err := signer.SignHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("SignHash: %v", err)
	}
	assertRecoverable(t, hash, sig, signer.Address().Hex())
}

func TestKMSSigner_RejectsNon32ByteHash(t *testing.T) {
	key, _ := ethcrypto.GenerateKey()
	signer, _ := NewKMSSigner(context.Background(), &fakeKMS{key: key})
	if _, err := signer.SignHash(context.Background(), []byte("short")); !errors.Is(err, ErrSignHashLength) {
		t.Fatalf("err = %v, want ErrSignHashLength", err)
	}
}

func TestNewKMSSigner_GetPublicKeyError(t *testing.T) {
	sentinel := errors.New("kms unavailable")
	if _, err := NewKMSSigner(context.Background(), &fakeKMS{getPubErr: sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapping %v", err, sentinel)
	}
}

func TestKMSSigner_SignError(t *testing.T) {
	key, _ := ethcrypto.GenerateKey()
	sentinel := errors.New("kms throttled")
	signer, _ := NewKMSSigner(context.Background(), &fakeKMS{key: key})
	signer.api = &fakeKMS{key: key, signErr: sentinel}
	hash := ethcrypto.Keccak256([]byte("digest"))
	if _, err := signer.SignHash(context.Background(), hash); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapping %v", err, sentinel)
	}
}
