// Package account provides the facilitator account domain type, its storage
// interface, and the client-cert identity mapping the mTLS middleware
// resolves on every /settle request (spec 0007 Decision 3).
//
// A facilitator account represents one managed-tier operator. It is looked up
// by the fingerprint of the TLS client certificate presented on the
// connection; there is no bearer/API-key path (the mTLS handshake itself is
// the authentication).
package account

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"time"

	"github.com/zerkerlabs/gateway/facilitator/internal/guardrail"
)

// ErrNotFound is returned by Store.Lookup when no account matches the given
// certificate fingerprint. The middleware treats this identically to a
// known-but-inactive account (both yield 403 "unknown facilitator account")
// so a caller cannot distinguish "no such account" from "deactivated account".
var ErrNotFound = errors.New("facilitator account not found")

// Account is the stored record mapping one TLS client certificate to one
// facilitator (managed-tier operator) account.
type Account struct {
	ID string

	// Name is a human-readable operator label (for logs, support, billing).
	Name string

	// CertFingerprint is the hex-encoded SHA-256 digest of the client
	// certificate's raw DER bytes (see Fingerprint). It is the unique key
	// Store.Lookup resolves on.
	CertFingerprint string

	// CertSubject is the client certificate's Subject distinguished name,
	// kept for audit/display alongside the fingerprint. It is not used for
	// lookup — fingerprints are unambiguous, subjects are not guaranteed
	// unique.
	CertSubject string

	// Active gates whether the account may be used to authenticate a
	// request. A valid certificate mapping to an inactive account is
	// rejected exactly like an unknown certificate (spec 0007).
	Active bool

	// Guardrails is this account's per-account override of the facilitator's
	// conservative default settle limits (spec 0007 T6, Decision 8): the
	// allowed (network, asset) pairs, the per-transaction maximum, and the
	// daily ceiling. Nil means the account settles under the service-wide
	// defaults (settle.Config.GuardrailDefaults); a non-nil value replaces the
	// defaults wholesale — see guardrail.Limits.
	Guardrails *guardrail.Limits

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store persists facilitator accounts and resolves the client-cert identity
// mapping. Implementations must be safe for concurrent use.
//
// Only an in-memory implementation exists as of this ticket (spec 0007 T1);
// a Postgres-backed implementation lands with the facilitator's persistence
// ticket (T4), following the same storage-interface convention as the
// gateway's credential store (ADR-0006).
type Store interface {
	// Create persists account under a newly assigned ID and timestamps.
	Create(ctx context.Context, account *Account) error

	// Lookup resolves a client certificate fingerprint to its account.
	// Returns ErrNotFound if no account has been issued that fingerprint.
	// Callers must additionally check the returned account's Active field —
	// Lookup itself does not filter on it.
	Lookup(ctx context.Context, certFingerprint string) (*Account, error)
}

// Fingerprint returns the hex-encoded SHA-256 digest of cert's raw DER bytes.
// This is the identity key Account.CertFingerprint stores and Store.Lookup
// resolves on.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}
