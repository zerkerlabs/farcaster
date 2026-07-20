package settlement

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/zerkerlabs/farcaster/facilitator/internal/guardrail"
)

// MemoryStore is a thread-safe, in-memory Store for unit tests and local dev
// (no database). It reproduces the Postgres store's contract exactly — most
// importantly the atomic Claim — so the single-flight/idempotency behavior is
// exercised by the default `make check` path without a Postgres.
type MemoryStore struct {
	mu    sync.Mutex
	byKey map[string]*Settlement // accountID\x00nonce → row
	byID  map[string]*Settlement // id → same row
}

// NewMemoryStore returns an empty MemoryStore ready for use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byKey: make(map[string]*Settlement),
		byID:  make(map[string]*Settlement),
	}
}

func idemKey(accountID, nonce string) string { return accountID + "\x00" + nonce }

// Claim implements Store. The whole check-ceiling-and-insert runs under one
// lock, so concurrent claims for the same account — not just the same nonce —
// are serialized: the in-memory equivalent of the Postgres advisory-lock-
// guarded transaction (see PostgresStore.Claim). That serialization is what
// makes the ceiling check race-free: without it, two concurrent claims could
// each read the same committed total before either inserted its row.
func (s *MemoryStore) Claim(ctx context.Context, key ClaimKey, ceiling DailyCeiling) (*Settlement, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	k := idemKey(key.AccountID, key.Nonce)
	if existing, ok := s.byKey[k]; ok {
		return cloneSettlement(existing), false, nil
	}

	committed, err := s.committedTotalLocked(key.AccountID, ceiling.Since)
	if err != nil {
		return nil, false, err
	}
	if new(big.Int).Add(committed, ceiling.Amount).Cmp(ceiling.Limit) > 0 {
		return nil, false, guardrail.ErrDailyCeilingExceeded
	}

	now := time.Now().UTC()
	row := &Settlement{
		ID:          "facset_" + id.String(),
		AccountID:   key.AccountID,
		Nonce:       key.Nonce,
		Payer:       key.Payer,
		PayTo:       key.PayTo,
		Network:     key.Network,
		Asset:       key.Asset,
		Value:       key.Value,
		ValidAfter:  key.ValidAfter,
		ValidBefore: key.ValidBefore,
		Status:      StatusInFlight,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.byKey[k] = row
	s.byID[row.ID] = row
	return cloneSettlement(row), true, nil
}

// committedTotalLocked sums the Value of every StatusInFlight or
// StatusSettled row for accountID created at or after since — the committed
// spend DailyCeiling checks against (see DailyCeiling for why in-flight rows
// count). The caller must hold s.mu.
func (s *MemoryStore) committedTotalLocked(accountID string, since time.Time) (*big.Int, error) {
	total := new(big.Int)
	for _, row := range s.byID {
		if row.AccountID != accountID || row.CreatedAt.Before(since) {
			continue
		}
		if row.Status != StatusInFlight && row.Status != StatusSettled {
			continue
		}
		value, ok := new(big.Int).SetString(row.Value, 10)
		if !ok {
			return nil, fmt.Errorf("settlement %s: non-integer value %q", row.ID, row.Value)
		}
		total.Add(total, value)
	}
	return total, nil
}

// MarkSettled implements Store.
func (s *MemoryStore) MarkSettled(ctx context.Context, id, txHash string) (*Settlement, error) {
	return s.transition(ctx, id, StatusSettled, txHash, "")
}

// MarkFailed implements Store.
func (s *MemoryStore) MarkFailed(ctx context.Context, id, reason, txHash string) (*Settlement, error) {
	return s.transition(ctx, id, StatusFailed, txHash, reason)
}

// transition guards MarkSettled/MarkFailed: it only mutates a row that is
// currently StatusInFlight. A row already in the target status with a
// matching txHash/reason is an idempotent replay (returns the row unchanged,
// no error); any other non-in-flight row is an illegal transition
// (ErrInvalidTransition, row left unchanged).
func (s *MemoryStore) transition(ctx context.Context, id string, status Status, txHash, reason string) (*Settlement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	row, ok := s.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	if row.Status != StatusInFlight {
		if row.Status == status && row.TxHash == txHash && row.ErrorReason == reason {
			return cloneSettlement(row), nil
		}
		return nil, ErrInvalidTransition
	}
	row.Status = status
	row.TxHash = txHash
	row.ErrorReason = reason
	row.UpdatedAt = time.Now().UTC()
	return cloneSettlement(row), nil
}

// Get implements Store.
func (s *MemoryStore) Get(ctx context.Context, accountID, nonce string) (*Settlement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	row, ok := s.byKey[idemKey(accountID, nonce)]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneSettlement(row), nil
}

// List implements Store, scoped to one account and ordered newest-first.
func (s *MemoryStore) List(ctx context.Context, accountID string) ([]*Settlement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Settlement
	for _, row := range s.byID {
		if row.AccountID == accountID {
			out = append(out, cloneSettlement(row))
		}
	}
	// Newest first; ID is a UUIDv7 (time-ordered) so it breaks CreatedAt ties
	// deterministically.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// SumSettled implements Store.
func (s *MemoryStore) SumSettled(ctx context.Context, accountID string, since time.Time) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	total := new(big.Int)
	for _, row := range s.byID {
		if row.AccountID != accountID || row.Status != StatusSettled || row.CreatedAt.Before(since) {
			continue
		}
		value, ok := new(big.Int).SetString(row.Value, 10)
		if !ok {
			return nil, fmt.Errorf("settlement %s: non-integer value %q", row.ID, row.Value)
		}
		total.Add(total, value)
	}
	return total, nil
}

// StaleInFlight implements Store, oldest first (matching PostgresStore's
// ORDER BY created_at ASC) so a long-stuck row is reconciled before newer
// ones in the same sweep pass.
func (s *MemoryStore) StaleInFlight(ctx context.Context, olderThan time.Time) ([]*Settlement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Settlement
	for _, row := range s.byID {
		if row.Status == StatusInFlight && row.CreatedAt.Before(olderThan) {
			out = append(out, cloneSettlement(row))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func cloneSettlement(s *Settlement) *Settlement {
	cp := *s
	return &cp
}
