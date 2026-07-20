//go:build integration

package settlement_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/farcaster/facilitator/db"
	"github.com/zerkerlabs/farcaster/facilitator/internal/settlement"
)

// openPGStore opens a pool from TEST_DATABASE_URL, migrates the facilitator
// schema, and truncates settlements so each store handed to the shared
// behavioral suite starts empty. Skips when no database is configured.
func openPGStore(t *testing.T) settlement.Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration tests")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE settlements`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return settlement.NewPostgresStore(pool)
}

// TestPostgresStore runs the same behavioral suite the memory store passes —
// including the concurrent single-flight test, which here exercises the real
// INSERT ... ON CONFLICT DO NOTHING atomic claim against Postgres.
func TestPostgresStore(t *testing.T) {
	testStoreBehavior(t, openPGStore)
}
