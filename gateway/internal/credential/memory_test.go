package credential_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/credential"
	"github.com/zerkerlabs/gateway/gateway/internal/kms"
)

const (
	tenantA = "tenant-alpha"
	tenantB = "tenant-beta"
)

func newService(t *testing.T) *credential.Service {
	t.Helper()
	provider, err := kms.NewLocalProvider()
	if err != nil {
		t.Fatalf("NewLocalProvider: %v", err)
	}
	return credential.NewService(
		credential.NewMemoryStore(),
		credential.NewMemoryKEKStore(),
		provider,
		credential.StubVaultResolver{},
	)
}

// ------------------------------------------------- managed create + decrypt ---

func TestService_CreateManaged_AssignsServerFields(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name:      "my-key",
		AuthType:  credential.AuthTypeBearer,
		Source:    credential.SourceManaged,
		Plaintext: []byte("s3cr3t"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(c.ID, "cred_") {
		t.Errorf("ID %q does not start with cred_", c.ID)
	}
	if c.TenantID != tenantA {
		t.Errorf("TenantID = %q, want %q", c.TenantID, tenantA)
	}
	if c.Version != 1 {
		t.Errorf("Version = %d, want 1", c.Version)
	}
	if c.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

func TestService_CreateManaged_DecryptRoundtrip(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	plaintext := []byte("sk-super-secret-api-key-12345678")

	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name:      "roundtrip-key",
		AuthType:  credential.AuthTypeBearer,
		Source:    credential.SourceManaged,
		Plaintext: plaintext,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Ciphertext must not equal plaintext.
	if bytes.Equal(c.EncryptedSecret, plaintext) {
		t.Error("EncryptedSecret equals plaintext — no encryption occurred")
	}

	// Masked hint must show last 4 chars.
	if c.MaskedHint != "...5678" {
		t.Errorf("MaskedHint = %q, want %q", c.MaskedHint, "...5678")
	}

	// VaultRef must be empty for managed source.
	if c.VaultRef != "" {
		t.Errorf("VaultRef = %q, want empty for managed source", c.VaultRef)
	}

	got, err := svc.Decrypt(context.Background(), tenantA, c.ID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Decrypt = %q, want %q", got, plaintext)
	}
}

func TestService_CreateManaged_ShortPlaintext(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	plaintext := []byte("ab")
	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name:      "short",
		AuthType:  credential.AuthTypeNone,
		Source:    credential.SourceManaged,
		Plaintext: plaintext,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Short plaintext: masked hint is fully starred.
	if c.MaskedHint != "**" {
		t.Errorf("MaskedHint = %q, want %q", c.MaskedHint, "**")
	}
	got, err := svc.Decrypt(context.Background(), tenantA, c.ID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Decrypt = %q, want %q", got, plaintext)
	}
}

// ------------------------------------------------------ vault create + resolve ---

func TestService_CreateVault_StubReturnsError(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name:     "ext-key",
		AuthType: credential.AuthTypeBearer,
		Source:   credential.SourceVault,
		VaultRef: "secret/data/my-key",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.VaultRef != "secret/data/my-key" {
		t.Errorf("VaultRef = %q, want %q", c.VaultRef, "secret/data/my-key")
	}
	if len(c.EncryptedSecret) != 0 {
		t.Error("EncryptedSecret should be empty for vault source")
	}

	// The stub vault resolver always returns ErrVaultNotConfigured.
	_, err = svc.Decrypt(context.Background(), tenantA, c.ID)
	if !errors.Is(err, credential.ErrVaultNotConfigured) {
		t.Errorf("Decrypt vault credential: err = %v, want ErrVaultNotConfigured", err)
	}
}

// ----------------------------------------------- name conflict + cross-tenant ---

func TestStore_NameConflictSameTenant(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	_, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "clash", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("x"),
	})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err = svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "clash", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("y"),
	})
	if !errors.Is(err, credential.ErrNameConflict) {
		t.Errorf("second Create: err = %v, want ErrNameConflict", err)
	}
}

func TestStore_SameNameDifferentTenants(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	if _, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "shared", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("a"),
	}); err != nil {
		t.Fatalf("Create tenantA: %v", err)
	}
	// Same name in a different tenant must succeed.
	if _, err := svc.Create(context.Background(), tenantB, credential.CreateParams{
		Name: "shared", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("b"),
	}); err != nil {
		t.Errorf("Create tenantB with same name: %v", err)
	}
}

func TestStore_GetNotFound(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	_, err := svc.Decrypt(context.Background(), tenantA, "cred_nonexistent")
	if !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Decrypt nonexistent: err = %v, want ErrNotFound", err)
	}
}

func TestStore_CrossTenantGetBlocked(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "mine", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("s"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Tenant B must not be able to decrypt tenant A's credential.
	_, err = svc.Decrypt(context.Background(), tenantB, c.ID)
	if !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("cross-tenant Decrypt: err = %v, want ErrNotFound", err)
	}
}

// -------------------------------------------------------------------- List ---

func TestStore_List_CrossTenantIsolation(t *testing.T) {
	t.Parallel()

	// Use raw stores to test List directly.
	store := credential.NewMemoryStore()
	kekStore := credential.NewMemoryKEKStore()
	provider, err := kms.NewLocalProvider()
	if err != nil {
		t.Fatalf("NewLocalProvider: %v", err)
	}
	svc := credential.NewService(store, kekStore, provider, credential.StubVaultResolver{})

	if _, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "a-key", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("a"),
	}); err != nil {
		t.Fatalf("Create tenantA: %v", err)
	}
	if _, err := svc.Create(context.Background(), tenantB, credential.CreateParams{
		Name: "b-key", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("b"),
	}); err != nil {
		t.Fatalf("Create tenantB: %v", err)
	}

	listA, err := store.List(context.Background(), tenantA)
	if err != nil {
		t.Fatalf("List(tenantA): %v", err)
	}
	if len(listA) != 1 || listA[0].Name != "a-key" {
		t.Errorf("List(tenantA): got %d items, want 1 named a-key", len(listA))
	}

	listB, err := store.List(context.Background(), tenantB)
	if err != nil {
		t.Fatalf("List(tenantB): %v", err)
	}
	if len(listB) != 1 || listB[0].Name != "b-key" {
		t.Errorf("List(tenantB): got %d items, want 1 named b-key", len(listB))
	}
}

// ------------------------------------------------------------------ Delete ---

func TestStore_Delete_NotFound(t *testing.T) {
	t.Parallel()

	store := credential.NewMemoryStore()
	err := store.Delete(context.Background(), tenantA, "cred_ghost")
	if !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Delete nonexistent: err = %v, want ErrNotFound", err)
	}
}

func TestStore_Delete_CrossTenantBlocked(t *testing.T) {
	t.Parallel()

	store := credential.NewMemoryStore()
	kekStore := credential.NewMemoryKEKStore()
	provider, err := kms.NewLocalProvider()
	if err != nil {
		t.Fatalf("NewLocalProvider: %v", err)
	}
	svc := credential.NewService(store, kekStore, provider, credential.StubVaultResolver{})

	c, createErr := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "protected", AuthType: credential.AuthTypeNone, Source: credential.SourceManaged, Plaintext: []byte("s"),
	})
	if createErr != nil {
		t.Fatalf("Create: %v", createErr)
	}
	if err := store.Delete(context.Background(), tenantB, c.ID); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("cross-tenant Delete: err = %v, want ErrNotFound", err)
	}
	// Original must still be retrievable.
	got, decErr := svc.Decrypt(context.Background(), tenantA, c.ID)
	if decErr != nil {
		t.Fatalf("Decrypt after blocked delete: %v", decErr)
	}
	if !bytes.Equal(got, []byte("s")) {
		t.Errorf("plaintext after blocked delete = %q, want %q", got, "s")
	}
}

// -------------------------------------------------------------- KEK rotation ---

func TestService_KEKRotation_CredentialsStillDecryptable(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	secret := []byte("before-rotation-secret-value-xyz")

	c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name:      "rotateme",
		AuthType:  credential.AuthTypeBearer,
		Source:    credential.SourceManaged,
		Plaintext: secret,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify decryption works before rotation.
	got, err := svc.Decrypt(context.Background(), tenantA, c.ID)
	if err != nil {
		t.Fatalf("Decrypt before rotation: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("Decrypt before rotation = %q, want %q", got, secret)
	}

	// Rotate the KEK.
	if err := svc.RotateKEK(context.Background(), tenantA); err != nil {
		t.Fatalf("RotateKEK: %v", err)
	}

	// Verify decryption still works after rotation.
	got, err = svc.Decrypt(context.Background(), tenantA, c.ID)
	if err != nil {
		t.Fatalf("Decrypt after rotation: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("Decrypt after rotation = %q, want %q", got, secret)
	}
}

func TestService_KEKRotation_MultipleCredentials(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	secrets := [][]byte{
		[]byte("secret-one-xxxxxxxxxxxxxxxxxxx"),
		[]byte("secret-two-xxxxxxxxxxxxxxxxxxx"),
		[]byte("secret-three-xxxxxxxxxxxxxxxxx"),
	}

	var ids []string
	for i, s := range secrets {
		c, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
			Name:      strings.Repeat("k", i+1),
			AuthType:  credential.AuthTypeAPIKey,
			Source:    credential.SourceManaged,
			Plaintext: s,
		})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, c.ID)
	}

	if err := svc.RotateKEK(context.Background(), tenantA); err != nil {
		t.Fatalf("RotateKEK: %v", err)
	}

	for i, id := range ids {
		got, err := svc.Decrypt(context.Background(), tenantA, id)
		if err != nil {
			t.Errorf("Decrypt[%d] after rotation: %v", i, err)
			continue
		}
		if !bytes.Equal(got, secrets[i]) {
			t.Errorf("Decrypt[%d] after rotation = %q, want %q", i, got, secrets[i])
		}
	}
}

func TestService_KEKRotation_DoesNotAffectOtherTenants(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	secretA := []byte("tenant-a-secret-xxxxxxxxxxx")
	secretB := []byte("tenant-b-secret-xxxxxxxxxxx")

	cA, err := svc.Create(context.Background(), tenantA, credential.CreateParams{
		Name: "key-a", AuthType: credential.AuthTypeBearer, Source: credential.SourceManaged, Plaintext: secretA,
	})
	if err != nil {
		t.Fatalf("Create tenantA: %v", err)
	}
	cB, err := svc.Create(context.Background(), tenantB, credential.CreateParams{
		Name: "key-b", AuthType: credential.AuthTypeBearer, Source: credential.SourceManaged, Plaintext: secretB,
	})
	if err != nil {
		t.Fatalf("Create tenantB: %v", err)
	}

	// Rotate only tenant A's KEK.
	if err := svc.RotateKEK(context.Background(), tenantA); err != nil {
		t.Fatalf("RotateKEK: %v", err)
	}

	// Both tenants' credentials must still decrypt.
	gotA, err := svc.Decrypt(context.Background(), tenantA, cA.ID)
	if err != nil {
		t.Fatalf("Decrypt tenantA after rotation: %v", err)
	}
	if !bytes.Equal(gotA, secretA) {
		t.Errorf("Decrypt tenantA = %q, want %q", gotA, secretA)
	}
	gotB, err := svc.Decrypt(context.Background(), tenantB, cB.ID)
	if err != nil {
		t.Fatalf("Decrypt tenantB after rotation: %v", err)
	}
	if !bytes.Equal(gotB, secretB) {
		t.Errorf("Decrypt tenantB = %q, want %q", gotB, secretB)
	}
}

// ------------------------------------------------------------------ Update ---

// seedMemCred creates a minimal managed credential directly in the store and
// returns its ID, for exercising Store.Update without the encryption path.
func seedMemCred(t *testing.T, s *credential.MemoryStore, name string) string {
	t.Helper()
	c := &credential.Credential{Name: name, AuthType: credential.AuthTypeBearer, Source: credential.SourceManaged}
	if err := s.Create(context.Background(), tenantA, c); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return c.ID
}

func TestStore_Update_NameAndAuthType(t *testing.T) {
	t.Parallel()
	s := credential.NewMemoryStore()
	id := seedMemCred(t, s, "old-name")

	newName, newAuth := "new-name", credential.AuthTypeAPIKey
	updated, err := s.Update(context.Background(), tenantA, id, credential.UpdateFields{
		Name:     &newName,
		AuthType: &newAuth,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != newName || updated.AuthType != newAuth {
		t.Errorf("updated = {%q, %q}, want {%q, %q}", updated.Name, updated.AuthType, newName, newAuth)
	}
	// Confirm it persisted, not just the returned struct.
	got, err := s.Get(context.Background(), tenantA, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != newName || got.AuthType != newAuth {
		t.Errorf("after Get = {%q, %q}, want {%q, %q}", got.Name, got.AuthType, newName, newAuth)
	}
}

func TestStore_Update_NotFound(t *testing.T) {
	t.Parallel()
	s := credential.NewMemoryStore()
	id := seedMemCred(t, s, "owned")
	name := "whatever"

	if _, err := s.Update(context.Background(), tenantA, "cred_missing", credential.UpdateFields{Name: &name}); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Update unknown id: err = %v, want ErrNotFound", err)
	}
	// A real id, but the wrong tenant, must also be ErrNotFound (no cross-tenant existence leak).
	if _, err := s.Update(context.Background(), tenantB, id, credential.UpdateFields{Name: &name}); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Update cross-tenant id: err = %v, want ErrNotFound", err)
	}
}

func TestStore_Update_NameConflict(t *testing.T) {
	t.Parallel()
	s := credential.NewMemoryStore()
	seedMemCred(t, s, "taken")
	id := seedMemCred(t, s, "mine")

	taken := "taken"
	if _, err := s.Update(context.Background(), tenantA, id, credential.UpdateFields{Name: &taken}); !errors.Is(err, credential.ErrNameConflict) {
		t.Errorf("Update to a taken name: err = %v, want ErrNameConflict", err)
	}
}
