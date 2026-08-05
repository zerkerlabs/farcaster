package credential

import (
	"context"
	"errors"
	"fmt"

	"github.com/zerkerlabs/gateway/gateway/internal/kms"
)

// Service orchestrates credential creation, decryption, and KEK rotation. It
// combines a Store (credential rows), a KEKStore (wrapped per-tenant KEKs),
// a KMS Provider (wrapping/unwrapping KEKs), and a VaultResolver
// (source=vault credentials).
//
// Plaintext secret material is held only in local stack variables. It is never
// assigned to a struct field, logged, or returned to callers except as the
// explicit return value of Decrypt.
type Service struct {
	store    Store
	kekStore KEKStore
	provider kms.Provider
	vault    VaultResolver
}

// NewService returns a Service backed by the provided dependencies.
func NewService(store Store, kekStore KEKStore, provider kms.Provider, vault VaultResolver) *Service {
	return &Service{
		store:    store,
		kekStore: kekStore,
		provider: provider,
		vault:    vault,
	}
}

// CreateParams carries the inputs for creating a new credential.
//   - Source == SourceManaged: Plaintext must be non-empty; VaultRef is ignored.
//   - Source == SourceVault: VaultRef must be non-empty; Plaintext is ignored.
type CreateParams struct {
	Name      string
	AuthType  AuthType
	Source    Source
	Plaintext []byte // managed only; envelope-encrypted in memory, never persisted
	VaultRef  string // vault only
}

// Create stores a new credential under tenantID. For source=managed the
// Plaintext is envelope-encrypted before any persistence call; the
// returned Credential contains ciphertext only. For source=vault only the
// VaultRef pointer is stored.
func (s *Service) Create(ctx context.Context, tenantID string, p CreateParams) (*Credential, error) {
	c := &Credential{
		Name:     p.Name,
		AuthType: p.AuthType,
		Source:   p.Source,
	}
	switch p.Source {
	case SourceManaged:
		if err := s.encryptInto(ctx, tenantID, p.Plaintext, c); err != nil {
			return nil, err
		}
	case SourceVault:
		c.VaultRef = p.VaultRef
	default:
		return nil, fmt.Errorf("unknown credential source %q", p.Source)
	}
	if err := s.store.Create(ctx, tenantID, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Decrypt resolves a credential to its plaintext secret for use in the proxy
// hot path. For source=managed it performs the full envelope-decryption chain:
// unwrap KEK via KMS → decrypt DEK with KEK → decrypt secret with DEK. For
// source=vault it delegates to the VaultResolver.
//
// The returned plaintext is ephemeral. Callers must use it immediately and
// must not persist, log, or echo it in any response (invariant #4, AGENTS.md).
func (s *Service) Decrypt(ctx context.Context, tenantID, id string) ([]byte, error) {
	c, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	switch c.Source {
	case SourceManaged:
		return s.decryptManaged(ctx, tenantID, c)
	case SourceVault:
		return s.vault.Resolve(ctx, c.VaultRef)
	default:
		return nil, fmt.Errorf("unknown credential source %q", c.Source)
	}
}

// UpdateParams carries the mutable inputs for a credential update. Nil pointer
// fields are left unchanged. For source=managed, a non-nil Plaintext is
// re-encrypted under the current KEK; nil Plaintext leaves the ciphertext
// untouched.
type UpdateParams struct {
	Name      *string   // nil = unchanged
	AuthType  *AuthType // nil = unchanged
	Plaintext []byte    // nil = unchanged; for source=managed re-encrypts under current KEK
	VaultRef  *string   // nil = unchanged; for source=vault only
}

// Get fetches one credential by ID within tenantID. Returned Credential
// contains ciphertext only; callers requiring the plaintext must use Decrypt.
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Credential, error) {
	return s.store.Get(ctx, tenantID, id)
}

// List returns all credentials for tenantID ordered by created_at ASC.
func (s *Service) List(ctx context.Context, tenantID string) ([]*Credential, error) {
	return s.store.List(ctx, tenantID)
}

// Delete removes a credential. Returns ErrReferenced if any agent holds a
// credential_ref pointing to this credential (spec 0002 §Q2: maps to HTTP 409).
func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.store.Delete(ctx, tenantID, id)
}

// Update applies non-nil UpdateParams fields to the credential and bumps its
// version. For managed credentials, a non-nil Plaintext is re-encrypted under
// the current KEK; the ciphertext columns are then updated atomically with
// any other field changes. The returned Credential contains ciphertext only.
func (s *Service) Update(ctx context.Context, tenantID, id string, p UpdateParams) (*Credential, error) {
	fields := UpdateFields{
		Name:     p.Name,
		AuthType: p.AuthType,
		VaultRef: p.VaultRef,
	}
	if p.Plaintext != nil {
		// Re-encrypt the new plaintext under the current KEK. encryptInto
		// populates EncryptedDEK, KEKVersion, EncryptedSecret, and MaskedHint on
		// a temporary Credential value — only the ciphertext fields matter here.
		tmp := &Credential{}
		if err := s.encryptInto(ctx, tenantID, p.Plaintext, tmp); err != nil {
			return nil, err
		}
		fields.EncryptedDEK = tmp.EncryptedDEK
		kv := tmp.KEKVersion
		fields.KEKVersion = &kv
		fields.EncryptedSecret = tmp.EncryptedSecret
		mh := tmp.MaskedHint
		fields.MaskedHint = &mh
	}
	return s.store.Update(ctx, tenantID, id, fields)
}

// RotateKEK generates a new KEK for tenantID, stores it wrapped by the KMS,
// and re-wraps every managed credential's DEK under the new KEK. The
// encrypted_secret column is never touched — only encrypted_dek and
// kek_version change. This satisfies the acceptance criterion: "KEK rotation
// re-wraps DEKs without re-encrypting rows."
func (s *Service) RotateKEK(ctx context.Context, tenantID string) error {
	newKEK, err := generateDEK()
	if err != nil {
		return fmt.Errorf("generate new KEK: %w", err)
	}
	// The new plaintext KEK lives for the whole re-wrap loop; wipe it on every
	// exit path so key material doesn't linger on the heap (defense in depth).
	defer zero(newKEK)
	wrappedNew, newVersion, err := s.provider.WrapKey(ctx, newKEK)
	if err != nil {
		return fmt.Errorf("wrap new KEK: %w", err)
	}
	if err := s.kekStore.Put(ctx, tenantID, newVersion, wrappedNew); err != nil {
		return fmt.Errorf("store new KEK: %w", err)
	}

	creds, err := s.store.List(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("list credentials for re-wrap: %w", err)
	}

	for _, c := range creds {
		if c.Source != SourceManaged || c.KEKVersion == newVersion {
			continue
		}
		oldKEK, unwrapErr := s.unwrapKEK(ctx, tenantID, c.KEKVersion)
		if unwrapErr != nil {
			return fmt.Errorf("unwrap old KEK for credential %s: %w", c.ID, unwrapErr)
		}
		dek, decErr := openGCM(oldKEK, c.EncryptedDEK)
		zero(oldKEK) // old KEK no longer needed
		if decErr != nil {
			return fmt.Errorf("decrypt DEK for credential %s: %w", c.ID, decErr)
		}
		newEncDEK, encErr := sealGCM(newKEK, dek)
		zero(dek) // plaintext DEK no longer needed
		if encErr != nil {
			return fmt.Errorf("re-encrypt DEK for credential %s: %w", c.ID, encErr)
		}
		if wdErr := s.store.WrapDEK(ctx, tenantID, c.ID, newEncDEK, newVersion); wdErr != nil {
			return fmt.Errorf("store re-wrapped DEK for credential %s: %w", c.ID, wdErr)
		}
	}
	return nil
}

// zero overwrites b with zeros to wipe plaintext key material from the heap as
// soon as it is no longer needed. Defense in depth: Go gives no guarantee the
// GC hasn't already copied the bytes, but this shrinks the window in which a
// heap or core dump could disclose live key material.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// encryptInto encrypts plaintext and writes all ciphertext fields onto c.
// Gets or creates the current KEK for tenantID from the KEK store.
func (s *Service) encryptInto(ctx context.Context, tenantID string, plaintext []byte, c *Credential) error {
	kek, kekVersion, err := s.currentKEK(ctx, tenantID)
	if err != nil {
		return err
	}
	dek, err := generateDEK()
	if err != nil {
		return err
	}
	encSecret, err := sealGCM(dek, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt secret: %w", err)
	}
	encDEK, err := sealGCM(kek, dek)
	if err != nil {
		return fmt.Errorf("encrypt DEK: %w", err)
	}
	c.EncryptedSecret = encSecret
	c.EncryptedDEK = encDEK
	c.KEKVersion = kekVersion
	c.MaskedHint = maskedHint(plaintext)
	return nil
}

// currentKEK returns the plaintext of the current KEK for tenantID, creating
// one on first use and persisting it (wrapped by the KMS) in the KEK store.
func (s *Service) currentKEK(ctx context.Context, tenantID string) (kek []byte, version string, err error) {
	wrappedKEK, version, err := s.kekStore.GetCurrent(ctx, tenantID)
	if err == nil {
		kek, err = s.provider.UnwrapKey(ctx, wrappedKEK, version)
		return kek, version, err
	}
	if !errors.Is(err, ErrKEKNotFound) {
		return nil, "", err
	}

	// First credential for this tenant — generate and store a KEK.
	kek, err = generateDEK()
	if err != nil {
		return nil, "", fmt.Errorf("generate KEK: %w", err)
	}
	wrapped, version, err := s.provider.WrapKey(ctx, kek)
	if err != nil {
		return nil, "", fmt.Errorf("wrap KEK: %w", err)
	}
	if err := s.kekStore.Put(ctx, tenantID, version, wrapped); err != nil {
		return nil, "", fmt.Errorf("store KEK: %w", err)
	}
	return kek, version, nil
}

// unwrapKEK fetches and decrypts a specific KEK version for tenantID.
func (s *Service) unwrapKEK(ctx context.Context, tenantID, version string) ([]byte, error) {
	wrapped, err := s.kekStore.Get(ctx, tenantID, version)
	if err != nil {
		return nil, err
	}
	return s.provider.UnwrapKey(ctx, wrapped, version)
}

// decryptManaged performs the full envelope decryption chain for a managed
// credential: unwrap KEK → decrypt DEK with KEK → decrypt secret with DEK.
func (s *Service) decryptManaged(ctx context.Context, tenantID string, c *Credential) ([]byte, error) {
	kek, err := s.unwrapKEK(ctx, tenantID, c.KEKVersion)
	if err != nil {
		return nil, fmt.Errorf("unwrap KEK: %w", err)
	}
	dek, err := openGCM(kek, c.EncryptedDEK)
	if err != nil {
		return nil, fmt.Errorf("decrypt DEK: %w", err)
	}
	plaintext, err := openGCM(dek, c.EncryptedSecret)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	return plaintext, nil
}
