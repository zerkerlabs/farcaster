//go:build integration

package settlement_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/farcaster/gateway/db"
	"github.com/zerkerlabs/farcaster/gateway/internal/credential"
	"github.com/zerkerlabs/farcaster/gateway/internal/kms"
	"github.com/zerkerlabs/farcaster/gateway/internal/settlement"
)

// credSeq gives each seeded credential a unique name; the credential store
// enforces name uniqueness per tenant, so tests that seed more than one
// credential for the same tenant must not reuse a name.
var credSeq atomic.Int64

// testKMSKey is a fixed 32-byte (64 hex char) master key. Pinning it makes
// kms.LocalProvider deterministic so every provider seedCredential constructs
// within a test shares one master key and can unwrap the tenant KEK a prior
// seed persisted. Without it, CI (which sets no ZERKER_KMS_KEY) hands each
// provider a random ephemeral key and the second seed for a tenant fails to
// unwrap the existing KEK ("message authentication failed").
const testKMSKey = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

// openTestPool opens a connection pool from TEST_DATABASE_URL, runs
// migrations, and truncates all relevant tables. Registers cleanup via
// t.Cleanup.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	t.Setenv("ZERKER_KMS_KEY", testKMSKey)
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

	if _, err := pool.Exec(
		ctx,
		`TRUNCATE TABLE settlement_config, invocations, agents, upstream_credentials, tenant_keks`,
	); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
	return pool
}

// seedCredential creates a managed credential owned by tenantID and returns
// its ID, so tests can exercise a facilitator_credential_ref that resolves.
func seedCredential(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	provider, err := kms.NewLocalProvider()
	if err != nil {
		t.Fatalf("NewLocalProvider: %v", err)
	}
	svc := credential.NewService(
		credential.NewPostgresStore(pool),
		credential.NewPostgresKEKStore(pool),
		provider,
		credential.StubVaultResolver{},
	)
	c, err := svc.Create(context.Background(), tenantID, credential.CreateParams{
		Name:      fmt.Sprintf("facilitator-auth-%d", credSeq.Add(1)),
		AuthType:  credential.AuthTypeBearer,
		Source:    credential.SourceManaged,
		Plaintext: []byte("facilitator-secret-1234567890ab"),
	})
	if err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return c.ID
}

func TestPG_GetAbsentReturnsNotFound(t *testing.T) {
	pool := openTestPool(t)
	s := settlement.NewPostgresStore(pool)

	if _, err := s.Get(context.Background(), "tenant-alpha"); !errors.Is(err, settlement.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestPG_UpsertRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	credRef := seedCredential(t, pool, "tenant-alpha")
	s := settlement.NewPostgresStore(pool)

	cfg, err := s.Upsert(context.Background(), "tenant-alpha", settlement.UpdateFields{
		FacilitatorURL:           strPtr("https://settle.example.com"),
		FacilitatorCredentialRef: strPtr(credRef),
	})
	if err != nil {
		t.Fatalf("Upsert (create): %v", err)
	}
	if cfg.FacilitatorURL != "https://settle.example.com" || cfg.FacilitatorCredentialRef != credRef {
		t.Errorf("Upsert() = %+v", cfg)
	}

	got, err := s.Get(context.Background(), "tenant-alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FacilitatorURL != cfg.FacilitatorURL || got.FacilitatorCredentialRef != cfg.FacilitatorCredentialRef {
		t.Errorf("Get() = %+v, want match with %+v", got, cfg)
	}

	// Partial update: only the credential ref changes.
	credRef2 := seedCredential(t, pool, "tenant-alpha")
	updated, err := s.Upsert(context.Background(), "tenant-alpha", settlement.UpdateFields{
		FacilitatorCredentialRef: strPtr(credRef2),
	})
	if err != nil {
		t.Fatalf("Upsert (partial): %v", err)
	}
	if updated.FacilitatorURL != "https://settle.example.com" {
		t.Errorf("FacilitatorURL changed unexpectedly: %q", updated.FacilitatorURL)
	}
	if updated.FacilitatorCredentialRef != credRef2 {
		t.Errorf("FacilitatorCredentialRef = %q, want %q", updated.FacilitatorCredentialRef, credRef2)
	}
}

func TestPG_UpsertFirstConfigureRequiresBothFields(t *testing.T) {
	pool := openTestPool(t)
	s := settlement.NewPostgresStore(pool)

	if _, err := s.Upsert(context.Background(), "tenant-alpha", settlement.UpdateFields{
		FacilitatorURL: strPtr("https://settle.example.com"),
	}); !errors.Is(err, settlement.ErrIncomplete) {
		t.Errorf("Upsert() error = %v, want ErrIncomplete", err)
	}
}

func TestPG_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	credRef := seedCredential(t, pool, "tenant-alpha")
	s := settlement.NewPostgresStore(pool)

	if _, err := s.Upsert(context.Background(), "tenant-alpha", settlement.UpdateFields{
		FacilitatorURL:           strPtr("https://settle.example.com"),
		FacilitatorCredentialRef: strPtr(credRef),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if _, err := s.Get(context.Background(), "tenant-beta"); !errors.Is(err, settlement.ErrNotFound) {
		t.Errorf("Get(tenant-beta) error = %v, want ErrNotFound", err)
	}
}

// TestPG_UpsertConcurrentDistinctFieldsNoLostUpdate is a regression test for the
// read-modify-write race the atomic COALESCE Upsert fixes: two callers each
// rotating a *different* single field must not clobber one another. Each
// goroutine only ever writes its own field, so once both have made their final
// write the row must carry both values — a merge done in Go (Get then write-back)
// could interleave and drop one.
func TestPG_UpsertConcurrentDistinctFieldsNoLostUpdate(t *testing.T) {
	pool := openTestPool(t)
	credRefA := seedCredential(t, pool, "tenant-alpha")
	credRefB := seedCredential(t, pool, "tenant-alpha")
	s := settlement.NewPostgresStore(pool)

	// Seed a complete config so the concurrent upserts take the UPDATE branch.
	if _, err := s.Upsert(context.Background(), "tenant-alpha", settlement.UpdateFields{
		FacilitatorURL:           strPtr("https://seed.example.com"),
		FacilitatorCredentialRef: strPtr(credRefA),
	}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	const (
		wantURL = "https://final.example.com"
		iters   = 50
	)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if _, err := s.Upsert(context.Background(), "tenant-alpha", settlement.UpdateFields{
				FacilitatorURL: strPtr(wantURL),
			}); err != nil {
				t.Errorf("url upsert: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if _, err := s.Upsert(context.Background(), "tenant-alpha", settlement.UpdateFields{
				FacilitatorCredentialRef: strPtr(credRefB),
			}); err != nil {
				t.Errorf("ref upsert: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	got, err := s.Get(context.Background(), "tenant-alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FacilitatorURL != wantURL {
		t.Errorf("FacilitatorURL = %q, want %q (lost update?)", got.FacilitatorURL, wantURL)
	}
	if got.FacilitatorCredentialRef != credRefB {
		t.Errorf("FacilitatorCredentialRef = %q, want %q (lost update?)", got.FacilitatorCredentialRef, credRefB)
	}
}

func strPtr(s string) *string { return &s }
