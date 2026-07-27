package memory

import (
	"context"
	"sync"
	"time"
)

// Fake is a thread-safe, in-memory Store. It is a stand-in for the real
// memory backend, which does not exist yet — not a product of its own: it
// keeps entries in a process-local map with no persistence, eviction, or
// querying beyond a scope's entries in append order.
type Fake struct {
	mu      sync.RWMutex
	entries map[Scope][]Entry
}

// NewFake returns an empty Fake ready for use.
func NewFake() *Fake {
	return &Fake{entries: make(map[Scope][]Entry)}
}

// Read implements Store.
func (f *Fake) Read(ctx context.Context, scope Scope) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.mu.RLock()
	defer f.mu.RUnlock()

	existing := f.entries[scope]
	out := make([]Entry, len(existing))
	copy(out, existing)
	return out, nil
}

// Append implements Store.
func (f *Fake) Append(ctx context.Context, scope Scope, content string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	e := Entry{Content: content, CreatedAt: time.Now().UTC()}
	f.entries[scope] = append(f.entries[scope], e)
	return e, nil
}
