package policy

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is a thread-safe, tenant-scoped, in-memory implementation of
// PolicyStore. Intended for unit tests; it does not persist across restarts.
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]*Policy // tenantID → *Policy
}

// NewMemoryStore returns an empty MemoryStore ready for use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]*Policy)}
}

// Get implements PolicyStore.
func (s *MemoryStore) Get(ctx context.Context, tenantID string) (*Policy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.records[tenantID]
	if !ok {
		return nil, ErrNotFound
	}
	p := clonePolicy(rec)
	return p, nil
}

// Put implements PolicyStore.
func (s *MemoryStore) Put(ctx context.Context, tenantID string, fields PutFields) (*Policy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	rec, exists := s.records[tenantID]
	if !exists {
		rec = &Policy{
			TenantID:  tenantID,
			Version:   1,
			Default:   fields.Default,
			OnError:   fields.OnError,
			Rules:     cloneRules(fields.Rules),
			CreatedAt: now,
			UpdatedAt: now,
		}
		s.records[tenantID] = rec
		return clonePolicy(rec), nil
	}

	rec.Version++
	rec.Default = fields.Default
	rec.OnError = fields.OnError
	rec.Rules = cloneRules(fields.Rules)
	rec.UpdatedAt = now
	return clonePolicy(rec), nil
}

// cloneRules returns a deep copy of rules, including each Rule.Match's
// slice and pointer fields, so stored records and returned records never
// alias a caller's backing array.
func cloneRules(rules []Rule) []Rule {
	if rules == nil {
		return nil
	}
	out := make([]Rule, len(rules))
	for i, r := range rules {
		out[i] = r
		out[i].Match = cloneMatch(r.Match)
		if r.Classifier != nil {
			c := *r.Classifier
			out[i].Classifier = &c
		}
	}
	return out
}

// cloneMatch returns a deep copy of m's slice and pointer fields.
func cloneMatch(m Match) Match {
	c := m
	if m.Agents != nil {
		c.Agents = append([]string(nil), m.Agents...)
	}
	if m.Tools != nil {
		c.Tools = append([]string(nil), m.Tools...)
	}
	if m.MaxBodyBytes != nil {
		v := *m.MaxBodyBytes
		c.MaxBodyBytes = &v
	}
	if m.RatePerMin != nil {
		v := *m.RatePerMin
		c.RatePerMin = &v
	}
	return c
}

// clonePolicy returns a copy of p, including a copy of its Rules slice.
func clonePolicy(p *Policy) *Policy {
	c := *p
	c.Rules = cloneRules(p.Rules)
	return &c
}
