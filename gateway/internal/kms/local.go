package kms

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/zerkerlabs/farcaster/gateway/internal/cryptoutil"
)

// LocalProvider is a KMS Provider for local development and integration
// tests. It wraps and unwraps key material with AES-256-GCM using a 32-byte
// master key from the FARCASTER_KMS_KEY environment variable (hex-encoded,
// 64 characters). If FARCASTER_KMS_KEY is not set, a random ephemeral key is
// generated. The ephemeral key does not survive process restart, so
// LocalProvider is safe only for single-process tests — never run a
// production environment without setting FARCASTER_KMS_KEY.
type LocalProvider struct {
	masterKey []byte // 32 bytes, AES-256
}

// NewLocalProvider returns a LocalProvider. The master key is read from
// FARCASTER_KMS_KEY (64 hex chars = 32 bytes). When the variable is absent a
// random ephemeral key is generated and a warning is implied by the name.
func NewLocalProvider() (*LocalProvider, error) {
	if raw := os.Getenv("FARCASTER_KMS_KEY"); raw != "" {
		key, err := hex.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("kms: decode FARCASTER_KMS_KEY: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("kms: FARCASTER_KMS_KEY must be 32 bytes (64 hex chars), got %d bytes", len(key))
		}
		return &LocalProvider{masterKey: key}, nil
	}
	// Ephemeral key — safe only for in-process tests.
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("kms: generate ephemeral key: %w", err)
	}
	return &LocalProvider{masterKey: key}, nil
}

// WrapKey implements Provider. The returned version is "local:" followed by a
// random 8-byte hex suffix, making every call produce a unique version token.
// This ensures that KEK rotation can store multiple versions in the KEK store
// without primary-key collisions.
func (p *LocalProvider) WrapKey(_ context.Context, plaintext []byte) ([]byte, string, error) {
	suffix := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, suffix); err != nil {
		return nil, "", fmt.Errorf("kms local: generate version suffix: %w", err)
	}
	version := "local:" + hex.EncodeToString(suffix)

	ct, err := cryptoutil.SealGCM(p.masterKey, plaintext)
	if err != nil {
		return nil, "", fmt.Errorf("kms local wrap: %w", err)
	}
	return ct, version, nil
}

// UnwrapKey implements Provider. The version token is accepted but ignored;
// the local provider always uses its single configured master key.
func (p *LocalProvider) UnwrapKey(_ context.Context, ciphertext []byte, _ string) ([]byte, error) {
	pt, err := cryptoutil.OpenGCM(p.masterKey, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("kms local unwrap: %w", err)
	}
	return pt, nil
}
