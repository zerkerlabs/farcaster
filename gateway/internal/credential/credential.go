// Package credential provides the UpstreamCredential domain type, its storage
// interfaces, and the Service that implements envelope encryption for the
// routing/proxy surface (spec 0002 §Q2).
//
// Secret material handled by this package is highly sensitive
// (invariant #4, AGENTS.md): plaintext values must never appear in logs,
// error bodies, HTTP responses, or any persistent storage column. Every
// function that receives a plaintext secret is documented as such.
package credential

import (
	"context"
	"errors"
	"time"
)

// Source indicates where the upstream credential secret is stored.
type Source string

const (
	// SourceManaged means the secret is stored envelope-encrypted inside
	// Zerker's database. The encryption hierarchy is:
	//   secret → AES-256-GCM(DEK) → AES-256-GCM(per-tenant KEK) → KMS(master key)
	SourceManaged Source = "managed"

	// SourceVault means the secret lives in an external secrets store and
	// Zerker holds only a reference path. The vault resolver is stubbed in
	// v1 and always returns ErrVaultNotConfigured.
	SourceVault Source = "vault"
)

// AuthType describes how the resolved credential is injected into the
// forwarded upstream request.
type AuthType string

const (
	// AuthTypeBearer injects the secret as an Authorization: Bearer header.
	AuthTypeBearer AuthType = "bearer"
	// AuthTypeAPIKey injects the secret as an API-key header (header name TBD
	// at the proxy layer).
	AuthTypeAPIKey AuthType = "api_key"
	// AuthTypeNone means no credential is injected; the upstream is open.
	AuthTypeNone AuthType = "none"
)

// Sentinel errors.
var (
	// ErrNotFound is returned when a credential ID does not exist within the
	// caller's tenant. "Doesn't exist" and "exists in another tenant" are
	// intentionally indistinguishable (invariant #2, AGENTS.md).
	ErrNotFound = errors.New("credential not found")

	// ErrNameConflict is returned when a credential name is already in use
	// within the same tenant.
	ErrNameConflict = errors.New("credential name already exists in tenant")

	// ErrReferenced is returned when Delete is attempted on a credential still
	// referenced by one or more agents (spec 0002 §Q2; maps to HTTP 409).
	ErrReferenced = errors.New("credential is referenced by one or more agents")

	// ErrKEKNotFound is returned by KEKStore.GetCurrent when no KEK has been
	// created for the tenant yet.
	ErrKEKNotFound = errors.New("no KEK found for tenant")
)

// Credential is the stored record for one upstream authentication credential.
//
// For source=managed, the plaintext secret is never held here; only ciphertext
// columns are populated. Callers resolve the plaintext via Service.Decrypt.
//
// For source=vault, VaultRef is the path in the external secrets store;
// the encryption columns are empty.
type Credential struct {
	ID       string
	TenantID string
	Name     string
	AuthType AuthType
	Source   Source

	// Managed-source fields (zero values when Source == SourceVault).
	EncryptedDEK    []byte // per-secret DEK encrypted by the per-tenant KEK
	KEKVersion      string // opaque token identifying the KEK that wrapped this DEK
	EncryptedSecret []byte // secret ciphertext (AES-256-GCM with the DEK as key)
	MaskedHint      string // last-4 chars of plaintext for display; never the value

	// Vault-source field (empty when Source == SourceManaged).
	VaultRef string

	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store persists and retrieves Credential records. Every method is scoped to
// tenantID — implementations must never return data belonging to a different
// tenant (invariant #2, AGENTS.md).
type Store interface {
	// Create persists c under tenantID. The store assigns ID, TenantID,
	// Version, CreatedAt, and UpdatedAt; the caller must populate all other
	// fields (including pre-computed ciphertext for managed credentials).
	// Returns ErrNameConflict if a credential with the same Name exists.
	Create(ctx context.Context, tenantID string, c *Credential) error

	// Get fetches one credential by ID. The returned struct contains ciphertext
	// only; callers decrypt via Service.Decrypt. Returns ErrNotFound if the ID
	// is absent or in another tenant.
	Get(ctx context.Context, tenantID, id string) (*Credential, error)

	// List returns all credentials for tenantID ordered by created_at ASC.
	List(ctx context.Context, tenantID string) ([]*Credential, error)

	// Update applies non-nil UpdateFields to the credential and bumps Version.
	// Returns ErrNotFound if the ID is absent or in another tenant.
	Update(ctx context.Context, tenantID, id string, fields UpdateFields) (*Credential, error)

	// Delete removes the credential. Returns ErrNotFound if the ID is absent
	// or in another tenant. Returns ErrReferenced if any agent holds a
	// credential_ref pointing to this credential (spec 0002 §Q2: 409).
	Delete(ctx context.Context, tenantID, id string) error

	// WrapDEK replaces encrypted_dek and kek_version without touching
	// encrypted_secret. Called exclusively during KEK rotation so that DEKs
	// are re-wrapped without re-encrypting rows (acceptance criterion).
	WrapDEK(ctx context.Context, tenantID, id string, encryptedDEK []byte, kekVersion string) error
}

// UpdateFields carries the fields that may change during a credential update.
// A nil pointer means "leave this field unchanged". A nil []byte also means
// "leave unchanged".
type UpdateFields struct {
	Name            *string
	AuthType        *AuthType
	EncryptedDEK    []byte // nil = unchanged
	KEKVersion      *string
	EncryptedSecret []byte // nil = unchanged
	MaskedHint      *string
	VaultRef        *string
}

// KEKStore persists per-tenant Key Encryption Keys already wrapped by the KMS
// provider. Only ciphertext is stored; the KMS provider decrypts on demand.
type KEKStore interface {
	// GetCurrent returns the most recently stored wrapped KEK for tenantID.
	// Returns (nil, "", ErrKEKNotFound) when no KEK has been created yet.
	GetCurrent(ctx context.Context, tenantID string) (wrappedKEK []byte, version string, err error)

	// Get returns a specific wrapped KEK by version for tenantID. Returns
	// ErrKEKNotFound if the version does not exist for this tenant.
	Get(ctx context.Context, tenantID, version string) (wrappedKEK []byte, err error)

	// Put stores a new KMS-wrapped KEK under version for tenantID. version is
	// the opaque token returned by KMSProvider.WrapKey.
	Put(ctx context.Context, tenantID, version string, wrappedKEK []byte) error
}
