// Package db provides database lifecycle helpers for the facilitator. The
// facilitator runs its own Postgres — a separate trust domain from the
// gateway's database (spec 0007 Decision 9) — so it carries its own migrations
// and its own migrate runner rather than sharing the gateway's. The only
// exported entry point is Migrate, which applies any pending numbered SQL
// migrations to the target database.
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate creates the schema_migrations tracking table if needed, then applies
// every unapplied *.sql file from db/migrations in lexicographic (version)
// order. Each migration runs in its own transaction so a partial failure leaves
// the database in a consistent state.
//
// Migrate is idempotent: files whose version is already recorded in
// schema_migrations are skipped. It is safe to call on every startup.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	// Run migrations on a single dedicated connection so the advisory lock and
	// every migration share one session.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Release()

	// Serialize concurrent migrators across instances: a second instance blocks
	// here until the first finishes, instead of racing on schema_migrations.
	// The lock key is facilitator-specific so it can never collide with the
	// gateway's migrator if they ever shared a database by accident.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('facilitator_migrate'))`); err != nil {
		return fmt.Errorf("acquire migrate lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('facilitator_migrate'))`)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT        PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, name,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists {
			continue
		}

		sql, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin tx for migration %s: %w", name, err)
		}

		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(
			ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, name,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}

	return nil
}
