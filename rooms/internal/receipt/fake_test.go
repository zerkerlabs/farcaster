package receipt_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/receipt"
	"github.com/zerkerlabs/farcaster/rooms/internal/receipt/receipttest"
)

func TestFake_Contract(t *testing.T) {
	t.Parallel()

	receipttest.RunContract(t, func() receipt.Emitter {
		return receipt.NewFake()
	})
}

func TestFake_RecordsEveryEmittedReceiptInOrder(t *testing.T) {
	t.Parallel()

	f := receipt.NewFake()
	r1 := receipt.Receipt{RoomID: "rom_1", Outcome: receipt.OutcomeSucceeded, OccurredAt: time.Now().UTC()}
	r2 := receipt.Receipt{RoomID: "rom_1", Outcome: receipt.OutcomeFailed, FailureClass: "upstream_failure", OccurredAt: time.Now().UTC()}

	if err := f.Emit(context.Background(), r1); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if err := f.Emit(context.Background(), r2); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	got := f.Receipts()
	if len(got) != 2 {
		t.Fatalf("len(Receipts()) = %d, want 2", len(got))
	}
	if got[0].Outcome != receipt.OutcomeSucceeded || got[1].Outcome != receipt.OutcomeFailed {
		t.Errorf("receipts = %+v, want [succeeded failed] in emission order", got)
	}
}

func TestFake_EmitRejectsAnAlreadyCancelledContext(t *testing.T) {
	t.Parallel()

	f := receipt.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := f.Emit(ctx, receipt.Receipt{RoomID: "rom_1"}); err == nil {
		t.Error("Emit with a cancelled context = nil error, want non-nil")
	}
	if got := f.Receipts(); len(got) != 0 {
		t.Errorf("Receipts() = %+v, want empty — a cancelled Emit must not record", got)
	}
}

func TestFake_ListByRoomReflectsConcurrentEmits(t *testing.T) {
	t.Parallel()

	const n = 50
	f := receipt.NewFake()

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			r := receipt.Receipt{RoomID: "rom_1", TenantID: "tenant-a", Outcome: receipt.OutcomeSucceeded, OccurredAt: time.Now().UTC()}
			if err := f.Emit(context.Background(), r); err != nil {
				t.Errorf("Emit %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got, err := f.ListByRoom(context.Background(), "tenant-a", "rom_1")
	if err != nil {
		t.Fatalf("ListByRoom: %v", err)
	}
	if len(got) != n {
		t.Errorf("len(got) = %d, want %d", len(got), n)
	}
}

// TestFake_ConcurrentEmitDoesNotRace exercises the Fake's append path from
// many goroutines at once; run with -race (make check's test target does).
func TestFake_ConcurrentEmitDoesNotRace(t *testing.T) {
	t.Parallel()

	const n = 50
	f := receipt.NewFake()

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			r := receipt.Receipt{RoomID: "rom_1", Outcome: receipt.OutcomeSucceeded, OccurredAt: time.Now().UTC()}
			if err := f.Emit(context.Background(), r); err != nil {
				t.Errorf("Emit %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if got := len(f.Receipts()); got != n {
		t.Errorf("len(Receipts()) = %d, want %d", got, n)
	}
}
