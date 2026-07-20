package policy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zerkerlabs/farcaster/gateway/internal/policy"
)

func recorded(tenant, agent, tool string, action policy.Action, matchedRule, reason string) policy.RecordedDecision {
	var mcpTool *string
	if tool != "" {
		mcpTool = &tool
	}
	return policy.RecordedDecision{
		TenantID: tenant,
		AgentID:  agent,
		Protocol: "mcp",
		MCPTool:  mcpTool,
		Decision: policy.Decision{Action: action, MatchedRule: matchedRule, Reason: reason},
	}
}

func TestMemoryDecisionStore_InsertAssignsIDAndTimestamp(t *testing.T) {
	t.Parallel()

	s := policy.NewMemoryDecisionStore()
	got, err := s.Insert(context.Background(), recorded(tenantA, "agt_1", "delete_repo", policy.ActionDeny, "1", "denied by rule 1"))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !strings.HasPrefix(got.ID, "pdec_") {
		t.Errorf("ID = %q, want pdec_ prefix", got.ID)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want a store-assigned timestamp")
	}
	if got.Action != policy.ActionDeny || got.MatchedRule != "1" || got.Reason != "denied by rule 1" {
		t.Errorf("decision fields not round-tripped: %+v", got)
	}
	if got.MCPTool == nil || *got.MCPTool != "delete_repo" {
		t.Errorf("MCPTool = %v, want delete_repo", got.MCPTool)
	}
}

func TestMemoryDecisionStore_ListRecentEmpty(t *testing.T) {
	t.Parallel()

	s := policy.NewMemoryDecisionStore()
	got, err := s.ListRecent(context.Background(), tenantA, 20)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListRecent on empty store = %d rows, want 0", len(got))
	}
}

func TestMemoryDecisionStore_ListRecentNewestFirst(t *testing.T) {
	t.Parallel()

	s := policy.NewMemoryDecisionStore()
	ctx := context.Background()
	for _, tool := range []string{"first", "second", "third"} {
		if _, err := s.Insert(ctx, recorded(tenantA, "agt_1", tool, policy.ActionWarn, "1", "warned")); err != nil {
			t.Fatalf("Insert %s: %v", tool, err)
		}
	}

	got, err := s.ListRecent(ctx, tenantA, 20)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	wantOrder := []string{"third", "second", "first"} // newest first
	if len(got) != len(wantOrder) {
		t.Fatalf("ListRecent = %d rows, want %d", len(got), len(wantOrder))
	}
	for i, want := range wantOrder {
		if got[i].MCPTool == nil || *got[i].MCPTool != want {
			t.Errorf("row %d tool = %v, want %q (newest-first order)", i, got[i].MCPTool, want)
		}
	}
}

func TestMemoryDecisionStore_ListRecentRespectsLimit(t *testing.T) {
	t.Parallel()

	s := policy.NewMemoryDecisionStore()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.Insert(ctx, recorded(tenantA, "agt_1", "tool", policy.ActionAllow, "", "")); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	got, err := s.ListRecent(ctx, tenantA, 2)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListRecent(limit=2) = %d rows, want 2", len(got))
	}
}

func TestMemoryDecisionStore_TenantIsolation(t *testing.T) {
	t.Parallel()

	s := policy.NewMemoryDecisionStore()
	ctx := context.Background()
	if _, err := s.Insert(ctx, recorded(tenantB, "agt_b", "b_tool", policy.ActionDeny, "1", "denied")); err != nil {
		t.Fatalf("Insert tenantB: %v", err)
	}

	got, err := s.ListRecent(ctx, tenantA, 20)
	if err != nil {
		t.Fatalf("ListRecent tenantA: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tenantA sees %d rows, want 0 — a tenant must never read another's decisions", len(got))
	}
}

func TestMemoryDecisionStore_ListRecentDoesNotAlias(t *testing.T) {
	t.Parallel()

	s := policy.NewMemoryDecisionStore()
	ctx := context.Background()
	if _, err := s.Insert(ctx, recorded(tenantA, "agt_1", "tool", policy.ActionWarn, "1", "warned")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.ListRecent(ctx, tenantA, 20)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	// Mutating a returned record must not corrupt the store's copy.
	*got[0].MCPTool = "mutated"
	got[0].Reason = "mutated"

	again, err := s.ListRecent(ctx, tenantA, 20)
	if err != nil {
		t.Fatalf("ListRecent (second): %v", err)
	}
	if *again[0].MCPTool != "tool" || again[0].Reason != "warned" {
		t.Errorf("store record was aliased and mutated: tool=%q reason=%q", *again[0].MCPTool, again[0].Reason)
	}
}
