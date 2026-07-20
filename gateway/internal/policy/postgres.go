package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// selectCols is the canonical column list for policies reads.
const selectCols = `tenant_id, version, default_action, on_error, rules, created_at, updated_at`

// PostgresStore is a PostgreSQL-backed, tenant-scoped implementation of
// PolicyStore. Use NewPostgresStore to construct one; do not copy by value.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a PostgresStore that uses pool for all queries.
// pool must already be open; the caller is responsible for closing it.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func scanPolicy(row pgx.Row) (*Policy, error) {
	var (
		p         Policy
		defAction string
		onError   string
		rulesJSON []byte
	)
	if err := row.Scan(&p.TenantID, &p.Version, &defAction, &onError, &rulesJSON, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Default = Action(defAction)
	p.OnError = Action(onError)
	if len(rulesJSON) > 0 {
		if err := json.Unmarshal(rulesJSON, &p.Rules); err != nil {
			return nil, fmt.Errorf("unmarshal rules: %w", err)
		}
	}
	return &p, nil
}

// Get implements PolicyStore.
func (s *PostgresStore) Get(ctx context.Context, tenantID string) (*Policy, error) {
	row := s.pool.QueryRow(
		ctx,
		`SELECT `+selectCols+` FROM policies WHERE tenant_id=$1`,
		tenantID,
	)
	p, err := scanPolicy(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get policy: %w", err)
	}
	return p, nil
}

// Put implements PolicyStore. It upserts the document in a single statement:
// on conflict, version is bumped from the existing row's live value so two
// concurrent Puts for the same tenant cannot compute a stale increment from a
// value each read into Go separately.
func (s *PostgresStore) Put(ctx context.Context, tenantID string, fields PutFields) (*Policy, error) {
	rulesJSON, err := json.Marshal(fields.Rules)
	if err != nil {
		return nil, fmt.Errorf("marshal rules: %w", err)
	}

	row := s.pool.QueryRow(
		ctx, `
		INSERT INTO policies (tenant_id, version, default_action, on_error, rules, created_at, updated_at)
		VALUES ($1, 1, $2, $3, $4, NOW(), NOW())
		ON CONFLICT (tenant_id) DO UPDATE
		SET version=policies.version + 1,
		    default_action=EXCLUDED.default_action,
		    on_error=EXCLUDED.on_error,
		    rules=EXCLUDED.rules,
		    updated_at=NOW()
		RETURNING `+selectCols,
		tenantID, string(fields.Default), string(fields.OnError), rulesJSON,
	)
	p, err := scanPolicy(row)
	if err != nil {
		return nil, fmt.Errorf("put policy: %w", err)
	}
	return p, nil
}
