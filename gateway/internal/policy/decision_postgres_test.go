//go:build integration

package policy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/policy"
)

func TestPGDecision_InsertAndListRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	s := policy.NewPostgresDecisionStore(pool)
	ctx := context.Background()

	got, err := s.Insert(ctx, recorded("tenant-alpha", "agt_1", "delete_repo", policy.ActionDeny, "2", "denied by rule 2"))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !strings.HasPrefix(got.ID, "pdec_") {
		t.Errorf("ID = %q, want pdec_ prefix", got.ID)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want a store-assigned timestamp")
	}

	list, err := s.ListRecent(ctx, "tenant-alpha", 20)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListRecent = %d rows, want 1", len(list))
	}
	d := list[0]
	if d.Action != policy.ActionDeny || d.MatchedRule != "2" || d.Reason != "denied by rule 2" {
		t.Errorf("decision fields not round-tripped: %+v", d)
	}
	if d.MCPTool == nil || *d.MCPTool != "delete_repo" {
		t.Errorf("MCPTool = %v, want delete_repo", d.MCPTool)
	}
	if d.AgentID != "agt_1" || d.Protocol != "mcp" {
		t.Errorf("agent/protocol not round-tripped: agent=%q protocol=%q", d.AgentID, d.Protocol)
	}
}

func TestPGDecision_ListRecentNewestFirstAndLimit(t *testing.T) {
	pool := openTestPool(t)
	s := policy.NewPostgresDecisionStore(pool)
	ctx := context.Background()

	for _, tool := range []string{"first", "second", "third"} {
		if _, err := s.Insert(ctx, recorded("tenant-alpha", "agt_1", tool, policy.ActionWarn, "1", "warned")); err != nil {
			t.Fatalf("Insert %s: %v", tool, err)
		}
	}

	list, err := s.ListRecent(ctx, "tenant-alpha", 2)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListRecent(limit=2) = %d rows, want 2", len(list))
	}
	// created_at DESC, id DESC: uuidv7 ids are time-ordered, so the two newest
	// inserts ("third", then "second") come back in that order.
	if list[0].MCPTool == nil || *list[0].MCPTool != "third" {
		t.Errorf("row 0 tool = %v, want third (newest)", list[0].MCPTool)
	}
	if list[1].MCPTool == nil || *list[1].MCPTool != "second" {
		t.Errorf("row 1 tool = %v, want second", list[1].MCPTool)
	}
}

func TestPGDecision_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := policy.NewPostgresDecisionStore(pool)
	ctx := context.Background()

	if _, err := s.Insert(ctx, recorded("tenant-beta", "agt_b", "b_tool", policy.ActionDeny, "1", "denied")); err != nil {
		t.Fatalf("Insert tenant-beta: %v", err)
	}

	list, err := s.ListRecent(ctx, "tenant-alpha", 20)
	if err != nil {
		t.Fatalf("ListRecent tenant-alpha: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("tenant-alpha sees %d rows, want 0 — a tenant must never read another's decisions", len(list))
	}
}

func TestPGDecision_HTTPProtocolNullTool(t *testing.T) {
	pool := openTestPool(t)
	s := policy.NewPostgresDecisionStore(pool)
	ctx := context.Background()

	// protocol=http → no mcp_tool; the null must round-trip as a nil pointer.
	rd := policy.RecordedDecision{
		TenantID: "tenant-alpha",
		AgentID:  "agt_http",
		Protocol: "http",
		Decision: policy.Decision{Action: policy.ActionAllow},
	}
	if _, err := s.Insert(ctx, rd); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	list, err := s.ListRecent(ctx, "tenant-alpha", 20)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListRecent = %d rows, want 1", len(list))
	}
	if list[0].MCPTool != nil {
		t.Errorf("MCPTool = %v, want nil for a protocol=http decision", *list[0].MCPTool)
	}
}
