// Package kms defines the Provider interface for wrapping and unwrapping Key
// Encryption Keys (KEKs). The LocalProvider is suitable for local development
// and integration tests. Cloud implementations (AWS KMS, GCP Cloud KMS, etc.)
// are pluggable via the same interface without any changes to callers.
package kms

import "context"

// Provider wraps and unwraps Key Encryption Keys (KEKs).
// Implementations must treat every plaintext argument as sensitive material:
// never log key bytes, surface them in error messages, or persist them
// outside the backing KMS store.
type Provider interface {
	// WrapKey encrypts plaintext key material and returns the ciphertext plus
	// an opaque version token. The version token identifies which KMS key (or
	// key version) produced the ciphertext. It must be stored alongside the
	// ciphertext and passed back to UnwrapKey so the provider can select the
	// correct decryption key after a KMS key rotation.
	WrapKey(ctx context.Context, plaintext []byte) (ciphertext []byte, version string, err error)

	// UnwrapKey decrypts a ciphertext produced by WrapKey. version is the
	// opaque token returned by the corresponding WrapKey call.
	UnwrapKey(ctx context.Context, ciphertext []byte, version string) (plaintext []byte, err error)
}
