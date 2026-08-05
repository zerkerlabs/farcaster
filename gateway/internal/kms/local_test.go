package kms_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/kms"
)

func TestLocalProvider_WrapUnwrapRoundtrip(t *testing.T) {
	t.Parallel()

	p, err := kms.NewLocalProvider()
	if err != nil {
		t.Fatalf("NewLocalProvider: %v", err)
	}

	plaintext := []byte("kek-material-32-bytes-xxxxxxxxxxx")
	ct, version, err := p.WrapKey(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("WrapKey: %v", err)
	}
	if version == "" {
		t.Error("WrapKey returned empty version")
	}
	if bytes.Equal(ct, plaintext) {
		t.Error("WrapKey returned plaintext unchanged (no encryption)")
	}

	got, err := p.UnwrapKey(context.Background(), ct, version)
	if err != nil {
		t.Fatalf("UnwrapKey: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("UnwrapKey = %q, want %q", got, plaintext)
	}
}

func TestLocalProvider_UniqueVersionPerCall(t *testing.T) {
	t.Parallel()

	p, err := kms.NewLocalProvider()
	if err != nil {
		t.Fatalf("NewLocalProvider: %v", err)
	}

	plaintext := []byte("key")
	_, v1, err := p.WrapKey(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("WrapKey 1: %v", err)
	}
	_, v2, err := p.WrapKey(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("WrapKey 2: %v", err)
	}
	if v1 == v2 {
		t.Errorf("WrapKey returned the same version %q on two calls; versions must be unique for KEK rotation", v1)
	}
}

func TestLocalProvider_TamperedCiphertextFails(t *testing.T) {
	t.Parallel()

	p, err := kms.NewLocalProvider()
	if err != nil {
		t.Fatalf("NewLocalProvider: %v", err)
	}

	ct, version, err := p.WrapKey(context.Background(), []byte("original"))
	if err != nil {
		t.Fatalf("WrapKey: %v", err)
	}
	ct[len(ct)-1] ^= 0xFF // corrupt the GCM authentication tag

	if _, err := p.UnwrapKey(context.Background(), ct, version); err == nil {
		t.Error("UnwrapKey with tampered ciphertext should fail, got nil error")
	}
}

func TestLocalProvider_DifferentPlaintextsProduceDifferentCiphertexts(t *testing.T) {
	t.Parallel()

	p, err := kms.NewLocalProvider()
	if err != nil {
		t.Fatalf("NewLocalProvider: %v", err)
	}

	ct1, _, err := p.WrapKey(context.Background(), []byte("key-one"))
	if err != nil {
		t.Fatalf("WrapKey 1: %v", err)
	}
	ct2, _, err := p.WrapKey(context.Background(), []byte("key-two"))
	if err != nil {
		t.Fatalf("WrapKey 2: %v", err)
	}
	if bytes.Equal(ct1, ct2) {
		t.Error("different plaintexts produced identical ciphertexts")
	}
}
