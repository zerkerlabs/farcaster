package settlement

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/farcaster/facilitator/internal/guardrail"
)

// selectCols is the canonical column list for settlement reads, in scan order.
const selectCols = `id, account_id, nonce, payer, pay_to, network, asset, value, valid_after, valid_before, tx_hash, status, error_reason, created_at, updated_at`

// PostgresStore is a PostgreSQL-backed Store using the facilitator's own
// database (spec 0007 Decision 9). Construct with NewPostgresStore; do not copy
// by value.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a PostgresStore that uses pool for all queries. pool
// must already be open; the caller owns closing it.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func scanSettlement(row pgx.Row) (*Settlement, error) {
	var s Settlement
	var status string
	if err := row.Scan(
		&s.ID, &s.AccountID, &s.Nonce, &s.Payer, &s.PayTo,
		&s.Network, &s.Asset, &s.Value, &s.ValidAfter, &s.ValidBefore,
		&s.TxHash, &status, &s.ErrorReason,
		&s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return nil, err
	}
	s.Status = Status(status)
	return &s, nil
}

// Claim implements Store. The whole check-existing-ceiling-and-insert runs
// inside one transaction, serialized per account by a Postgres advisory lock
// (pg_advisory_xact_lock, keyed on account_id and held for the transaction's
// lifetime): without that lock, two concurrent transactions could each read
// the same committed-today total before either committed its insert, each
// pass the ceiling check independently, and collectively exceed it. Inside
// the lock, a retry of an already-claimed (account_id, nonce) is detected
// first — mirroring MemoryStore.Claim — and its existing row is replayed
// without touching the ceiling check at all, so a legitimate retry near the
// daily ceiling can't be misreported as guardrail-exceeded. The actual row
// insert is still a normal
// INSERT ... ON CONFLICT (account_id, nonce) DO NOTHING RETURNING — the
// unique index remains the belt-and-suspenders guarantee that exactly one
// caller ever claims a given (account, nonce) even if the advisory lock were
// ever bypassed.
func (s *PostgresStore) Claim(ctx context.Context, key ClaimKey, ceiling DailyCeiling) (*Settlement, bool, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("claim settlement: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit succeeds

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, key.AccountID); err != nil {
		return nil, false, fmt.Errorf("claim settlement: acquire guardrail lock: %w", err)
	}

	if existing, err := scanSettlement(tx.QueryRow(
		ctx, `SELECT `+selectCols+` FROM settlements WHERE account_id=$1 AND nonce=$2`,
		key.AccountID, key.Nonce,
	)); err == nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("claim settlement: commit: %w", err)
		}
		return existing, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("claim settlement (check existing): %w", err)
	}

	var sumText string
	err = tx.QueryRow(
		ctx, `
		SELECT COALESCE(SUM(value::numeric), 0)::text FROM settlements
		WHERE account_id=$1 AND status IN ('in_flight', 'settled') AND created_at >= $2`,
		key.AccountID, ceiling.Since,
	).Scan(&sumText)
	if err != nil {
		return nil, false, fmt.Errorf("claim settlement: committed total: %w", err)
	}
	committed, ok := new(big.Int).SetString(sumText, 10)
	if !ok {
		return nil, false, fmt.Errorf("claim settlement: non-integer committed total %q", sumText)
	}
	if new(big.Int).Add(committed, ceiling.Amount).Cmp(ceiling.Limit) > 0 {
		return nil, false, guardrail.ErrDailyCeilingExceeded
	}

	row := tx.QueryRow(
		ctx, `
		INSERT INTO settlements
			(id, account_id, nonce, payer, pay_to, network, asset, value, valid_after, valid_before, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'in_flight', NOW(), NOW())
		ON CONFLICT (account_id, nonce) DO NOTHING
		RETURNING `+selectCols,
		"facset_"+id.String(), key.AccountID, key.Nonce, key.Payer, key.PayTo,
		key.Network, key.Asset, key.Value, key.ValidAfter, key.ValidBefore,
	)
	inserted, err := scanSettlement(row)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("claim settlement: commit: %w", err)
		}
		return inserted, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("claim settlement: %w", err)
	}

	// Conflict: despite the pre-check above finding no row, one now exists
	// (only possible if the advisory lock was somehow bypassed) — read and
	// return it within the same transaction for a consistent view.
	existing, err := scanSettlement(tx.QueryRow(
		ctx, `SELECT `+selectCols+` FROM settlements WHERE account_id=$1 AND nonce=$2`,
		key.AccountID, key.Nonce,
	))
	if err != nil {
		return nil, false, fmt.Errorf("claim settlement (fetch existing): %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("claim settlement: commit: %w", err)
	}
	return existing, false, nil
}

// MarkSettled implements Store.
func (s *PostgresStore) MarkSettled(ctx context.Context, id, txHash string) (*Settlement, error) {
	return s.transition(ctx, id, StatusSettled, txHash, "")
}

// MarkFailed implements Store.
func (s *PostgresStore) MarkFailed(ctx context.Context, id, reason, txHash string) (*Settlement, error) {
	return s.transition(ctx, id, StatusFailed, txHash, reason)
}

// transition guards MarkSettled/MarkFailed: the UPDATE only matches a row
// that is currently 'in_flight', so a concurrent or later illegal/duplicate
// call can never clobber a terminal row. When the UPDATE matches no row, a
// follow-up read distinguishes an idempotent replay (same target
// status/txHash/reason — return the existing row, no error) from an illegal
// transition (ErrInvalidTransition) or an unknown id (ErrNotFound); in every
// case the row itself is left unchanged by this call.
func (s *PostgresStore) transition(ctx context.Context, id string, status Status, txHash, reason string) (*Settlement, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE settlements
		SET status=$2, tx_hash=$3, error_reason=$4, updated_at=NOW()
		WHERE id=$1 AND status='in_flight'
		RETURNING `+selectCols,
		id, string(status), txHash, reason)
	updated, err := scanSettlement(row)
	if err == nil {
		return updated, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("update settlement: %w", err)
	}

	existing, err := scanSettlement(s.pool.QueryRow(
		ctx, `SELECT `+selectCols+` FROM settlements WHERE id=$1`, id,
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("update settlement (check existing): %w", err)
	}
	if existing.Status == status && existing.TxHash == txHash && existing.ErrorReason == reason {
		return existing, nil
	}
	return nil, ErrInvalidTransition
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, accountID, nonce string) (*Settlement, error) {
	row := s.pool.QueryRow(
		ctx,
		`SELECT `+selectCols+` FROM settlements WHERE account_id=$1 AND nonce=$2`,
		accountID, nonce,
	)
	got, err := scanSettlement(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get settlement: %w", err)
	}
	return got, nil
}

// SumSettled implements Store. value is stored as decimal text (arbitrary
// precision), so the sum runs as ::numeric — never bigint — to avoid
// overflowing a 256-bit token amount; the result is cast back to text so pgx
// hands back a string big.Int can parse rather than a lossy float.
func (s *PostgresStore) SumSettled(ctx context.Context, accountID string, since time.Time) (*big.Int, error) {
	var sumText string
	err := s.pool.QueryRow(
		ctx,
		`SELECT COALESCE(SUM(value::numeric), 0)::text FROM settlements
		 WHERE account_id=$1 AND status='settled' AND created_at >= $2`,
		accountID, since,
	).Scan(&sumText)
	if err != nil {
		return nil, fmt.Errorf("sum settled: %w", err)
	}
	total, ok := new(big.Int).SetString(sumText, 10)
	if !ok {
		return nil, fmt.Errorf("sum settled: non-integer total %q", sumText)
	}
	return total, nil
}

// StaleInFlight implements Store, oldest first so a long-stuck row is
// reconciled before newer ones in the same sweep pass. The partial index on
// (status, created_at) WHERE status = 'in_flight' (migration 002) keeps this
// an index scan rather than a full table scan as the settlements table grows.
func (s *PostgresStore) StaleInFlight(ctx context.Context, olderThan time.Time) ([]*Settlement, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT `+selectCols+` FROM settlements WHERE status='in_flight' AND created_at < $1 ORDER BY created_at ASC, id ASC`,
		olderThan,
	)
	if err != nil {
		return nil, fmt.Errorf("list stale in-flight settlements: %w", err)
	}
	defer rows.Close()

	var out []*Settlement
	for rows.Next() {
		row, err := scanSettlement(rows)
		if err != nil {
			return nil, fmt.Errorf("scan settlement: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stale in-flight settlements: %w", err)
	}
	return out, nil
}

// List implements Store, scoped to one account and ordered newest-first.
func (s *PostgresStore) List(ctx context.Context, accountID string) ([]*Settlement, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT `+selectCols+` FROM settlements WHERE account_id=$1 ORDER BY created_at DESC, id DESC`,
		accountID,
	)
	if err != nil {
		return nil, fmt.Errorf("list settlements: %w", err)
	}
	defer rows.Close()

	var out []*Settlement
	for rows.Next() {
		row, err := scanSettlement(rows)
		if err != nil {
			return nil, fmt.Errorf("scan settlement: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate settlements: %w", err)
	}
	return out, nil
}
