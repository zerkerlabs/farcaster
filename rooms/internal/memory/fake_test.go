package memory_test

import (
	"context"
	"sync"
	"testing"

	"github.com/zerkerlabs/gateway/rooms/internal/memory"
	"github.com/zerkerlabs/gateway/rooms/internal/memory/memorytest"
)

func TestFake_Contract(t *testing.T) {
	t.Parallel()

	memorytest.RunContract(t, func() memory.Store {
		return memory.NewFake()
	})
}

// TestFake_ConcurrentAppendDoesNotRace exercises the Fake's append path from
// many goroutines at once; run with -race (make check's test target does).
func TestFake_ConcurrentAppendDoesNotRace(t *testing.T) {
	t.Parallel()

	const n = 50
	f := memory.NewFake()
	scope := memory.Scope{TenantID: "tenant-a", AgentID: "agt_1"}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			if _, err := f.Append(context.Background(), scope, "entry"); err != nil {
				t.Errorf("Append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	entries, err := f.Read(context.Background(), scope)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != n {
		t.Errorf("len(entries) = %d, want %d", len(entries), n)
	}
}
