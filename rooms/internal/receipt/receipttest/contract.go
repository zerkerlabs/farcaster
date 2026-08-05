// Package receipttest holds the contract test suite every receipt.Emitter
// implementation must satisfy. It is written against the receipt.Emitter
// interface, not any concrete implementation, so it runs unchanged against
// receipt.NewFake today and a real client later without modification.
//
// It exercises both sides of the seam: Emit accepts a well-formed receipt for
// either outcome and is safe to call concurrently; ListByRoom reads back what
// was emitted, scoped to one room within one tenant, in emission order.
// General receipt querying beyond that — pagination, filtering, cross-room
// listing — is out of scope, so this suite does not exercise it.
package receipttest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zerkerlabs/gateway/rooms/internal/receipt"
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

	t.Run("ListByRoom returns emitted receipts for that room in emission order", func(t *testing.T) {
		t.Parallel()

		e := newEmitter()
		r1 := receipt.Receipt{
			RoomID:     "rom_1",
			TenantID:   "tenant-a",
			Outcome:    receipt.OutcomeSucceeded,
			OccurredAt: time.Now().UTC(),
		}
		r2 := receipt.Receipt{
			RoomID:       "rom_1",
			TenantID:     "tenant-a",
			Outcome:      receipt.OutcomeFailed,
			FailureClass: "upstream_failure",
			OccurredAt:   time.Now().UTC(),
		}
		if err := e.Emit(context.Background(), r1); err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if err := e.Emit(context.Background(), r2); err != nil {
			t.Fatalf("Emit: %v", err)
		}

		got, err := e.ListByRoom(context.Background(), "tenant-a", "rom_1")
		if err != nil {
			t.Fatalf("ListByRoom: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len(got) = %d, want 2", len(got))
		}
		if got[0].Outcome != receipt.OutcomeSucceeded || got[1].Outcome != receipt.OutcomeFailed {
			t.Errorf("got = %+v, want [succeeded failed] in emission order", got)
		}
	})

	t.Run("ListByRoom isolates receipts by room", func(t *testing.T) {
		t.Parallel()

		e := newEmitter()
		if err := e.Emit(context.Background(), receipt.Receipt{
			RoomID: "rom_1", TenantID: "tenant-a", Outcome: receipt.OutcomeSucceeded, OccurredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if err := e.Emit(context.Background(), receipt.Receipt{
			RoomID: "rom_2", TenantID: "tenant-a", Outcome: receipt.OutcomeSucceeded, OccurredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Emit: %v", err)
		}

		got, err := e.ListByRoom(context.Background(), "tenant-a", "rom_1")
		if err != nil {
			t.Fatalf("ListByRoom: %v", err)
		}
		if len(got) != 1 || got[0].RoomID != "rom_1" {
			t.Errorf("got = %+v, want exactly rom_1's receipt (rom_2's leaked)", got)
		}
	})

	t.Run("ListByRoom isolates receipts by tenant for the same room ID", func(t *testing.T) {
		t.Parallel()

		e := newEmitter()
		if err := e.Emit(context.Background(), receipt.Receipt{
			RoomID: "rom_1", TenantID: "tenant-a", Outcome: receipt.OutcomeSucceeded, OccurredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if err := e.Emit(context.Background(), receipt.Receipt{
			RoomID: "rom_1", TenantID: "tenant-b", Outcome: receipt.OutcomeSucceeded, OccurredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Emit: %v", err)
		}

		got, err := e.ListByRoom(context.Background(), "tenant-a", "rom_1")
		if err != nil {
			t.Fatalf("ListByRoom: %v", err)
		}
		if len(got) != 1 || got[0].TenantID != "tenant-a" {
			t.Errorf("got = %+v, want exactly tenant-a's receipt (tenant-b's leaked across tenants)", got)
		}
	})

	t.Run("ListByRoom for a room with no receipts returns empty, not an error", func(t *testing.T) {
		t.Parallel()

		e := newEmitter()

		got, err := e.ListByRoom(context.Background(), "tenant-a", "rom_never_emitted")
		if err != nil {
			t.Fatalf("ListByRoom: %v, want a nil error for a room with no receipts", err)
		}
		if len(got) != 0 {
			t.Errorf("got = %+v, want empty", got)
		}
	})
}
