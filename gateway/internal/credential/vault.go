package credential

import (
	"context"
	"errors"
)

// ErrVaultNotConfigured is returned by StubVaultResolver. It indicates that
// no real vault backend has been wired up, which is the expected state in v1.
var ErrVaultNotConfigured = errors.New("vault resolver not configured")

// VaultResolver resolves a vault reference to the plaintext secret it points
// to. A real implementation would talk to HashiCorp Vault, AWS Secrets
// Manager, GCP Secret Manager, etc. The v1 implementation is a stub.
type VaultResolver interface {
	// Resolve returns the plaintext secret for the given vault reference. The
	// returned bytes are sensitive — callers must use them immediately in the
	// proxy hot path and must not persist, log, or return them in responses
	// (invariant #4, AGENTS.md).
	Resolve(ctx context.Context, ref string) ([]byte, error)
}

// StubVaultResolver always returns ErrVaultNotConfigured. It is the default
// VaultResolver for v1, allowing the vault code path to compile and be
// exercised in tests without requiring an external secrets store.
type StubVaultResolver struct{}

// Resolve implements VaultResolver. Always returns ErrVaultNotConfigured.
func (StubVaultResolver) Resolve(_ context.Context, _ string) ([]byte, error) {
	return nil, ErrVaultNotConfigured
}
