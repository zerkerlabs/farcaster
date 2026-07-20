package settlement

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// selectCols is the canonical column list for settlement_config reads.
const selectCols = `tenant_id, facilitator_url, facilitator_credential_ref, created_at, updated_at`

// PostgresStore is a PostgreSQL-backed, tenant-scoped implementation of Store.
// Use NewPostgresStore to construct one; do not copy by value.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a PostgresStore that uses pool for all queries.
// pool must already be open; the caller is responsible for closing it.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func scanConfig(row pgx.Row) (*Config, error) {
	var c Config
	if err := row.Scan(&c.TenantID, &c.FacilitatorURL, &c.FacilitatorCredentialRef, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, tenantID string) (*Config, error) {
	row := s.pool.QueryRow(
		ctx,
		`SELECT `+selectCols+` FROM settlement_config WHERE tenant_id=$1`,
		tenantID,
	)
	c, err := scanConfig(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get settlement config: %w", err)
	}
	return c, nil
}

// Upsert implements Store without a read-modify-write race. An existing row is
// merged with a single atomic UPDATE whose COALESCE reads each column's live
// value in-statement, so two concurrent PATCHes each rotating a different field
// can no longer clobber one another (neither reads the row into Go first). Only
// when no row exists yet does it fall back to an INSERT — the first-configure
// case, which must supply both required fields (ErrIncomplete otherwise). The
// COALESCE-in-VALUES trick cannot be used here because facilitator_url /
// facilitator_credential_ref are NOT NULL: Postgres checks NOT NULL on the
// candidate INSERT tuple before ON CONFLICT can route it to the UPDATE branch.
func (s *PostgresStore) Upsert(ctx context.Context, tenantID string, fields UpdateFields) (*Config, error) {
	row := s.pool.QueryRow(
		ctx, `
		UPDATE settlement_config
		SET facilitator_url=COALESCE($2, facilitator_url),
		    facilitator_credential_ref=COALESCE($3, facilitator_credential_ref),
		    updated_at=NOW()
		WHERE tenant_id=$1
		RETURNING `+selectCols,
		tenantID, fields.FacilitatorURL, fields.FacilitatorCredentialRef,
	)
	updated, err := scanConfig(row)
	if err == nil {
		return updated, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("upsert settlement config: %w", err)
	}

	// No row yet: first-time configure must set both fields.
	if fields.FacilitatorURL == nil || *fields.FacilitatorURL == "" ||
		fields.FacilitatorCredentialRef == nil || *fields.FacilitatorCredentialRef == "" {
		return nil, ErrIncomplete
	}
	// ON CONFLICT DO UPDATE guards the rare race where another first-configure
	// for this tenant inserted between the UPDATE above and this INSERT.
	row = s.pool.QueryRow(
		ctx, `
		INSERT INTO settlement_config (tenant_id, facilitator_url, facilitator_credential_ref, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())
		ON CONFLICT (tenant_id) DO UPDATE
		SET facilitator_url=EXCLUDED.facilitator_url,
		    facilitator_credential_ref=EXCLUDED.facilitator_credential_ref,
		    updated_at=NOW()
		RETURNING `+selectCols,
		tenantID, *fields.FacilitatorURL, *fields.FacilitatorCredentialRef,
	)
	inserted, err := scanConfig(row)
	if err != nil {
		return nil, fmt.Errorf("upsert settlement config: %w", err)
	}
	return inserted, nil
}
