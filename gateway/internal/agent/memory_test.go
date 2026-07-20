package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
)

const (
	tenantA = "tenant-alpha"
	tenantB = "tenant-beta"
)

func newStore(t *testing.T) *agent.MemoryStore {
	t.Helper()
	return agent.NewMemoryStore()
}

// mustCreate is a test helper that creates an agent and fails the test on error.
// The passed *Agent is mutated in-place with server-assigned fields.
func mustCreate(t *testing.T, s *agent.MemoryStore, tenantID string, a *agent.Agent) {
	t.Helper()
	if err := s.Create(context.Background(), tenantID, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// ------------------------------------------------------------------ Create ---

func TestCreate_AssignsServerFields(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "bot-1", CreatedBy: "user-x"}
	mustCreate(t, s, tenantA, a)

	if !strings.HasPrefix(a.ID, "agt_") {
		t.Errorf("ID %q does not start with agt_", a.ID)
	}
	if a.TenantID != tenantA {
		t.Errorf("TenantID = %q, want %q", a.TenantID, tenantA)
	}
	// No upstream_url supplied → status must be pending (spec 0002).
	if a.Status != agent.StatusPending {
		t.Errorf("Status = %q, want %q", a.Status, agent.StatusPending)
	}
	if a.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if a.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
	if a.CreatedBy != "user-x" {
		t.Errorf("CreatedBy = %q, want %q", a.CreatedBy, "user-x")
	}
}

func TestCreate_DuplicateNameSameTenant(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	mustCreate(t, s, tenantA, &agent.Agent{Name: "clash"})

	err := s.Create(context.Background(), tenantA, &agent.Agent{Name: "clash"})
	if !errors.Is(err, agent.ErrNameConflict) {
		t.Errorf("err = %v, want ErrNameConflict", err)
	}
}

func TestCreate_SameNameDifferentTenants(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	mustCreate(t, s, tenantA, &agent.Agent{Name: "shared-name"})

	// Same name in tenant B must succeed — names are scoped per tenant.
	if err := s.Create(context.Background(), tenantB, &agent.Agent{Name: "shared-name"}); err != nil {
		t.Errorf("cross-tenant name reuse should succeed, got: %v", err)
	}
}

func TestCreate_ReuseNameAfterSoftDelete(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "recycled"}
	mustCreate(t, s, tenantA, a)

	if err := s.Delete(context.Background(), tenantA, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Name should be reusable once the original agent is inactive.
	if err := s.Create(context.Background(), tenantA, &agent.Agent{Name: "recycled"}); err != nil {
		t.Errorf("name reuse after soft-delete should succeed, got: %v", err)
	}
}

// --------------------------------------------------------------------- Get ---

func TestGet_Found(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "lookup-me", Description: "desc", Tags: []string{"a", "b"}}
	mustCreate(t, s, tenantA, a)

	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != a.ID {
		t.Errorf("ID = %q, want %q", got.ID, a.ID)
	}
	if got.Name != "lookup-me" {
		t.Errorf("Name = %q, want %q", got.Name, "lookup-me")
	}
}

func TestGet_NotFound(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	_, err := s.Get(context.Background(), tenantA, "agt_nonexistent")
	if !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGet_CrossTenantBlocked(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "secret"}
	mustCreate(t, s, tenantA, a)

	// Tenant B must not be able to fetch tenant A's agent.
	_, err := s.Get(context.Background(), tenantB, a.ID)
	if !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("cross-tenant get: err = %v, want ErrNotFound", err)
	}
}

func TestGet_InactiveAgentVisible(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "deleted"}
	mustCreate(t, s, tenantA, a)
	if err := s.Delete(context.Background(), tenantA, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Inactive records are preserved for audit; Get should still return them.
	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get on inactive agent: %v", err)
	}
	if got.Status != agent.StatusInactive {
		t.Errorf("Status = %q, want %q", got.Status, agent.StatusInactive)
	}
}

// -------------------------------------------------------------------- List ---

func TestList_Empty(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	agents, total, err := s.List(context.Background(), tenantA, 1, 50, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if len(agents) != 0 {
		t.Errorf("len(agents) = %d, want 0", len(agents))
	}
}

func TestList_ActiveOnly(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	active := &agent.Agent{Name: "active-one"}
	deleted := &agent.Agent{Name: "deleted-one"}
	mustCreate(t, s, tenantA, active)
	mustCreate(t, s, tenantA, deleted)
	if err := s.Delete(context.Background(), tenantA, deleted.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	agents, total, err := s.List(context.Background(), tenantA, 1, 50, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(agents) != 1 {
		t.Fatalf("len(agents) = %d, want 1", len(agents))
	}
	if agents[0].ID != active.ID {
		t.Errorf("agents[0].ID = %q, want %q", agents[0].ID, active.ID)
	}
}

func TestList_CrossTenantIsolation(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	mustCreate(t, s, tenantA, &agent.Agent{Name: "a-only"})
	mustCreate(t, s, tenantB, &agent.Agent{Name: "b-only"})

	agentsA, totalA, err := s.List(context.Background(), tenantA, 1, 50, "")
	if err != nil {
		t.Fatalf("List(tenantA): %v", err)
	}
	if totalA != 1 || len(agentsA) != 1 {
		t.Errorf("tenantA: total=%d len=%d, want 1/1", totalA, len(agentsA))
	}
	if agentsA[0].Name != "a-only" {
		t.Errorf("tenantA agent name = %q, want %q", agentsA[0].Name, "a-only")
	}

	agentsB, totalB, err := s.List(context.Background(), tenantB, 1, 50, "")
	if err != nil {
		t.Fatalf("List(tenantB): %v", err)
	}
	if totalB != 1 || len(agentsB) != 1 {
		t.Errorf("tenantB: total=%d len=%d, want 1/1", totalB, len(agentsB))
	}
	if agentsB[0].Name != "b-only" {
		t.Errorf("tenantB agent name = %q, want %q", agentsB[0].Name, "b-only")
	}
}

func TestList_Pagination(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	for i := range 5 {
		mustCreate(t, s, tenantA, &agent.Agent{Name: strings.Repeat("x", i+1)})
	}

	page1, total, err := s.List(context.Background(), tenantA, 1, 2, "")
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(page1) != 2 {
		t.Errorf("page 1 len = %d, want 2", len(page1))
	}

	page3, _, err := s.List(context.Background(), tenantA, 3, 2, "")
	if err != nil {
		t.Fatalf("List page 3: %v", err)
	}
	if len(page3) != 1 {
		t.Errorf("page 3 len = %d, want 1", len(page3))
	}

	beyond, _, err := s.List(context.Background(), tenantA, 10, 2, "")
	if err != nil {
		t.Fatalf("List page beyond: %v", err)
	}
	if len(beyond) != 0 {
		t.Errorf("page beyond len = %d, want 0", len(beyond))
	}
}

func TestList_ProtocolFilter(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	httpAgent := &agent.Agent{Name: "http-agent"}
	mustCreate(t, s, tenantA, httpAgent)
	mcpAgent := &agent.Agent{Name: "mcp-agent", Protocol: "mcp"}
	mustCreate(t, s, tenantA, mcpAgent)

	mcpOnly, total, err := s.List(context.Background(), tenantA, 1, 50, "mcp")
	if err != nil {
		t.Fatalf("List(protocol=mcp): %v", err)
	}
	if total != 1 || len(mcpOnly) != 1 {
		t.Fatalf("protocol=mcp: total=%d len=%d, want 1/1", total, len(mcpOnly))
	}
	if mcpOnly[0].ID != mcpAgent.ID {
		t.Errorf("protocol=mcp: got agent %q, want %q", mcpOnly[0].ID, mcpAgent.ID)
	}

	httpOnly, total, err := s.List(context.Background(), tenantA, 1, 50, "http")
	if err != nil {
		t.Fatalf("List(protocol=http): %v", err)
	}
	if total != 1 || len(httpOnly) != 1 {
		t.Fatalf("protocol=http: total=%d len=%d, want 1/1", total, len(httpOnly))
	}
	if httpOnly[0].ID != httpAgent.ID {
		t.Errorf("protocol=http: got agent %q, want %q", httpOnly[0].ID, httpAgent.ID)
	}

	all, total, err := s.List(context.Background(), tenantA, 1, 50, "")
	if err != nil {
		t.Fatalf("List(protocol=\"\"): %v", err)
	}
	if total != 2 || len(all) != 2 {
		t.Errorf("protocol=\"\": total=%d len=%d, want 2/2 (unfiltered)", total, len(all))
	}
}

// ------------------------------------------------------------------ Update ---

func TestUpdate_MutableFields(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{
		Name:        "before",
		Description: "old desc",
		Tags:        []string{"old"},
		Metadata:    map[string]any{"k": "v1"},
	}
	mustCreate(t, s, tenantA, a)

	newName := "after"
	newDesc := "new desc"
	newTags := []string{"new", "tags"}
	newMeta := map[string]any{"k": "v2"}

	updated, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{
		Name:        &newName,
		Description: &newDesc,
		Tags:        &newTags,
		Metadata:    &newMeta,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if updated.Name != "after" {
		t.Errorf("Name = %q, want %q", updated.Name, "after")
	}
	if updated.Description != "new desc" {
		t.Errorf("Description = %q, want %q", updated.Description, "new desc")
	}
	if len(updated.Tags) != 2 || updated.Tags[0] != "new" {
		t.Errorf("Tags = %v, want [new tags]", updated.Tags)
	}
	if updated.Metadata["k"] != "v2" {
		t.Errorf("Metadata[k] = %v, want v2", updated.Metadata["k"])
	}
	if !updated.UpdatedAt.After(a.CreatedAt) {
		t.Error("UpdatedAt should be after CreatedAt")
	}
}

func TestUpdate_PartialFields(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "partial", Description: "keep-me"}
	mustCreate(t, s, tenantA, a)

	newDesc := "changed"
	updated, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{
		Description: &newDesc,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "partial" {
		t.Errorf("Name changed unexpectedly: %q", updated.Name)
	}
	if updated.Description != "changed" {
		t.Errorf("Description = %q, want %q", updated.Description, "changed")
	}
}

func TestUpdate_NotFound(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	name := "x"
	_, err := s.Update(context.Background(), tenantA, "agt_missing", agent.UpdateFields{Name: &name})
	if !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdate_CrossTenantBlocked(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "owned-by-a"}
	mustCreate(t, s, tenantA, a)

	name := "hijack"
	_, err := s.Update(context.Background(), tenantB, a.ID, agent.UpdateFields{Name: &name})
	if !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("cross-tenant update: err = %v, want ErrNotFound", err)
	}
}

func TestUpdate_NameConflict(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	mustCreate(t, s, tenantA, &agent.Agent{Name: "taken"})
	a := &agent.Agent{Name: "free"}
	mustCreate(t, s, tenantA, a)

	name := "taken"
	_, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{Name: &name})
	if !errors.Is(err, agent.ErrNameConflict) {
		t.Errorf("err = %v, want ErrNameConflict", err)
	}
}

func TestUpdate_SameNameNoConflict(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "same"}
	mustCreate(t, s, tenantA, a)

	name := "same"
	if _, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{Name: &name}); err != nil {
		t.Errorf("updating to the same name should not conflict: %v", err)
	}
}

// ------------------------------------------------------------------ Delete ---

func TestDelete_SoftDeletes(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "bye"}
	mustCreate(t, s, tenantA, a)

	if err := s.Delete(context.Background(), tenantA, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Record is preserved; status is inactive.
	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if got.Status != agent.StatusInactive {
		t.Errorf("Status = %q, want %q", got.Status, agent.StatusInactive)
	}

	// Must not appear in List.
	agents, total, err := s.List(context.Background(), tenantA, 1, 50, "")
	if err != nil {
		t.Fatalf("List after Delete: %v", err)
	}
	if total != 0 || len(agents) != 0 {
		t.Errorf("deleted agent appears in list: total=%d len=%d", total, len(agents))
	}
}

func TestDelete_NotFound(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	if err := s.Delete(context.Background(), tenantA, "agt_ghost"); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDelete_CrossTenantBlocked(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "protected"}
	mustCreate(t, s, tenantA, a)

	if err := s.Delete(context.Background(), tenantB, a.ID); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("cross-tenant delete: err = %v, want ErrNotFound", err)
	}

	// Original record must remain unchanged (pending — no upstream_url).
	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get after cross-tenant delete attempt: %v", err)
	}
	if got.Status != agent.StatusPending {
		t.Errorf("Status = %q after blocked delete, want %q", got.Status, agent.StatusPending)
	}
}

func TestDelete_AlreadyInactive(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "once"}
	mustCreate(t, s, tenantA, a)
	if err := s.Delete(context.Background(), tenantA, a.ID); err != nil {
		t.Fatalf("first Delete: %v", err)
	}

	// A second delete on an already-inactive agent returns ErrNotFound.
	if err := s.Delete(context.Background(), tenantA, a.ID); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("second Delete: err = %v, want ErrNotFound", err)
	}
}

// --------------------------------------------------------- Return isolation ---

// TestReturnedValuesAreIsolated verifies that mutating a returned *Agent does
// not corrupt the stored record.
func TestReturnedValuesAreIsolated(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{
		Name:     "immutable",
		Tags:     []string{"keep"},
		Metadata: map[string]any{"nested": map[string]any{"k": "original"}},
	}
	mustCreate(t, s, tenantA, a)

	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Tags[0] = "mutated"
	got.Metadata["nested"].(map[string]any)["k"] = "mutated"

	got2, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if got2.Tags[0] != "keep" {
		t.Errorf("stored Tags[0] = %q, want %q (caller mutation leaked into store)", got2.Tags[0], "keep")
	}
	if k := got2.Metadata["nested"].(map[string]any)["k"]; k != "original" {
		t.Errorf("stored nested Metadata = %q, want %q (nested mutation leaked into store)", k, "original")
	}
}

// TestUpdate_OnSoftDeletedReturnsNotFound verifies a soft-deleted agent is
// treated as gone by Update, not silently mutated.
func TestUpdate_OnSoftDeletedReturnsNotFound(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "gone"}
	mustCreate(t, s, tenantA, a)

	if err := s.Delete(context.Background(), tenantA, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	desc := "should not apply"
	if _, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{Description: &desc}); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("Update on soft-deleted = %v, want ErrNotFound", err)
	}
}

// ----------------------------------------- pending/active status transitions ---

func TestCreate_WithoutUpstreamURL_IsPending(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "catalog-only"}
	mustCreate(t, s, tenantA, a)

	if a.Status != agent.StatusPending {
		t.Errorf("Status = %q, want %q", a.Status, agent.StatusPending)
	}
	if a.UpstreamURL != nil {
		t.Errorf("UpstreamURL = %v, want nil", a.UpstreamURL)
	}
}

func TestCreate_WithUpstreamURL_IsActive(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	url := "https://agent.example.com/invoke"
	a := &agent.Agent{Name: "routable", UpstreamURL: &url}
	mustCreate(t, s, tenantA, a)

	if a.Status != agent.StatusActive {
		t.Errorf("Status = %q, want %q", a.Status, agent.StatusActive)
	}
	if a.UpstreamURL == nil || *a.UpstreamURL != url {
		t.Errorf("UpstreamURL = %v, want %q", a.UpstreamURL, url)
	}
}

func TestUpdate_SetUpstreamURL_TransitionsToActive(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "pending-agent"}
	mustCreate(t, s, tenantA, a)

	if a.Status != agent.StatusPending {
		t.Fatalf("precondition: Status = %q, want pending", a.Status)
	}

	url := "https://agent.example.com/invoke"
	urlPtr := &url
	updated, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{
		UpstreamURL: &urlPtr,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != agent.StatusActive {
		t.Errorf("Status after set UpstreamURL = %q, want %q", updated.Status, agent.StatusActive)
	}
	if updated.UpstreamURL == nil || *updated.UpstreamURL != url {
		t.Errorf("UpstreamURL = %v, want %q", updated.UpstreamURL, url)
	}
}

func TestUpdate_ClearUpstreamURL_TransitionsToPending(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	url := "https://agent.example.com/invoke"
	a := &agent.Agent{Name: "was-active", UpstreamURL: &url}
	mustCreate(t, s, tenantA, a)

	if a.Status != agent.StatusActive {
		t.Fatalf("precondition: Status = %q, want active", a.Status)
	}

	var nilURL *string
	updated, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{
		UpstreamURL: &nilURL,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != agent.StatusPending {
		t.Errorf("Status after clear UpstreamURL = %q, want %q", updated.Status, agent.StatusPending)
	}
	if updated.UpstreamURL != nil {
		t.Errorf("UpstreamURL = %v, want nil", updated.UpstreamURL)
	}
}

func TestList_IncludesPendingAgents(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	url := "https://agent.example.com"
	mustCreate(t, s, tenantA, &agent.Agent{Name: "active-agent", UpstreamURL: &url})
	mustCreate(t, s, tenantA, &agent.Agent{Name: "pending-agent"})

	agents, total, err := s.List(context.Background(), tenantA, 1, 50, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 (both active and pending)", total)
	}
	if len(agents) != 2 {
		t.Errorf("len(agents) = %d, want 2", len(agents))
	}

	statusCounts := map[agent.Status]int{}
	for _, a := range agents {
		statusCounts[a.Status]++
	}
	if statusCounts[agent.StatusActive] != 1 {
		t.Errorf("active count = %d, want 1", statusCounts[agent.StatusActive])
	}
	if statusCounts[agent.StatusPending] != 1 {
		t.Errorf("pending count = %d, want 1", statusCounts[agent.StatusPending])
	}
}

func TestDelete_PendingAgent(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "pending-to-delete"}
	mustCreate(t, s, tenantA, a)

	if a.Status != agent.StatusPending {
		t.Fatalf("precondition: Status = %q, want pending", a.Status)
	}

	if err := s.Delete(context.Background(), tenantA, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if got.Status != agent.StatusInactive {
		t.Errorf("Status = %q, want %q", got.Status, agent.StatusInactive)
	}
}

// ------------------------------------------------------ protocol / mcp (spec 0004) ---

func TestCreate_ProtocolDefaultsToHTTP(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "no-protocol-given"}
	mustCreate(t, s, tenantA, a)

	if a.Protocol != "http" {
		t.Errorf("Protocol = %q, want %q", a.Protocol, "http")
	}
	if a.MCPTransport != nil {
		t.Errorf("MCPTransport = %v, want nil", a.MCPTransport)
	}
}

func TestCreate_MCPAgent_RoundTrips(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	transport := "streamable_http"
	version := "2025-06-18"
	a := &agent.Agent{
		Name:               "mcp-agent",
		Protocol:           "mcp",
		MCPTransport:       &transport,
		MCPProtocolVersion: &version,
	}
	mustCreate(t, s, tenantA, a)

	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Protocol != "mcp" {
		t.Errorf("Protocol = %q, want %q", got.Protocol, "mcp")
	}
	if got.MCPTransport == nil || *got.MCPTransport != transport {
		t.Errorf("MCPTransport = %v, want %q", got.MCPTransport, transport)
	}
	if got.MCPProtocolVersion == nil || *got.MCPProtocolVersion != version {
		t.Errorf("MCPProtocolVersion = %v, want %q", got.MCPProtocolVersion, version)
	}
}

func TestUpdate_ProtocolTransitionHTTPToMCP(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "http-agent"}
	mustCreate(t, s, tenantA, a)

	protocol := "mcp"
	transport := "streamable_http"
	transportPtr := &transport
	updated, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{
		Protocol:     &protocol,
		MCPTransport: &transportPtr,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Protocol != "mcp" {
		t.Errorf("Protocol = %q, want %q", updated.Protocol, "mcp")
	}
	if updated.MCPTransport == nil || *updated.MCPTransport != "streamable_http" {
		t.Errorf("MCPTransport = %v, want streamable_http", updated.MCPTransport)
	}
}

func TestUpdate_ClearMCPTransport(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	transport := "streamable_http"
	a := &agent.Agent{Name: "mcp-to-http", Protocol: "mcp", MCPTransport: &transport}
	mustCreate(t, s, tenantA, a)

	protocol := "http"
	var nilTransport *string
	updated, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{
		Protocol:     &protocol,
		MCPTransport: &nilTransport,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Protocol != "http" {
		t.Errorf("Protocol = %q, want %q", updated.Protocol, "http")
	}
	if updated.MCPTransport != nil {
		t.Errorf("MCPTransport = %v, want nil", updated.MCPTransport)
	}
}

// ------------------------------------------------------ pricing / x402 (spec 0005) ---

func TestCreate_UnpricedByDefault(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "unpriced"}
	mustCreate(t, s, tenantA, a)

	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Pricing != nil {
		t.Errorf("Pricing = %+v, want nil", got.Pricing)
	}
}

func TestCreate_Pricing_RoundTrips(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{
		Name:     "priced-mcp",
		Protocol: "mcp",
		Pricing: &agent.Pricing{
			Amount:  "10000",
			Asset:   "USDC",
			Network: "base",
			PayTo:   "0x0000000000000000000000000000000000000001",
			Tools:   map[string]string{"search_docs": "50000"},
		},
	}
	mustCreate(t, s, tenantA, a)

	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Pricing == nil {
		t.Fatalf("Pricing = nil, want non-nil")
	}
	if got.Pricing.Amount != "10000" || got.Pricing.Asset != "USDC" ||
		got.Pricing.Network != "base" ||
		got.Pricing.PayTo != "0x0000000000000000000000000000000000000001" {
		t.Errorf("Pricing = %+v, unexpected scalar fields", got.Pricing)
	}
	if got.Pricing.Tools["search_docs"] != "50000" {
		t.Errorf("Tools = %v, want search_docs=50000", got.Pricing.Tools)
	}
}

// TestPricing_DeepCopy verifies the store returns a deep copy: mutating a
// returned Pricing (including its Tools map) must not affect stored state.
func TestPricing_DeepCopy(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{
		Name: "priced",
		Pricing: &agent.Pricing{
			Amount:  "1",
			Asset:   "USDC",
			Network: "base",
			PayTo:   "0x0000000000000000000000000000000000000002",
			Tools:   map[string]string{"t": "2"},
		},
	}
	mustCreate(t, s, tenantA, a)

	// Mutate the caller-held struct after create; stored state must be unaffected.
	a.Pricing.Amount = "999"
	a.Pricing.Tools["t"] = "999"

	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Pricing.Amount != "1" {
		t.Errorf("Amount = %q, want 1 (stored state mutated via shared struct)", got.Pricing.Amount)
	}
	if got.Pricing.Tools["t"] != "2" {
		t.Errorf("Tools[t] = %q, want 2 (stored map mutated via shared reference)", got.Pricing.Tools["t"])
	}

	// Mutating the returned copy must not affect a subsequent Get either.
	got.Pricing.Tools["t"] = "abc"
	again, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get again: %v", err)
	}
	if again.Pricing.Tools["t"] != "2" {
		t.Errorf("Tools[t] = %q, want 2 (returned copy shares stored map)", again.Pricing.Tools["t"])
	}
}

func TestUpdate_SetAndClearPricing(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	a := &agent.Agent{Name: "to-be-priced"}
	mustCreate(t, s, tenantA, a)

	// Set pricing.
	p := &agent.Pricing{
		Amount:  "5000",
		Asset:   "USDC",
		Network: "base",
		PayTo:   "0x0000000000000000000000000000000000000003",
	}
	updated, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{Pricing: &p})
	if err != nil {
		t.Fatalf("Update set: %v", err)
	}
	if updated.Pricing == nil || updated.Pricing.Amount != "5000" {
		t.Fatalf("Pricing = %+v, want amount 5000", updated.Pricing)
	}

	// Leave pricing unchanged (absent field).
	unchanged, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{})
	if err != nil {
		t.Fatalf("Update unchanged: %v", err)
	}
	if unchanged.Pricing == nil || unchanged.Pricing.Amount != "5000" {
		t.Errorf("Pricing = %+v, want unchanged amount 5000", unchanged.Pricing)
	}

	// Clear pricing.
	var nilPricing *agent.Pricing
	cleared, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{Pricing: &nilPricing})
	if err != nil {
		t.Fatalf("Update clear: %v", err)
	}
	if cleared.Pricing != nil {
		t.Errorf("Pricing = %+v, want nil after clear", cleared.Pricing)
	}
}
