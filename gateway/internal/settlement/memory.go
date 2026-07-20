package settlement

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is a thread-safe, tenant-scoped, in-memory implementation of
// Store. Intended for unit tests; it does not persist across restarts and does
// not enforce the FK constraint on facilitator_credential_ref (Postgres does
// that via the DB constraint in migration 013).
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]*Config // tenantID → *Config
}

// NewMemoryStore returns an empty MemoryStore ready for use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]*Config)}
}

// Get implements Store.
func (s *MemoryStore) Get(ctx context.Context, tenantID string) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.records[tenantID]
	if !ok {
		return nil, ErrNotFound
	}
	c := *rec
	return &c, nil
}

// Upsert implements Store.
func (s *MemoryStore) Upsert(ctx context.Context, tenantID string, fields UpdateFields) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	rec, exists := s.records[tenantID]
	if !exists {
		var url, ref string
		if fields.FacilitatorURL != nil {
			url = *fields.FacilitatorURL
		}
		if fields.FacilitatorCredentialRef != nil {
			ref = *fields.FacilitatorCredentialRef
		}
		if url == "" || ref == "" {
			return nil, ErrIncomplete
		}
		rec = &Config{
			TenantID:                 tenantID,
			FacilitatorURL:           url,
			FacilitatorCredentialRef: ref,
			CreatedAt:                now,
			UpdatedAt:                now,
		}
		s.records[tenantID] = rec
		c := *rec
		return &c, nil
	}

	if fields.FacilitatorURL != nil {
		rec.FacilitatorURL = *fields.FacilitatorURL
	}
	if fields.FacilitatorCredentialRef != nil {
		rec.FacilitatorCredentialRef = *fields.FacilitatorCredentialRef
	}
	rec.UpdatedAt = now
	c := *rec
	return &c, nil
}
