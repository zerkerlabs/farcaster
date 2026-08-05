//go:build integration

package credential_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/gateway/gateway/db"
	"github.com/zerkerlabs/gateway/gateway/internal/credential"
	"github.com/zerkerlabs/gateway/gateway/internal/kms"
)

// openTestPool opens a connection pool from TEST_DATABASE_URL, runs
// migrations, and truncates all relevant tables. Registers cleanup via
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

	// Truncate all tables that reference upstream_credentials (directly or via
	// agents), plus the KEK store. settlement_config was added by migration 013
	// with an FK to upstream_credentials, so it must be truncated alongside it —
	// otherwise TRUNCATE fails with "cannot truncate a table referenced in a
	// foreign key constraint".
	if _, err := pool.Exec(
		ctx,
		`TRUNCATE TABLE settlement_config, invocations, agents, upstream_credentials, tenant_keks`,
	); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
	return pool
}

func newPGService(t *testing.T) (*credential.Service, *pgxpool.Pool) {
	t.Helper()
	pool := openTestPool(t)
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
	return svc, pool
}

// ----------------------------------------- managed create + decrypt roundtrip ---

func TestPG_CreateManaged_DecryptRoundtrip(t *testing.T) {
	svc, _ := newPGService(t)
	secret := []byte("pg-secret-key-xyz-1234567890abc")

	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name:      "pg-key",
		AuthType:  credential.AuthTypeBearer,
		Source:    credential.SourceManaged,
		Plaintext: secret,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.ID == "" {
		t.Error("ID is empty")
	}
	if c.TenantID != tenantA {
		t.Errorf("TenantID = %q, want %q", c.TenantID, tenantA)
	}
	// Ciphertext must not equal plaintext.
	if bytes.Equal(c.EncryptedSecret, secret) {
		t.Error("EncryptedSecret equals plaintext — no encryption occurred")
	}

	got, err := svc.Decrypt(context.Background(), tenantA, c.ID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("Decrypt = %q, want %q", got, secret)
	}
}

func TestPG_CrossTenantGetBlocked(t *testing.T) {
	svc, _ := newPGService(t)

	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "isolated", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("x"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = svc.Decrypt(context.Background(), tenantB, c.ID)
	if !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("cross-tenant Decrypt: err = %v, want ErrNotFound", err)
	}
}

func TestPG_NameConflict(t *testing.T) {
	svc, _ := newPGService(t)

	if _, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "clash", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("a"),
	}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "clash", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("b"),
	})
	if !errors.Is(err, credential.ErrNameConflict) {
		t.Errorf("second Create: err = %v, want ErrNameConflict", err)
	}
}

// ---------------------------------------------------------------- KEK rotation ---

func TestPG_KEKRotation_CredentialsStillDecryptable(t *testing.T) {
	svc, _ := newPGService(t)
	secret := []byte("rotate-me-please-xxxxxxxxxxxxxxx")

	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "rotate-key", AuthType: credential.AuthTypeBearer, Source: credential.SourceManaged, Plaintext: secret,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.RotateKEK(context.Background(), tenantA); err != nil {
		t.Fatalf("RotateKEK: %v", err)
	}

	got, err := svc.Decrypt(context.Background(), tenantA, c.ID)
	if err != nil {
		t.Fatalf("Decrypt after rotation: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("Decrypt after rotation = %q, want %q", got, secret)
	}
}

// ----------------------------------- delete: not found + referenced + success ---

func TestPG_Delete_NotFound(t *testing.T) {
	_, pool := newPGService(t)
	store := credential.NewPostgresStore(pool)

	err := store.Delete(context.Background(), tenantA, "cred_nonexistent")
	if !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Delete nonexistent: err = %v, want ErrNotFound", err)
	}
}

func TestPG_Delete_Referenced(t *testing.T) {
	svc, pool := newPGService(t)

	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "ref-key", AuthType: credential.AuthTypeBearer, Source: credential.SourceManaged, Plaintext: []byte("s"),
	})
	if err != nil {
		t.Fatalf("Create credential: %v", err)
	}

	// Create an agent row that references this credential via credential_ref FK.
	if _, err := pool.Exec(
		context.Background(),
		`INSERT INTO agents (id, tenant_id, name, status, credential_ref)
		 VALUES ('agt_ref-test-001', $1, 'ref-agent', 'active', $2)`,
		tenantA, c.ID,
	); err != nil {
		t.Fatalf("insert agent with credential_ref: %v", err)
	}

	store := credential.NewPostgresStore(pool)
	err = store.Delete(context.Background(), tenantA, c.ID)
	if !errors.Is(err, credential.ErrReferenced) {
		t.Errorf("Delete referenced credential: err = %v, want ErrReferenced", err)
	}
}

func TestPG_Delete_Success(t *testing.T) {
	svc, pool := newPGService(t)

	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "deleteme", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("x"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	store := credential.NewPostgresStore(pool)
	if err := store.Delete(context.Background(), tenantA, c.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Credential must be gone.
	if _, err := store.Get(context.Background(), tenantA, c.ID); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
	}
}

// ----------------------------------------------------------------- WrapDEK ---

func TestPG_WrapDEK_UpdatesOnlyDEKColumns(t *testing.T) {
	svc, pool := newPGService(t)
	secret := []byte("stays-the-same-xxxxxxxxxxxxxxxxx")

	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "wrap-dek-test", AuthType: credential.AuthTypeBearer, Source: credential.SourceManaged, Plaintext: secret,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	originalEncSecret := append([]byte(nil), c.EncryptedSecret...)

	// WrapDEK with dummy bytes to verify it doesn't touch encrypted_secret.
	store := credential.NewPostgresStore(pool)
	fakeDEK := make([]byte, 44) // plausible ciphertext length
	for i := range fakeDEK {
		fakeDEK[i] = byte(i)
	}
	if err := store.WrapDEK(context.Background(), tenantA, c.ID, fakeDEK, "test-version"); err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}

	updated, err := store.Get(context.Background(), tenantA, c.ID)
	if err != nil {
		t.Fatalf("Get after WrapDEK: %v", err)
	}
	if updated.KEKVersion != "test-version" {
		t.Errorf("KEKVersion = %q, want %q", updated.KEKVersion, "test-version")
	}
	// encrypted_secret must be unchanged.
	if !bytes.Equal(updated.EncryptedSecret, originalEncSecret) {
		t.Error("WrapDEK modified encrypted_secret (must only touch encrypted_dek)")
	}
}

// ------------------------------------------------------------------ Update ---

func TestPG_Update_NotFound(t *testing.T) {
	svc, pool := newPGService(t)
	store := credential.NewPostgresStore(pool)

	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "owned", AuthType: credential.AuthTypeBearer, Source: credential.SourceManaged, Plaintext: []byte("s3cret"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	name := "whatever"
	if _, err := store.Update(context.Background(), tenantA, "cred_missing", credential.UpdateFields{Name: &name}); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Update unknown id: err = %v, want ErrNotFound", err)
	}
	// Real id, wrong tenant — must be ErrNotFound (no cross-tenant existence leak).
	if _, err := store.Update(context.Background(), tenantB, c.ID, credential.UpdateFields{Name: &name}); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Update cross-tenant id: err = %v, want ErrNotFound", err)
	}
}

func TestPG_Update_NameConflict(t *testing.T) {
	svc, pool := newPGService(t)
	store := credential.NewPostgresStore(pool)

	if _, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "taken", AuthType: credential.AuthTypeBearer, Source: credential.SourceManaged, Plaintext: []byte("a"),
	}); err != nil {
		t.Fatalf("Create taken: %v", err)
	}
	mine, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "mine", AuthType: credential.AuthTypeBearer, Source: credential.SourceManaged, Plaintext: []byte("b"),
	})
	if err != nil {
		t.Fatalf("Create mine: %v", err)
	}
	taken := "taken"
	if _, err := store.Update(context.Background(), tenantA, mine.ID, credential.UpdateFields{Name: &taken}); !errors.Is(err, credential.ErrNameConflict) {
		t.Errorf("Update to a taken name: err = %v, want ErrNameConflict", err)
	}
}
