package receipt

import (
	"context"
	"sync"
)

// Fake is a thread-safe, in-memory Emitter. It is a stand-in for the real
// receipt backend, which does not exist yet — not a product of its own: it
// keeps every emitted receipt in a process-local slice with no persistence,
// signing, or delivery beyond recording the call.
type Fake struct {
	mu       sync.Mutex
	receipts []Receipt
}

// NewFake returns an empty Fake ready for use.
func NewFake() *Fake {
	return &Fake{}
}

// Emit implements Emitter.
func (f *Fake) Emit(ctx context.Context, r Receipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.receipts = append(f.receipts, r)
	return nil
}

// Receipts returns every receipt recorded so far, across every room and
// tenant, in emission order. It is a test-observability accessor, not part of
// the Emitter interface — unlike ListByRoom, a real backend has no equivalent
// for reading everything at once.
func (f *Fake) Receipts() []Receipt {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Receipt, len(f.receipts))
	copy(out, f.receipts)
	return out
}

// ListByRoom implements Emitter. It returns the receipts recorded for roomID
// within tenantID, in emission order — the fake's only order, since it keeps
// no other.
func (f *Fake) ListByRoom(ctx context.Context, tenantID, roomID string) ([]Receipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Receipt, 0, len(f.receipts))
	for _, r := range f.receipts {
		if r.TenantID == tenantID && r.RoomID == roomID {
			out = append(out, r)
		}
	}
	return out, nil
}
