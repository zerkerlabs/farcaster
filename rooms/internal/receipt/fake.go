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

// Receipts returns every receipt recorded so far, in emission order. It is a
// test-observability accessor, not part of the Emitter interface — a real
// backend has no equivalent, since a receipt query API is out of scope.
func (f *Fake) Receipts() []Receipt {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Receipt, len(f.receipts))
	copy(out, f.receipts)
	return out
}
