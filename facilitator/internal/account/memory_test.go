package account_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zerkerlabs/farcaster/facilitator/internal/account"
)

func TestMemoryStore_LookupNotFound(t *testing.T) {
	t.Parallel()

	store := account.NewMemoryStore()
	_, err := store.Lookup(context.Background(), "deadbeef")
	if !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("Lookup() error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_CreateThenLookup(t *testing.T) {
	t.Parallel()

	store := account.NewMemoryStore()
	in := &account.Account{
		Name:            "acme-operator",
		CertFingerprint: "abc123",
		CertSubject:     "CN=acme-client",
		Active:          true,
	}
	if err := store.Create(context.Background(), in); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if in.ID == "" {
		t.Fatalf("Create did not assign an ID")
	}
	if in.CreatedAt.IsZero() || in.UpdatedAt.IsZero() {
		t.Fatalf("Create did not assign timestamps")
	}

	got, err := store.Lookup(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Name != "acme-operator" || got.CertFingerprint != "abc123" || !got.Active {
		t.Fatalf("Lookup returned %+v, want a match for the created account", got)
	}

	if _, err := store.Lookup(context.Background(), "not-a-known-fingerprint"); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("Lookup(unknown) error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_LookupReturnsInactiveAccounts(t *testing.T) {
	t.Parallel()

	// Lookup itself does not filter on Active — that's the middleware's job
	// (spec 0007: unknown vs. inactive are indistinguishable to the caller,
	// but the middleware needs the account record to tell them apart from
	// "no such fingerprint" internally / for logging).
	store := account.NewMemoryStore()
	in := &account.Account{CertFingerprint: "inactive-fp", Active: false}
	if err := store.Create(context.Background(), in); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Lookup(context.Background(), "inactive-fp")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Active {
		t.Fatalf("Lookup returned Active = true, want false")
	}
}
