package settlement_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/settlement"
)

const (
	tenantA = "tenant-alpha"
	tenantB = "tenant-beta"
)

func ptr(s string) *string { return &s }

func TestMemoryStore_GetAbsentReturnsNotFound(t *testing.T) {
	t.Parallel()

	s := settlement.NewMemoryStore()
	if _, err := s.Get(context.Background(), tenantA); !errors.Is(err, settlement.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_UpsertCreatesOnFirstConfigure(t *testing.T) {
	t.Parallel()

	s := settlement.NewMemoryStore()
	cfg, err := s.Upsert(context.Background(), tenantA, settlement.UpdateFields{
		FacilitatorURL:           ptr("https://settle.example.com"),
		FacilitatorCredentialRef: ptr("cred_123"),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if cfg.TenantID != tenantA {
		t.Errorf("TenantID = %q, want %q", cfg.TenantID, tenantA)
	}
	if cfg.FacilitatorURL != "https://settle.example.com" {
		t.Errorf("FacilitatorURL = %q", cfg.FacilitatorURL)
	}
	if cfg.FacilitatorCredentialRef != "cred_123" {
		t.Errorf("FacilitatorCredentialRef = %q", cfg.FacilitatorCredentialRef)
	}
	if cfg.CreatedAt.IsZero() || cfg.UpdatedAt.IsZero() {
		t.Error("CreatedAt/UpdatedAt not set")
	}

	got, err := s.Get(context.Background(), tenantA)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FacilitatorURL != cfg.FacilitatorURL || got.FacilitatorCredentialRef != cfg.FacilitatorCredentialRef {
		t.Errorf("Get() = %+v, want match with created %+v", got, cfg)
	}
}

func TestMemoryStore_UpsertFirstConfigureRequiresBothFields(t *testing.T) {
	t.Parallel()

	s := settlement.NewMemoryStore()

	if _, err := s.Upsert(context.Background(), tenantA, settlement.UpdateFields{
		FacilitatorURL: ptr("https://settle.example.com"),
	}); !errors.Is(err, settlement.ErrIncomplete) {
		t.Errorf("Upsert() error = %v, want ErrIncomplete (missing credential_ref)", err)
	}

	if _, err := s.Upsert(context.Background(), tenantA, settlement.UpdateFields{
		FacilitatorCredentialRef: ptr("cred_123"),
	}); !errors.Is(err, settlement.ErrIncomplete) {
		t.Errorf("Upsert() error = %v, want ErrIncomplete (missing facilitator_url)", err)
	}

	if _, err := s.Get(context.Background(), tenantA); !errors.Is(err, settlement.ErrNotFound) {
		t.Errorf("Get() after failed Upsert error = %v, want ErrNotFound (no partial row created)", err)
	}
}

func TestMemoryStore_UpsertPartialUpdateLeavesOtherFieldUnchanged(t *testing.T) {
	t.Parallel()

	s := settlement.NewMemoryStore()
	if _, err := s.Upsert(context.Background(), tenantA, settlement.UpdateFields{
		FacilitatorURL:           ptr("https://settle.example.com"),
		FacilitatorCredentialRef: ptr("cred_123"),
	}); err != nil {
		t.Fatalf("Upsert (create): %v", err)
	}

	updated, err := s.Upsert(context.Background(), tenantA, settlement.UpdateFields{
		FacilitatorCredentialRef: ptr("cred_456"),
	})
	if err != nil {
		t.Fatalf("Upsert (partial): %v", err)
	}
	if updated.FacilitatorURL != "https://settle.example.com" {
		t.Errorf("FacilitatorURL changed unexpectedly: %q", updated.FacilitatorURL)
	}
	if updated.FacilitatorCredentialRef != "cred_456" {
		t.Errorf("FacilitatorCredentialRef = %q, want %q", updated.FacilitatorCredentialRef, "cred_456")
	}
}

func TestMemoryStore_TenantIsolation(t *testing.T) {
	t.Parallel()

	s := settlement.NewMemoryStore()
	if _, err := s.Upsert(context.Background(), tenantA, settlement.UpdateFields{
		FacilitatorURL:           ptr("https://settle.example.com"),
		FacilitatorCredentialRef: ptr("cred_123"),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if _, err := s.Get(context.Background(), tenantB); !errors.Is(err, settlement.ErrNotFound) {
		t.Errorf("Get(tenantB) error = %v, want ErrNotFound", err)
	}
}
