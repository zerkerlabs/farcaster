package invocation

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/resource"
)

// MemoryStore is a thread-safe, tenant-scoped, in-memory implementation of
// Store. It is intended for unit tests; do not use in production.
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]map[string]*Invocation // tenantID → id → *Invocation
}

// NewMemoryStore returns an empty MemoryStore ready for use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		records: make(map[string]map[string]*Invocation),
	}
}

// Create implements Store.
func (s *MemoryStore) Create(ctx context.Context, tenantID string, inv *Invocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	id, err := resource.New("inv")
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	inv.ID = id
	inv.TenantID = tenantID
	inv.CreatedAt = now
	inv.UpdatedAt = now

	s.bucket(tenantID)[id] = cloneInvocation(inv)
	return nil
}

// Get implements Store.
func (s *MemoryStore) Get(ctx context.Context, tenantID, id string) (*Invocation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, err := s.find(tenantID, id)
	if err != nil {
		return nil, err
	}
	return cloneInvocation(rec), nil
}

// List implements Store.
func (s *MemoryStore) List(ctx context.Context, tenantID, agentID string, page, perPage int) ([]*Invocation, int, error) {
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

	var matching []*Invocation
	for _, rec := range s.records[tenantID] {
		if rec.AgentID == agentID {
			matching = append(matching, cloneInvocation(rec))
		}
	}

	// Most recent first; break ties by ID descending for determinism.
	slices.SortFunc(matching, func(a, b *Invocation) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})

	total := len(matching)
	start := (page - 1) * perPage
	if start >= total {
		return []*Invocation{}, total, nil
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return matching[start:end], total, nil
}

// ListFiltered implements Store.
func (s *MemoryStore) ListFiltered(ctx context.Context, tenantID string, filter ListFilter) ([]*Invocation, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	limit := filter.Limit
	if limit < 1 {
		limit = 20
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var matching []*Invocation
	for _, rec := range s.records[tenantID] {
		if filter.AgentID != "" && rec.AgentID != filter.AgentID {
			continue
		}
		if filter.Status != nil && rec.Status != *filter.Status {
			continue
		}
		if filter.Mode != nil && rec.Mode != *filter.Mode {
			continue
		}
		if filter.ErrorClass != nil {
			if rec.ErrorClass == nil || *rec.ErrorClass != *filter.ErrorClass {
				continue
			}
		}
		if filter.Model != "" {
			if rec.Model == nil || *rec.Model != filter.Model {
				continue
			}
		}
		if filter.SettlementStatus != nil {
			if rec.SettlementStatus == nil || *rec.SettlementStatus != *filter.SettlementStatus {
				continue
			}
		}
		if filter.Since != nil && rec.CreatedAt.Before(*filter.Since) {
			continue
		}
		if filter.Until != nil && rec.CreatedAt.After(*filter.Until) {
			continue
		}
		matching = append(matching, cloneInvocation(rec))
	}

	slices.SortFunc(matching, func(a, b *Invocation) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})

	total := len(matching)
	if offset >= total {
		return []*Invocation{}, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return matching[offset:end], total, nil
}

// Aggregate implements Store.
func (s *MemoryStore) Aggregate(ctx context.Context, tenantID string, q AggregateQuery) ([]AggregateGroup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Hold the read lock through aggregation: rows are live record pointers, and
	// aggregateRows only reads them, so concurrent readers are fine while writers
	// (which take the write lock) are excluded.
	s.mu.RLock()
	defer s.mu.RUnlock()

	var rows []*Invocation
	for _, rec := range s.records[tenantID] {
		if rec.CreatedAt.Before(q.Since) || rec.CreatedAt.After(q.Until) {
			continue
		}
		rows = append(rows, rec)
	}

	return aggregateRows(rows, q.Bucket), nil
}

// Update implements Store.
func (s *MemoryStore) Update(ctx context.Context, tenantID, id string, fields UpdateFields) (*Invocation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.find(tenantID, id)
	if err != nil {
		return nil, err
	}

	if fields.Status != nil {
		rec.Status = *fields.Status
	}
	if fields.CompletedAt != nil {
		t := *fields.CompletedAt
		rec.CompletedAt = &t
	}
	if fields.UpstreamStatus != nil {
		v := *fields.UpstreamStatus
		rec.UpstreamStatus = &v
	}
	if fields.LatencyMS != nil {
		v := *fields.LatencyMS
		rec.LatencyMS = &v
	}
	if fields.RequestSize != nil {
		v := *fields.RequestSize
		rec.RequestSize = &v
	}
	if fields.ResponseSize != nil {
		v := *fields.ResponseSize
		rec.ResponseSize = &v
	}
	if fields.RequestBody != nil {
		rec.RequestBody = cloneBytes(*fields.RequestBody)
	}
	if fields.ResponseBody != nil {
		rec.ResponseBody = cloneBytes(*fields.ResponseBody)
	}
	if fields.TTFTMS != nil {
		v := *fields.TTFTMS
		rec.TTFTMS = &v
	}
	if fields.ErrorClass != nil {
		ec := *fields.ErrorClass
		rec.ErrorClass = &ec
	}
	if fields.Model != nil {
		m := *fields.Model
		rec.Model = &m
	}
	if fields.MCPMethod != nil {
		mm := *fields.MCPMethod
		rec.MCPMethod = &mm
	}
	if fields.MCPTool != nil {
		mt := *fields.MCPTool
		rec.MCPTool = &mt
	}
	if fields.PaymentNetwork != nil {
		pn := *fields.PaymentNetwork
		rec.PaymentNetwork = &pn
	}
	if fields.PaymentAsset != nil {
		pa := *fields.PaymentAsset
		rec.PaymentAsset = &pa
	}
	if fields.PaymentAmount != nil {
		pa := *fields.PaymentAmount
		rec.PaymentAmount = &pa
	}
	if fields.PaymentPayer != nil {
		pp := *fields.PaymentPayer
		rec.PaymentPayer = &pp
	}
	if fields.PaymentNonce != nil {
		pn := *fields.PaymentNonce
		rec.PaymentNonce = &pn
	}
	if fields.SettlementStatus != nil {
		ss := *fields.SettlementStatus
		rec.SettlementStatus = &ss
	}
	if fields.SettlementTxHash != nil {
		th := *fields.SettlementTxHash
		rec.SettlementTxHash = &th
	}
	if fields.SettledAmount != nil {
		sa := *fields.SettledAmount
		rec.SettledAmount = &sa
	}
	if fields.OperatorAmount != nil {
		oa := *fields.OperatorAmount
		rec.OperatorAmount = &oa
	}
	if fields.FacilitatorFee != nil {
		ff := *fields.FacilitatorFee
		rec.FacilitatorFee = &ff
	}
	if fields.SettlementAttempts != nil {
		sa := *fields.SettlementAttempts
		rec.SettlementAttempts = &sa
	}
	if fields.SettlementReason != nil {
		sr := *fields.SettlementReason
		rec.SettlementReason = &sr
	}
	if fields.SettledAt != nil {
		t := *fields.SettledAt
		rec.SettledAt = &t
	}
	rec.UpdatedAt = time.Now().UTC()

	return cloneInvocation(rec), nil
}

// bucket returns (and lazily initialises) the per-tenant record map. Caller
// must hold s.mu write lock.
func (s *MemoryStore) bucket(tenantID string) map[string]*Invocation {
	if s.records[tenantID] == nil {
		s.records[tenantID] = make(map[string]*Invocation)
	}
	return s.records[tenantID]
}

// find looks up a record by tenant and ID. Caller must hold s.mu (any lock).
func (s *MemoryStore) find(tenantID, id string) (*Invocation, error) {
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

func cloneInvocation(inv *Invocation) *Invocation {
	c := *inv
	c.RequestBody = cloneBytes(inv.RequestBody)
	c.ResponseBody = cloneBytes(inv.ResponseBody)
	if inv.CompletedAt != nil {
		t := *inv.CompletedAt
		c.CompletedAt = &t
	}
	if inv.UpstreamStatus != nil {
		v := *inv.UpstreamStatus
		c.UpstreamStatus = &v
	}
	if inv.LatencyMS != nil {
		v := *inv.LatencyMS
		c.LatencyMS = &v
	}
	if inv.RequestSize != nil {
		v := *inv.RequestSize
		c.RequestSize = &v
	}
	if inv.ResponseSize != nil {
		v := *inv.ResponseSize
		c.ResponseSize = &v
	}
	if inv.TTFTMS != nil {
		v := *inv.TTFTMS
		c.TTFTMS = &v
	}
	if inv.ErrorClass != nil {
		ec := *inv.ErrorClass
		c.ErrorClass = &ec
	}
	if inv.Model != nil {
		m := *inv.Model
		c.Model = &m
	}
	if inv.MCPMethod != nil {
		mm := *inv.MCPMethod
		c.MCPMethod = &mm
	}
	if inv.MCPTool != nil {
		mt := *inv.MCPTool
		c.MCPTool = &mt
	}
	if inv.PaymentNetwork != nil {
		pn := *inv.PaymentNetwork
		c.PaymentNetwork = &pn
	}
	if inv.PaymentAsset != nil {
		pa := *inv.PaymentAsset
		c.PaymentAsset = &pa
	}
	if inv.PaymentAmount != nil {
		pa := *inv.PaymentAmount
		c.PaymentAmount = &pa
	}
	if inv.PaymentPayer != nil {
		pp := *inv.PaymentPayer
		c.PaymentPayer = &pp
	}
	if inv.PaymentNonce != nil {
		pn := *inv.PaymentNonce
		c.PaymentNonce = &pn
	}
	if inv.SettlementStatus != nil {
		ss := *inv.SettlementStatus
		c.SettlementStatus = &ss
	}
	if inv.SettlementTxHash != nil {
		th := *inv.SettlementTxHash
		c.SettlementTxHash = &th
	}
	if inv.SettledAmount != nil {
		sa := *inv.SettledAmount
		c.SettledAmount = &sa
	}
	if inv.OperatorAmount != nil {
		oa := *inv.OperatorAmount
		c.OperatorAmount = &oa
	}
	if inv.FacilitatorFee != nil {
		ff := *inv.FacilitatorFee
		c.FacilitatorFee = &ff
	}
	if inv.SettlementAttempts != nil {
		sa := *inv.SettlementAttempts
		c.SettlementAttempts = &sa
	}
	if inv.SettlementReason != nil {
		sr := *inv.SettlementReason
		c.SettlementReason = &sr
	}
	if inv.SettledAt != nil {
		t := *inv.SettledAt
		c.SettledAt = &t
	}
	return &c
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
