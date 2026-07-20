//go:build integration

package policy_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/farcaster/gateway/db"
	"github.com/zerkerlabs/farcaster/gateway/internal/policy"
)

// openTestPool opens a connection pool from TEST_DATABASE_URL, runs
// migrations, and truncates the policies and policy_decisions tables. Registers cleanup via
// t.Cleanup.
func openTestPool(t *testing.T) *pgxpool.Pool {
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

	if _, err := pool.Exec(ctx, `TRUNCATE TABLE policies, policy_decisions`); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
	return pool
}

func TestPG_GetAbsentReturnsNotFound(t *testing.T) {
	pool := openTestPool(t)
	s := policy.NewPostgresStore(pool)

	if _, err := s.Get(context.Background(), "tenant-alpha"); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestPG_PutRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	s := policy.NewPostgresStore(pool)

	maxBytes := int64(32768)
	fields := policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionAllow, Match: policy.Match{Agents: []string{"agt_01J9X8"}, Tools: []string{"search_docs", "read_*"}}},
			{Action: policy.ActionDeny, Match: policy.Match{Tools: []string{"delete_*"}}},
			{Action: policy.ActionWarn, Match: policy.Match{MaxBodyBytes: &maxBytes}},
		},
	}

	p, err := s.Put(context.Background(), "tenant-alpha", fields)
	if err != nil {
		t.Fatalf("Put (create): %v", err)
	}
	if p.Version != 1 {
		t.Errorf("Version = %d, want 1", p.Version)
	}
	if len(p.Rules) != 3 {
		t.Fatalf("len(Rules) = %d, want 3", len(p.Rules))
	}
	if p.Rules[2].Match.MaxBodyBytes == nil || *p.Rules[2].Match.MaxBodyBytes != maxBytes {
		t.Errorf("Rules[2].Match.MaxBodyBytes = %+v, want %d", p.Rules[2].Match.MaxBodyBytes, maxBytes)
	}

	got, err := s.Get(context.Background(), "tenant-alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Version != p.Version || len(got.Rules) != len(p.Rules) {
		t.Errorf("Get() = %+v, want match with %+v", got, p)
	}

	// A second Put replaces wholesale and bumps the version.
	updated, err := s.Put(context.Background(), "tenant-alpha", policy.PutFields{
		Default: policy.ActionDeny,
		OnError: policy.ActionDeny,
		Rules:   nil,
	})
	if err != nil {
		t.Fatalf("Put (replace): %v", err)
	}
	if updated.Version != 2 {
		t.Errorf("Version = %d, want 2", updated.Version)
	}
	if len(updated.Rules) != 0 {
		t.Errorf("Rules = %+v, want empty after replace", updated.Rules)
	}
}

func TestPG_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := policy.NewPostgresStore(pool)

	if _, err := s.Put(context.Background(), "tenant-alpha", policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := s.Get(context.Background(), "tenant-beta"); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Get(tenant-beta) error = %v, want ErrNotFound", err)
	}
}
