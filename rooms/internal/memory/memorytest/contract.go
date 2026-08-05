// Package memorytest holds the contract test suite every memory.Store
// implementation must satisfy. It is written against the memory.Store
// interface, not any concrete implementation, so it runs unchanged against
// memory.NewFake today and a real client later without modification.
package memorytest

import (
	"context"
	"testing"

	"github.com/zerkerlabs/gateway/rooms/internal/memory"
)

// RunContract runs the full contract suite against implementations returned
// by newStore, calling it once per behaviour so each case starts from a
// clean store.
func RunContract(t *testing.T, newStore func() memory.Store) {
	t.Helper()

	t.Run("read-after-write", func(t *testing.T) {
		t.Parallel()

		s := newStore()
		scope := memory.Scope{TenantID: "tenant-a", AgentID: "agt_1"}

		if _, err := s.Append(context.Background(), scope, "first"); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if _, err := s.Append(context.Background(), scope, "second"); err != nil {
			t.Fatalf("Append: %v", err)
		}

		entries, err := s.Read(context.Background(), scope)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("len(entries) = %d, want 2", len(entries))
		}
		if entries[0].Content != "first" || entries[1].Content != "second" {
			t.Errorf("entries = %+v, want [first second] in append order", entries)
		}
	})

	t.Run("scope isolation between agents in the same tenant", func(t *testing.T) {
		t.Parallel()

		s := newStore()
		tenantID := "tenant-a"
		scopeA := memory.Scope{TenantID: tenantID, AgentID: "agt_1"}
		scopeB := memory.Scope{TenantID: tenantID, AgentID: "agt_2"}

		if _, err := s.Append(context.Background(), scopeA, "agent-1-only"); err != nil {
			t.Fatalf("Append: %v", err)
		}

		entriesB, err := s.Read(context.Background(), scopeB)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(entriesB) != 0 {
			t.Errorf("agt_2's scope = %+v, want empty (agt_1's entry leaked)", entriesB)
		}
	})

	t.Run("tenant isolation for the same agent ID", func(t *testing.T) {
		t.Parallel()

		s := newStore()
		agentID := "agt_shared"
		scopeA := memory.Scope{TenantID: "tenant-a", AgentID: agentID}
		scopeB := memory.Scope{TenantID: "tenant-b", AgentID: agentID}

		if _, err := s.Append(context.Background(), scopeA, "tenant-a-only"); err != nil {
			t.Fatalf("Append: %v", err)
		}

		entriesB, err := s.Read(context.Background(), scopeB)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(entriesB) != 0 {
			t.Errorf("tenant-b's scope = %+v, want empty (tenant-a's entry leaked across tenants)", entriesB)
		}
	})

	t.Run("reading an empty scope returns empty, not an error", func(t *testing.T) {
		t.Parallel()

		s := newStore()
		scope := memory.Scope{TenantID: "tenant-a", AgentID: "agt_never_written"}

		entries, err := s.Read(context.Background(), scope)
		if err != nil {
			t.Fatalf("Read: %v, want a nil error for an empty scope", err)
		}
		if len(entries) != 0 {
			t.Errorf("entries = %+v, want empty", entries)
		}
	})
}
