package account

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore is a thread-safe, in-memory implementation of Store. It does
// not persist across restarts; intended for tests and local dev until a
// Postgres-backed Store lands (see Store's doc comment).
type MemoryStore struct {
	mu   sync.RWMutex
	byFP map[string]*Account // certFingerprint → account
}

// NewMemoryStore returns an empty MemoryStore ready for use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byFP: make(map[string]*Account)}
}

// Create implements Store.
func (s *MemoryStore) Create(ctx context.Context, a *Account) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	a.ID = "facacct_" + id.String()
	a.CreatedAt = now
	a.UpdatedAt = now
	s.byFP[a.CertFingerprint] = cloneAccount(a)
	return nil
}

// Lookup implements Store.
func (s *MemoryStore) Lookup(ctx context.Context, certFingerprint string) (*Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	a, ok := s.byFP[certFingerprint]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneAccount(a), nil
}

func cloneAccount(a *Account) *Account {
	cp := *a
	return &cp
}
