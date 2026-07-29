// Package receipttest holds the contract test suite every receipt.Emitter
// implementation must satisfy. It is written against the receipt.Emitter
// interface, not any concrete implementation, so it runs unchanged against
// receipt.NewFake today and a real client later without modification.
//
// The interface is emit-only by design (a receipt query API is out of
// scope), so this suite can only exercise what Emit itself promises: it
// accepts a well-formed receipt for either outcome, and it is safe to call
// concurrently. It cannot assert what a backend does with what it was
// handed — that would require a read seam this package deliberately does
// not have.
package receipttest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/receipt"
)

// RunContract runs the full contract suite against implementations returned
// by newEmitter, calling it once per behaviour so each case starts from a
// clean emitter.
func RunContract(t *testing.T, newEmitter func() receipt.Emitter) {
	t.Helper()

	t.Run("Emit accepts a well-formed succeeded receipt", func(t *testing.T) {
		t.Parallel()

		e := newEmitter()
		r := receipt.Receipt{
			RoomID:       "rom_1",
			TenantID:     "tenant-a",
			FromMemberID: "mem_1",
			ToMemberID:   "mem_2",
			ToAgentID:    "agt_2",
			InvocationID: "inv_1",
			Outcome:      receipt.OutcomeSucceeded,
			OccurredAt:   time.Now().UTC(),
		}
		if err := e.Emit(context.Background(), r); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	})

	t.Run("Emit accepts a well-formed failed receipt with no invocation ID", func(t *testing.T) {
		t.Parallel()

		e := newEmitter()
		r := receipt.Receipt{
			RoomID:       "rom_1",
			TenantID:     "tenant-a",
			FromMemberID: "mem_1",
			ToMemberID:   "mem_2",
			ToAgentID:    "agt_2",
			Outcome:      receipt.OutcomeFailed,
			FailureClass: "upstream_failure",
			OccurredAt:   time.Now().UTC(),
		}
		if err := e.Emit(context.Background(), r); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	})

	t.Run("Emit is safe for concurrent use", func(t *testing.T) {
		t.Parallel()

		e := newEmitter()
		const n = 50

		var wg sync.WaitGroup
		wg.Add(n)
		for i := range n {
			go func(i int) {
				defer wg.Done()
				r := receipt.Receipt{
					RoomID:       "rom_1",
					TenantID:     "tenant-a",
					FromMemberID: "mem_1",
					ToMemberID:   "mem_2",
					ToAgentID:    "agt_2",
					Outcome:      receipt.OutcomeSucceeded,
					OccurredAt:   time.Now().UTC(),
				}
				if err := e.Emit(context.Background(), r); err != nil {
					t.Errorf("Emit %d: %v", i, err)
				}
			}(i)
		}
		wg.Wait()
	})
}
