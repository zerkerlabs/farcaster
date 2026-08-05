package policy

import (
	"context"
	"sync"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/resource"
)

// MemoryDecisionStore is a thread-safe, tenant-scoped, in-memory
// DecisionStore. Intended for unit tests and the in-memory dev server; it does
// not persist across restarts.
type MemoryDecisionStore struct {
	mu sync.RWMutex
	// records holds each tenant's decisions in insertion order (oldest first),
	// so ListRecent can walk from the end for most-recent-first without relying
	// on timestamp resolution to order decisions recorded in the same instant.
	records map[string][]*StoredDecision
}

// NewMemoryDecisionStore returns an empty MemoryDecisionStore ready for use.
func NewMemoryDecisionStore() *MemoryDecisionStore {
	return &MemoryDecisionStore{records: make(map[string][]*StoredDecision)}
}

// Insert implements DecisionStore.
func (s *MemoryDecisionStore) Insert(ctx context.Context, d RecordedDecision) (*StoredDecision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := resource.New(decisionIDPrefix)
	if err != nil {
		return nil, err
	}

	rec := storedFromRecorded(id, d, time.Now().UTC())

	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[d.TenantID] = append(s.records[d.TenantID], rec)
	return cloneStoredDecision(rec), nil
}

// ListRecent implements DecisionStore.
func (s *MemoryDecisionStore) ListRecent(ctx context.Context, tenantID string, limit int) ([]*StoredDecision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = decisionDefaultLimit
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	all := s.records[tenantID]
	out := make([]*StoredDecision, 0, min(limit, len(all)))
	// Walk newest → oldest (records is oldest-first), capped at limit.
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, cloneStoredDecision(all[i]))
	}
	return out, nil
}

// cloneStoredDecision returns a copy of d, including its MCPTool pointer, so a
// stored record never aliases a caller's memory.
func cloneStoredDecision(d *StoredDecision) *StoredDecision {
	c := *d
	if d.MCPTool != nil {
		tool := *d.MCPTool
		c.MCPTool = &tool
	}
	return &c
}
