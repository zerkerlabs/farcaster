package chain

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
)

func TestLocalSigner_SignHashRecoversToAddress(t *testing.T) {
	key, err := ethcrypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer := NewLocalSigner(key)

	if got, want := signer.Address(), ethcrypto.PubkeyToAddress(key.PublicKey); got != want {
		t.Fatalf("Address() = %s, want %s", got, want)
	}

	hash := ethcrypto.Keccak256([]byte("a 32-byte digest to sign"))
	sig, err := signer.SignHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("SignHash: %v", err)
	}
	assertRecoverable(t, hash, sig, signer.Address().Hex())
}

func TestLocalSigner_RejectsNon32ByteHash(t *testing.T) {
	key, _ := ethcrypto.GenerateKey()
	signer := NewLocalSigner(key)
	if _, err := signer.SignHash(context.Background(), []byte("too short")); !errors.Is(err, ErrSignHashLength) {
		t.Fatalf("err = %v, want ErrSignHashLength", err)
	}
}

func TestNewLocalSignerFromKeystore_RoundTrip(t *testing.T) {
	key, _ := ethcrypto.GenerateKey()
	ksKey := &keystore.Key{
		Address:    ethcrypto.PubkeyToAddress(key.PublicKey),
		PrivateKey: key,
	}
	const passphrase = "correct horse battery staple"
	// LightScryptN/P keep the test fast; production keystores use stronger params.
	keyJSON, err := keystore.EncryptKey(ksKey, passphrase, keystore.LightScryptN, keystore.LightScryptP)
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}

	signer, err := NewLocalSignerFromKeystore(keyJSON, passphrase)
	if err != nil {
		t.Fatalf("NewLocalSignerFromKeystore: %v", err)
	}
	if got, want := signer.Address(), ethcrypto.PubkeyToAddress(key.PublicKey); got != want {
		t.Fatalf("Address() = %s, want %s", got, want)
	}

	hash := ethcrypto.Keccak256([]byte("digest"))
	sig, err := signer.SignHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("SignHash: %v", err)
	}
	assertRecoverable(t, hash, sig, signer.Address().Hex())
}

func TestNewLocalSignerFromKeystore_WrongPassphrase(t *testing.T) {
	key, _ := ethcrypto.GenerateKey()
	ksKey := &keystore.Key{Address: ethcrypto.PubkeyToAddress(key.PublicKey), PrivateKey: key}
	keyJSON, _ := keystore.EncryptKey(ksKey, "right", keystore.LightScryptN, keystore.LightScryptP)

	if _, err := NewLocalSignerFromKeystore(keyJSON, "wrong"); err == nil {
		t.Fatal("expected error decrypting with wrong passphrase, got nil")
	}
}

// assertRecoverable checks that sig is a well-formed 65-byte Ethereum signature
// (v in {0,1}) that recovers to wantAddr over hash — the contract every Signer
// must meet.
func assertRecoverable(t *testing.T, hash, sig []byte, wantAddr string) {
	t.Helper()
	if len(sig) != 65 {
		t.Fatalf("signature length = %d, want 65", len(sig))
	}
	if v := sig[64]; v != 0 && v != 1 {
		t.Fatalf("recovery id v = %d, want 0 or 1", v)
	}
	// low-S: s must be in the lower half of the group order.
	s := new(big.Int).SetBytes(sig[32:64])
	if s.Cmp(secp256k1HalfN) > 0 {
		t.Fatalf("signature s is not canonical (high-S)")
	}
	pub, err := ethcrypto.SigToPub(hash, sig)
	if err != nil {
		t.Fatalf("SigToPub: %v", err)
	}
	if got := ethcrypto.PubkeyToAddress(*pub).Hex(); got != wantAddr {
		t.Fatalf("recovered address = %s, want %s", got, wantAddr)
	}
}
