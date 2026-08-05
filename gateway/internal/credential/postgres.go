package credential

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/gateway/gateway/internal/resource"
)

const (
	// pgUniqueViolation is the PostgreSQL SQLSTATE for unique_violation.
	pgUniqueViolation = "23505"
	// pgFKViolation is the PostgreSQL SQLSTATE for foreign_key_violation.
	pgFKViolation = "23503"
)

// credSelectCols is the canonical SELECT column list for upstream_credentials.
const credSelectCols = `id, tenant_id, name, auth_type, source, encrypted_dek, kek_version, encrypted_secret, masked_hint, vault_ref, version, created_at, updated_at` //nolint:gosec // column name list, not a credential value

// rowScanner abstracts pgx.Row and pgx.Rows so scanCredential can be used in
// both QueryRow and rows.Next contexts without duplicating scan logic.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanCredential reads one row into a Credential. Nullable TEXT columns are
// scanned into *string; nil is mapped to the empty string. Nullable BYTEA
// columns are scanned into []byte; pgx returns nil for NULL BYTEA.
func scanCredential(row rowScanner) (*Credential, error) {
	var (
		c                Credential
		authType, source string
		kekVersion       *string
		maskedHint       *string
		vaultRef         *string
	)
	if err := row.Scan(
		&c.ID, &c.TenantID, &c.Name, &authType, &source,
		&c.EncryptedDEK, &kekVersion, &c.EncryptedSecret,
		&maskedHint, &vaultRef,
		&c.Version, &c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return nil, err
	}
	c.AuthType = AuthType(authType)
	c.Source = Source(source)
	if kekVersion != nil {
		c.KEKVersion = *kekVersion
	}
	if maskedHint != nil {
		c.MaskedHint = *maskedHint
	}
	if vaultRef != nil {
		c.VaultRef = *vaultRef
	}
	return &c, nil
}

// PostgresStore is a PostgreSQL-backed, tenant-scoped implementation of Store.
// Use NewPostgresStore to construct one.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a PostgresStore backed by pool. pool must already
// be open; the caller is responsible for closing it.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// Create implements Store.
func (s *PostgresStore) Create(ctx context.Context, tenantID string, c *Credential) error {
	id, err := resource.New("cred")
	if err != nil {
		return err
	}

	// Map empty strings to nil so nullable columns are stored as NULL.
	var kekVersion, maskedHint, vaultRef *string
	if c.KEKVersion != "" {
		kekVersion = &c.KEKVersion
	}
	if c.MaskedHint != "" {
		maskedHint = &c.MaskedHint
	}
	if c.VaultRef != "" {
		vaultRef = &c.VaultRef
	}

	row := s.pool.QueryRow(
		ctx, `
		INSERT INTO upstream_credentials
			(id, tenant_id, name, auth_type, source,
			 encrypted_dek, kek_version, encrypted_secret, masked_hint,
			 vault_ref, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1, NOW(), NOW())
		RETURNING `+credSelectCols,
		id, tenantID, c.Name, string(c.AuthType), string(c.Source),
		c.EncryptedDEK, kekVersion, c.EncryptedSecret, maskedHint,
		vaultRef,
	)
	got, err := scanCredential(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return ErrNameConflict
		}
		return fmt.Errorf("create credential: %w", err)
	}
	*c = *got
	return nil
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (*Credential, error) {
	row := s.pool.QueryRow(
		ctx,
		`SELECT `+credSelectCols+` FROM upstream_credentials WHERE id=$1 AND tenant_id=$2`,
		id, tenantID,
	)
	c, err := scanCredential(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get credential: %w", err)
	}
	return c, nil
}

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, tenantID string) ([]*Credential, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT `+credSelectCols+` FROM upstream_credentials WHERE tenant_id=$1 ORDER BY created_at ASC, id ASC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()

	var creds []*Credential
	for rows.Next() {
		c, scanErr := scanCredential(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan credential: %w", scanErr)
		}
		creds = append(creds, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	if creds == nil {
		creds = []*Credential{}
	}
	return creds, nil
}

// Update implements Store.
//
// Concurrency: this is last-writer-wins. It reads the current row, merges the
// supplied fields, and writes back with version=version+1, but it does NOT
// guard the write with the read version, so two concurrent updates both succeed
// and the later one overwrites the earlier without a conflict signal. The
// version column is informational here (and authoritative only on the
// WrapDEK/rotation path). Optimistic concurrency (WHERE version=$n →
// ErrConflict) is deferred to the credentials HTTP API (#29), where the
// caller-supplied-version contract (e.g. an If-Match header) is designed.
func (s *PostgresStore) Update(ctx context.Context, tenantID, id string, fields UpdateFields) (*Credential, error) {
	current, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}

	name := current.Name
	if fields.Name != nil {
		name = *fields.Name
	}
	authType := current.AuthType
	if fields.AuthType != nil {
		authType = *fields.AuthType
	}
	encDEK := current.EncryptedDEK
	if fields.EncryptedDEK != nil {
		encDEK = fields.EncryptedDEK
	}
	kekVersion := current.KEKVersion
	if fields.KEKVersion != nil {
		kekVersion = *fields.KEKVersion
	}
	encSecret := current.EncryptedSecret
	if fields.EncryptedSecret != nil {
		encSecret = fields.EncryptedSecret
	}
	mh := current.MaskedHint
	if fields.MaskedHint != nil {
		mh = *fields.MaskedHint
	}
	vr := current.VaultRef
	if fields.VaultRef != nil {
		vr = *fields.VaultRef
	}

	// Map empty strings to nil for nullable columns.
	var kekVersionPtr, maskedHintPtr, vaultRefPtr *string
	if kekVersion != "" {
		kekVersionPtr = &kekVersion
	}
	if mh != "" {
		maskedHintPtr = &mh
	}
	if vr != "" {
		vaultRefPtr = &vr
	}

	row := s.pool.QueryRow(
		ctx, `
		UPDATE upstream_credentials
		SET name=$3, auth_type=$4,
		    encrypted_dek=$5, kek_version=$6, encrypted_secret=$7, masked_hint=$8,
		    vault_ref=$9, version=version+1, updated_at=NOW()
		WHERE id=$1 AND tenant_id=$2
		RETURNING `+credSelectCols,
		id, tenantID, name, string(authType),
		encDEK, kekVersionPtr, encSecret, maskedHintPtr,
		vaultRefPtr,
	)
	updated, err := scanCredential(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, ErrNameConflict
		}
		return nil, fmt.Errorf("update credential: %w", err)
	}
	return updated, nil
}

// Delete implements Store. A FK violation from agents.credential_ref is
// translated to ErrReferenced so the API layer can return 409.
func (s *PostgresStore) Delete(ctx context.Context, tenantID, id string) error {
	tag, err := s.pool.Exec(
		ctx,
		`DELETE FROM upstream_credentials WHERE id=$1 AND tenant_id=$2`,
		id, tenantID,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgFKViolation {
			return ErrReferenced
		}
		return fmt.Errorf("delete credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// WrapDEK implements Store. Updates only encrypted_dek and kek_version so
// that encrypted_secret remains unchanged during KEK rotation.
func (s *PostgresStore) WrapDEK(ctx context.Context, tenantID, id string, encryptedDEK []byte, kekVersion string) error {
	tag, err := s.pool.Exec(
		ctx, `
		UPDATE upstream_credentials
		SET encrypted_dek=$3, kek_version=$4, updated_at=NOW()
		WHERE id=$1 AND tenant_id=$2`,
		id, tenantID, encryptedDEK, kekVersion,
	)
	if err != nil {
		return fmt.Errorf("wrap DEK: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PostgresKEKStore is a PostgreSQL-backed implementation of KEKStore that
// persists per-tenant wrapped KEKs in the tenant_keks table.
type PostgresKEKStore struct {
	pool *pgxpool.Pool
}

// NewPostgresKEKStore returns a PostgresKEKStore backed by pool.
func NewPostgresKEKStore(pool *pgxpool.Pool) *PostgresKEKStore {
	return &PostgresKEKStore{pool: pool}
}

// GetCurrent implements KEKStore.
func (k *PostgresKEKStore) GetCurrent(ctx context.Context, tenantID string) ([]byte, string, error) {
	var wrappedKEK []byte
	var version string
	err := k.pool.QueryRow(
		ctx, `
		SELECT wrapped_kek, version
		FROM tenant_keks
		WHERE tenant_id=$1
		ORDER BY created_at DESC
		LIMIT 1`,
		tenantID,
	).Scan(&wrappedKEK, &version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", ErrKEKNotFound
		}
		return nil, "", fmt.Errorf("get current KEK: %w", err)
	}
	return wrappedKEK, version, nil
}

// Get implements KEKStore.
func (k *PostgresKEKStore) Get(ctx context.Context, tenantID, version string) ([]byte, error) {
	var wrappedKEK []byte
	err := k.pool.QueryRow(
		ctx,
		`SELECT wrapped_kek FROM tenant_keks WHERE tenant_id=$1 AND version=$2`,
		tenantID, version,
	).Scan(&wrappedKEK)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrKEKNotFound
		}
		return nil, fmt.Errorf("get KEK: %w", err)
	}
	return wrappedKEK, nil
}

// Put implements KEKStore.
func (k *PostgresKEKStore) Put(ctx context.Context, tenantID, version string, wrappedKEK []byte) error {
	_, err := k.pool.Exec(
		ctx,
		`INSERT INTO tenant_keks (tenant_id, version, wrapped_kek) VALUES ($1, $2, $3)`,
		tenantID, version, wrappedKEK,
	)
	if err != nil {
		return fmt.Errorf("put KEK: %w", err)
	}
	return nil
}
