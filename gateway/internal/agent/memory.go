package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/resource"
)

// MemoryStore is a thread-safe, tenant-scoped, in-memory implementation of
// AgentStore. It is intended for unit tests; do not use in production.
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]map[string]*Agent // tenantID → id → *Agent
}

// NewMemoryStore returns an empty MemoryStore ready for use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		records: make(map[string]map[string]*Agent),
	}
}

// Create implements AgentStore.
func (s *MemoryStore) Create(ctx context.Context, tenantID string, a *Agent) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	id, err := resource.New("agt")
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	bucket := s.bucket(tenantID)
	for _, existing := range bucket {
		if existing.Status != StatusInactive && existing.Name == a.Name {
			return ErrNameConflict
		}
	}

	now := time.Now().UTC()
	a.ID = id
	a.TenantID = tenantID
	a.Status = statusFromUpstreamURL(a.UpstreamURL)
	a.Protocol = normalizeProtocol(a.Protocol)
	a.CreatedAt = now
	a.UpdatedAt = now

	bucket[id] = cloneAgent(a)
	return nil
}

// Get implements AgentStore.
func (s *MemoryStore) Get(ctx context.Context, tenantID, id string) (*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, err := s.find(tenantID, id)
	if err != nil {
		return nil, err
	}
	return cloneAgent(rec), nil
}

// List implements AgentStore.
func (s *MemoryStore) List(ctx context.Context, tenantID string, page, perPage int, protocol string) ([]*Agent, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 50
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var active []*Agent
	for _, rec := range s.records[tenantID] {
		if rec.Status != StatusInactive && (protocol == "" || rec.Protocol == protocol) {
			active = append(active, cloneAgent(rec))
		}
	}

	// Sort by created_at ascending, then by ID for deterministic ordering.
	slices.SortFunc(active, func(a, b *Agent) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})

	total := len(active)
	start := (page - 1) * perPage
	if start >= total {
		return []*Agent{}, total, nil
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return active[start:end], total, nil
}

// Update implements AgentStore.
func (s *MemoryStore) Update(ctx context.Context, tenantID, id string, fields UpdateFields) (*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.find(tenantID, id)
	if err != nil {
		return nil, err
	}
	// A soft-deleted agent is treated as gone: the storage layer is the
	// enforcement point, so handlers can rely on it rather than re-checking.
	if rec.Status == StatusInactive {
		return nil, ErrNotFound
	}

	if fields.Name != nil && *fields.Name != rec.Name {
		for _, existing := range s.records[tenantID] {
			if existing.ID != id && existing.Status != StatusInactive && existing.Name == *fields.Name {
				return nil, ErrNameConflict
			}
		}
		rec.Name = *fields.Name
	}
	if fields.Description != nil {
		rec.Description = *fields.Description
	}
	if fields.Tags != nil {
		tags := make([]string, len(*fields.Tags))
		copy(tags, *fields.Tags)
		rec.Tags = tags
	}
	if fields.Metadata != nil {
		rec.Metadata = copyMetadata(*fields.Metadata)
	}
	if fields.UpstreamURL != nil {
		rec.UpstreamURL = *fields.UpstreamURL
	}
	if fields.CredentialRef != nil {
		rec.CredentialRef = *fields.CredentialRef
	}
	if fields.CaptureBody != nil {
		rec.CaptureBody = *fields.CaptureBody
	}
	if fields.EmitReceipts != nil {
		rec.EmitReceipts = *fields.EmitReceipts
	}
	if fields.Suspended != nil {
		rec.Suspended = *fields.Suspended
	}
	if fields.InvocationRateLimit != nil {
		rec.InvocationRateLimit = *fields.InvocationRateLimit
	}
	if fields.InvocationBurst != nil {
		rec.InvocationBurst = *fields.InvocationBurst
	}
	if fields.Protocol != nil {
		rec.Protocol = normalizeProtocol(*fields.Protocol)
	}
	if fields.MCPTransport != nil {
		rec.MCPTransport = *fields.MCPTransport
	}
	if fields.MCPProtocolVersion != nil {
		rec.MCPProtocolVersion = *fields.MCPProtocolVersion
	}
	if fields.Pricing != nil {
		rec.Pricing = clonePricing(*fields.Pricing)
	}
	rec.Status = statusFromUpstreamURL(rec.UpstreamURL)
	rec.UpdatedAt = time.Now().UTC()

	return cloneAgent(rec), nil
}

// Delete implements AgentStore.
func (s *MemoryStore) Delete(ctx context.Context, tenantID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.find(tenantID, id)
	if err != nil {
		return err
	}
	if rec.Status == StatusInactive {
		return ErrNotFound
	}
	rec.Status = StatusInactive
	rec.UpdatedAt = time.Now().UTC()
	return nil
}

// bucket returns (and lazily initialises) the per-tenant record map. Caller
// must hold s.mu (write lock). Read paths (List/find) intentionally access
// s.records[tenantID] directly: a nil map reads as empty in Go, so they need no
// allocation and can run under the read lock.
func (s *MemoryStore) bucket(tenantID string) map[string]*Agent {
	if s.records[tenantID] == nil {
		s.records[tenantID] = make(map[string]*Agent)
	}
	return s.records[tenantID]
}

// find looks up a record by tenant and ID. Caller must hold s.mu (any lock).
func (s *MemoryStore) find(tenantID, id string) (*Agent, error) {
	bucket, ok := s.records[tenantID]
	if !ok {
		return nil, ErrNotFound
	}
	rec, ok := bucket[id]
	if !ok {
		return nil, ErrNotFound
	}
	return rec, nil
}

func cloneAgent(a *Agent) *Agent {
	c := *a
	if a.Tags != nil {
		c.Tags = make([]string, len(a.Tags))
		copy(c.Tags, a.Tags)
	}
	if a.Metadata != nil {
		c.Metadata = copyMetadata(a.Metadata)
	}
	if a.InvocationRateLimit != nil {
		v := *a.InvocationRateLimit
		c.InvocationRateLimit = &v
	}
	if a.InvocationBurst != nil {
		v := *a.InvocationBurst
		c.InvocationBurst = &v
	}
	c.Pricing = clonePricing(a.Pricing)
	return &c
}

// clonePricing returns a deep copy of p (including its Tools map) so callers
// cannot mutate stored records through a shared pointer or map reference. It
// returns nil for a nil input, preserving the "unpriced" sentinel.
func clonePricing(p *Pricing) *Pricing {
	if p == nil {
		return nil
	}
	c := *p
	if p.Tools != nil {
		c.Tools = make(map[string]string, len(p.Tools))
		for k, v := range p.Tools {
			c.Tools[k] = v
		}
	}
	return &c
}

// copyMetadata returns a deep copy of m so callers cannot mutate stored records
// (or vice versa) through shared nested map/slice references.
func copyMetadata(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

// deepCopyValue recursively copies the JSON-shaped values that appear in agent
// metadata (objects and arrays); scalars are returned as-is.
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return copyMetadata(t)
	case []any:
		cp := make([]any, len(t))
		for i, e := range t {
			cp[i] = deepCopyValue(e)
		}
		return cp
	default:
		return v
	}
}
