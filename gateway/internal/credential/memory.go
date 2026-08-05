package credential

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/resource"
)

// MemoryStore is a thread-safe, tenant-scoped, in-memory implementation of
// Store. Intended for unit tests; it does not persist across restarts and does
// not enforce the FK constraint on agents.credential_ref (Postgres does that
// via the DB constraint in migration 005).
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]map[string]*Credential // tenantID → id → *Credential
}

// NewMemoryStore returns an empty MemoryStore ready for use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]map[string]*Credential)}
}

// Create implements Store.
func (s *MemoryStore) Create(ctx context.Context, tenantID string, c *Credential) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := resource.New("cred")
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	bucket := s.bucket(tenantID)
	for _, existing := range bucket {
		if existing.Name == c.Name {
			return ErrNameConflict
		}
	}

	now := time.Now().UTC()
	c.ID = id
	c.TenantID = tenantID
	c.Version = 1
	c.CreatedAt = now
	c.UpdatedAt = now

	bucket[id] = cloneCredential(c)
	return nil
}

// Get implements Store.
func (s *MemoryStore) Get(ctx context.Context, tenantID, id string) (*Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	c, err := s.find(tenantID, id)
	if err != nil {
		return nil, err
	}
	return cloneCredential(c), nil
}

// List implements Store.
func (s *MemoryStore) List(ctx context.Context, tenantID string) ([]*Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var creds []*Credential
	for _, c := range s.records[tenantID] {
		creds = append(creds, cloneCredential(c))
	}
	slices.SortFunc(creds, func(a, b *Credential) int {
		if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	if creds == nil {
		creds = []*Credential{}
	}
	return creds, nil
}

// Update implements Store.
func (s *MemoryStore) Update(ctx context.Context, tenantID, id string, fields UpdateFields) (*Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.find(tenantID, id)
	if err != nil {
		return nil, err
	}
	if fields.Name != nil {
		// Mirror Create's tenant-scoped name-uniqueness so the in-memory store
		// can't silently accept a conflicting rename that Postgres would reject.
		if *fields.Name != c.Name {
			for otherID, existing := range s.records[tenantID] {
				if otherID != id && existing.Name == *fields.Name {
					return nil, ErrNameConflict
				}
			}
		}
		c.Name = *fields.Name
	}
	if fields.AuthType != nil {
		c.AuthType = *fields.AuthType
	}
	if fields.EncryptedDEK != nil {
		c.EncryptedDEK = append([]byte(nil), fields.EncryptedDEK...)
	}
	if fields.KEKVersion != nil {
		c.KEKVersion = *fields.KEKVersion
	}
	if fields.EncryptedSecret != nil {
		c.EncryptedSecret = append([]byte(nil), fields.EncryptedSecret...)
	}
	if fields.MaskedHint != nil {
		c.MaskedHint = *fields.MaskedHint
	}
	if fields.VaultRef != nil {
		c.VaultRef = *fields.VaultRef
	}
	c.Version++
	c.UpdatedAt = time.Now().UTC()
	return cloneCredential(c), nil
}

// Delete implements Store. The in-memory store does not enforce the
// agents.credential_ref FK — Postgres enforces that via the DB constraint.
// Non-test code should always use the Postgres store in production.
func (s *MemoryStore) Delete(ctx context.Context, tenantID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.find(tenantID, id); err != nil {
		return err
	}
	delete(s.records[tenantID], id)
	return nil
}

// WrapDEK implements Store.
func (s *MemoryStore) WrapDEK(ctx context.Context, tenantID, id string, encryptedDEK []byte, kekVersion string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.find(tenantID, id)
	if err != nil {
		return err
	}
	c.EncryptedDEK = append([]byte(nil), encryptedDEK...)
	c.KEKVersion = kekVersion
	c.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *MemoryStore) bucket(tenantID string) map[string]*Credential {
	if s.records[tenantID] == nil {
		s.records[tenantID] = make(map[string]*Credential)
	}
	return s.records[tenantID]
}

func (s *MemoryStore) find(tenantID, id string) (*Credential, error) {
	bucket, ok := s.records[tenantID]
	if !ok {
		return nil, ErrNotFound
	}
	c, ok := bucket[id]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

func cloneCredential(c *Credential) *Credential {
	cp := *c
	if c.EncryptedDEK != nil {
		cp.EncryptedDEK = append([]byte(nil), c.EncryptedDEK...)
	}
	if c.EncryptedSecret != nil {
		cp.EncryptedSecret = append([]byte(nil), c.EncryptedSecret...)
	}
	return &cp
}

// MemoryKEKStore is a thread-safe, in-memory implementation of KEKStore.
// Intended for unit tests.
type MemoryKEKStore struct {
	mu      sync.RWMutex
	entries map[string][]kekEntry // tenantID → entries in insertion order
}

type kekEntry struct {
	version    string
	wrappedKEK []byte
}

// NewMemoryKEKStore returns an empty MemoryKEKStore ready for use.
func NewMemoryKEKStore() *MemoryKEKStore {
	return &MemoryKEKStore{entries: make(map[string][]kekEntry)}
}

// GetCurrent implements KEKStore.
func (k *MemoryKEKStore) GetCurrent(ctx context.Context, tenantID string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()

	entries := k.entries[tenantID]
	if len(entries) == 0 {
		return nil, "", ErrKEKNotFound
	}
	last := entries[len(entries)-1]
	return append([]byte(nil), last.wrappedKEK...), last.version, nil
}

// Get implements KEKStore.
func (k *MemoryKEKStore) Get(ctx context.Context, tenantID, version string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()

	for _, e := range k.entries[tenantID] {
		if e.version == version {
			return append([]byte(nil), e.wrappedKEK...), nil
		}
	}
	return nil, ErrKEKNotFound
}

// Put implements KEKStore.
func (k *MemoryKEKStore) Put(ctx context.Context, tenantID, version string, wrappedKEK []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()

	k.entries[tenantID] = append(k.entries[tenantID], kekEntry{
		version:    version,
		wrappedKEK: append([]byte(nil), wrappedKEK...),
	})
	return nil
}
