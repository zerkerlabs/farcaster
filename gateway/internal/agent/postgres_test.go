//go:build integration

package agent_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/farcaster/gateway/db"
	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
)

// openTestPool opens a connection pool from TEST_DATABASE_URL and runs
// migrations. It returns the pool and registers cleanup via t.Cleanup.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration tests")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Truncate agents so each test starts clean. CASCADE is required because the
	// invocations table (migration 003) has a foreign key to agents; a plain
	// TRUNCATE is rejected while that reference exists.
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE agents CASCADE`); err != nil {
		t.Fatalf("truncate agents: %v", err)
	}

	return pool
}

// newPGStore returns a PostgresStore backed by a freshly migrated database.
func newPGStore(t *testing.T) *agent.PostgresStore {
	t.Helper()
	return agent.NewPostgresStore(openTestPool(t))
}

// pgMustCreate is the same as mustCreate but for PostgresStore.
func pgMustCreate(t *testing.T, s *agent.PostgresStore, tenantID string, a *agent.Agent) {
	t.Helper()
	if err := s.Create(context.Background(), tenantID, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// ------------------------------------------------------------------ Create ---

func TestPG_Create_AssignsServerFields(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "bot-1", CreatedBy: "user-x"}
	pgMustCreate(t, s, tenantA, a)

	if !strings.HasPrefix(a.ID, "agt_") {
		t.Errorf("ID %q does not start with agt_", a.ID)
	}
	if a.TenantID != tenantA {
		t.Errorf("TenantID = %q, want %q", a.TenantID, tenantA)
	}
	// Spec 0002: an agent created without an upstream_url is pending, not active.
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

func TestPG_Create_WithUpstreamURL_Active(t *testing.T) {
	s := newPGStore(t)
	url := "https://agent.example.com/invoke"
	a := &agent.Agent{Name: "routable", CreatedBy: "user-x", UpstreamURL: &url}
	pgMustCreate(t, s, tenantA, a)

	// Spec 0002: a non-nil upstream_url makes the agent active and invocable.
	if a.Status != agent.StatusActive {
		t.Errorf("Status = %q, want %q", a.Status, agent.StatusActive)
	}
	if a.UpstreamURL == nil || *a.UpstreamURL != url {
		t.Errorf("UpstreamURL = %v, want %q", a.UpstreamURL, url)
	}
}

func TestPG_Create_DuplicateNameSameTenant(t *testing.T) {
	s := newPGStore(t)
	pgMustCreate(t, s, tenantA, &agent.Agent{Name: "clash"})

	err := s.Create(context.Background(), tenantA, &agent.Agent{Name: "clash"})
	if !errors.Is(err, agent.ErrNameConflict) {
		t.Errorf("err = %v, want ErrNameConflict", err)
	}
}

func TestPG_Create_SameNameDifferentTenants(t *testing.T) {
	s := newPGStore(t)
	pgMustCreate(t, s, tenantA, &agent.Agent{Name: "shared-name"})

	if err := s.Create(context.Background(), tenantB, &agent.Agent{Name: "shared-name"}); err != nil {
		t.Errorf("cross-tenant name reuse should succeed, got: %v", err)
	}
}

func TestPG_Create_ReuseNameAfterSoftDelete(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "recycled"}
	pgMustCreate(t, s, tenantA, a)

	if err := s.Delete(context.Background(), tenantA, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Create(context.Background(), tenantA, &agent.Agent{Name: "recycled"}); err != nil {
		t.Errorf("name reuse after soft-delete should succeed, got: %v", err)
	}
}

// --------------------------------------------------------------------- Get ---

func TestPG_Get_Found(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "lookup-me", Description: "desc", Tags: []string{"a", "b"}}
	pgMustCreate(t, s, tenantA, a)

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
	if len(got.Tags) != 2 || got.Tags[0] != "a" || got.Tags[1] != "b" {
		t.Errorf("Tags = %v, want [a b]", got.Tags)
	}
}

func TestPG_Get_NotFound(t *testing.T) {
	s := newPGStore(t)
	_, err := s.Get(context.Background(), tenantA, "agt_nonexistent")
	if !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPG_Get_CrossTenantBlocked(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "secret"}
	pgMustCreate(t, s, tenantA, a)

	_, err := s.Get(context.Background(), tenantB, a.ID)
	if !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("cross-tenant get: err = %v, want ErrNotFound", err)
	}
}

func TestPG_Get_InactiveAgentVisible(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "deleted"}
	pgMustCreate(t, s, tenantA, a)
	if err := s.Delete(context.Background(), tenantA, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get on inactive agent: %v", err)
	}
	if got.Status != agent.StatusInactive {
		t.Errorf("Status = %q, want %q", got.Status, agent.StatusInactive)
	}
}

// -------------------------------------------------------------------- List ---

func TestPG_List_Empty(t *testing.T) {
	s := newPGStore(t)
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

func TestPG_List_ActiveOnly(t *testing.T) {
	s := newPGStore(t)
	active := &agent.Agent{Name: "active-one"}
	deleted := &agent.Agent{Name: "deleted-one"}
	pgMustCreate(t, s, tenantA, active)
	pgMustCreate(t, s, tenantA, deleted)
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

func TestPG_List_CrossTenantIsolation(t *testing.T) {
	s := newPGStore(t)
	pgMustCreate(t, s, tenantA, &agent.Agent{Name: "a-only"})
	pgMustCreate(t, s, tenantB, &agent.Agent{Name: "b-only"})

	agentsA, totalA, err := s.List(context.Background(), tenantA, 1, 50, "")
	if err != nil {
		t.Fatalf("List(tenantA): %v", err)
	}
	if totalA != 1 || len(agentsA) != 1 {
		t.Errorf("tenantA: total=%d len=%d, want 1/1", totalA, len(agentsA))
	}
	if agentsA[0].Name != "a-only" {
		t.Errorf("tenantA agent name = %q, want a-only", agentsA[0].Name)
	}

	agentsB, totalB, err := s.List(context.Background(), tenantB, 1, 50, "")
	if err != nil {
		t.Fatalf("List(tenantB): %v", err)
	}
	if totalB != 1 || len(agentsB) != 1 {
		t.Errorf("tenantB: total=%d len=%d, want 1/1", totalB, len(agentsB))
	}
	if agentsB[0].Name != "b-only" {
		t.Errorf("tenantB agent name = %q, want b-only", agentsB[0].Name)
	}
}

func TestPG_List_Pagination(t *testing.T) {
	s := newPGStore(t)
	for i := range 5 {
		pgMustCreate(t, s, tenantA, &agent.Agent{Name: strings.Repeat("x", i+1)})
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

func TestPG_List_ProtocolFilter(t *testing.T) {
	s := newPGStore(t)
	httpAgent := &agent.Agent{Name: "http-agent"}
	pgMustCreate(t, s, tenantA, httpAgent)
	mcpAgent := &agent.Agent{Name: "mcp-agent", Protocol: "mcp"}
	pgMustCreate(t, s, tenantA, mcpAgent)

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

func TestPG_Update_MutableFields(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{
		Name:        "before",
		Description: "old desc",
		Tags:        []string{"old"},
		Metadata:    map[string]any{"k": "v1"},
	}
	pgMustCreate(t, s, tenantA, a)

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
		t.Errorf("Name = %q, want after", updated.Name)
	}
	if updated.Description != "new desc" {
		t.Errorf("Description = %q, want new desc", updated.Description)
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

func TestPG_Update_PartialFields(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "partial", Description: "keep-me"}
	pgMustCreate(t, s, tenantA, a)

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
		t.Errorf("Description = %q, want changed", updated.Description)
	}
}

func TestPG_Update_NotFound(t *testing.T) {
	s := newPGStore(t)
	name := "x"
	_, err := s.Update(context.Background(), tenantA, "agt_missing", agent.UpdateFields{Name: &name})
	if !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPG_Update_CrossTenantBlocked(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "owned-by-a"}
	pgMustCreate(t, s, tenantA, a)

	name := "hijack"
	_, err := s.Update(context.Background(), tenantB, a.ID, agent.UpdateFields{Name: &name})
	if !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("cross-tenant update: err = %v, want ErrNotFound", err)
	}
}

func TestPG_Update_NameConflict(t *testing.T) {
	s := newPGStore(t)
	pgMustCreate(t, s, tenantA, &agent.Agent{Name: "taken"})
	a := &agent.Agent{Name: "free"}
	pgMustCreate(t, s, tenantA, a)

	name := "taken"
	_, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{Name: &name})
	if !errors.Is(err, agent.ErrNameConflict) {
		t.Errorf("err = %v, want ErrNameConflict", err)
	}
}

func TestPG_Update_SameNameNoConflict(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "same"}
	pgMustCreate(t, s, tenantA, a)

	name := "same"
	if _, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{Name: &name}); err != nil {
		t.Errorf("updating to same name should not conflict: %v", err)
	}
}

func TestPG_Update_OnSoftDeletedReturnsNotFound(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "gone"}
	pgMustCreate(t, s, tenantA, a)

	if err := s.Delete(context.Background(), tenantA, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	desc := "should not apply"
	if _, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{Description: &desc}); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("Update on soft-deleted = %v, want ErrNotFound", err)
	}
}

// ------------------------------------------------------------------ Delete ---

func TestPG_Delete_SoftDeletes(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "bye"}
	pgMustCreate(t, s, tenantA, a)

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

	agents, total, err := s.List(context.Background(), tenantA, 1, 50, "")
	if err != nil {
		t.Fatalf("List after Delete: %v", err)
	}
	if total != 0 || len(agents) != 0 {
		t.Errorf("deleted agent appears in list: total=%d len=%d", total, len(agents))
	}
}

func TestPG_Delete_NotFound(t *testing.T) {
	s := newPGStore(t)
	if err := s.Delete(context.Background(), tenantA, "agt_ghost"); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPG_Delete_CrossTenantBlocked(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "protected"}
	pgMustCreate(t, s, tenantA, a)

	if err := s.Delete(context.Background(), tenantB, a.ID); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("cross-tenant delete: err = %v, want ErrNotFound", err)
	}

	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get after cross-tenant delete attempt: %v", err)
	}
	// "protected" was created without an upstream_url, so it is pending; a
	// blocked cross-tenant delete must leave it untouched (still pending).
	if got.Status != agent.StatusPending {
		t.Errorf("Status = %q after blocked delete, want %q", got.Status, agent.StatusPending)
	}
}

func TestPG_Delete_AlreadyInactive(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "once"}
	pgMustCreate(t, s, tenantA, a)
	if err := s.Delete(context.Background(), tenantA, a.ID); err != nil {
		t.Fatalf("first Delete: %v", err)
	}

	if err := s.Delete(context.Background(), tenantA, a.ID); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("second Delete: err = %v, want ErrNotFound", err)
	}
}

// ------------------------------------------------------ protocol / mcp (spec 0004) ---

func TestPG_Create_ProtocolDefaultsToHTTP(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "no-protocol-given"}
	pgMustCreate(t, s, tenantA, a)

	if a.Protocol != "http" {
		t.Errorf("Protocol = %q, want %q", a.Protocol, "http")
	}
	if a.MCPTransport != nil {
		t.Errorf("MCPTransport = %v, want nil", a.MCPTransport)
	}
}

func TestPG_Create_MCPAgent_RoundTrips(t *testing.T) {
	s := newPGStore(t)
	transport := "streamable_http"
	version := "2025-06-18"
	a := &agent.Agent{
		Name:               "mcp-agent",
		Protocol:           "mcp",
		MCPTransport:       &transport,
		MCPProtocolVersion: &version,
	}
	pgMustCreate(t, s, tenantA, a)

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

func TestPG_Update_ProtocolTransitionHTTPToMCP(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "http-agent"}
	pgMustCreate(t, s, tenantA, a)

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

func TestPG_Update_ClearMCPTransport(t *testing.T) {
	s := newPGStore(t)
	transport := "streamable_http"
	a := &agent.Agent{Name: "mcp-to-http", Protocol: "mcp", MCPTransport: &transport}
	pgMustCreate(t, s, tenantA, a)

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

// TestPG_Get_PreMigrationRowDefaultsToHTTP verifies that a row inserted before
// migration 010 (protocol/mcp_transport/mcp_protocol_version all NULL, as if
// the columns had just been added with no backfill) scans as an ordinary
// "http" agent — the acceptance bullet "existing http agents unaffected".
func TestPG_Get_PreMigrationRowDefaultsToHTTP(t *testing.T) {
	pool := openTestPool(t)
	s := agent.NewPostgresStore(pool)

	a := &agent.Agent{Name: "legacy-agent"}
	if err := s.Create(context.Background(), tenantA, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate a pre-migration row by nulling out the new columns directly.
	if _, err := pool.Exec(
		context.Background(),
		`UPDATE agents SET protocol=NULL, mcp_transport=NULL, mcp_protocol_version=NULL WHERE id=$1`,
		a.ID,
	); err != nil {
		t.Fatalf("simulate pre-migration row: %v", err)
	}

	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Protocol != "http" {
		t.Errorf("Protocol = %q, want %q", got.Protocol, "http")
	}
	if got.MCPTransport != nil {
		t.Errorf("MCPTransport = %v, want nil", got.MCPTransport)
	}
	if got.MCPProtocolVersion != nil {
		t.Errorf("MCPProtocolVersion = %v, want nil", got.MCPProtocolVersion)
	}
}

// ------------------------------------------------------ pricing / x402 (spec 0005) ---

func TestPG_Create_UnpricedByDefault(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "unpriced"}
	pgMustCreate(t, s, tenantA, a)

	got, err := s.Get(context.Background(), tenantA, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Pricing != nil {
		t.Errorf("Pricing = %+v, want nil", got.Pricing)
	}
}

func TestPG_Create_Pricing_RoundTrips(t *testing.T) {
	s := newPGStore(t)
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
	pgMustCreate(t, s, tenantA, a)

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

func TestPG_Update_SetAndClearPricing(t *testing.T) {
	s := newPGStore(t)
	a := &agent.Agent{Name: "to-be-priced"}
	pgMustCreate(t, s, tenantA, a)

	p := &agent.Pricing{
		Amount:  "5000",
		Asset:   "USDC",
		Network: "base",
		PayTo:   "0x0000000000000000000000000000000000000003",
		Tools:   map[string]string{"t": "7"},
	}
	updated, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{Pricing: &p})
	if err != nil {
		t.Fatalf("Update set: %v", err)
	}
	if updated.Pricing == nil || updated.Pricing.Amount != "5000" || updated.Pricing.Tools["t"] != "7" {
		t.Fatalf("Pricing = %+v, want amount 5000 and tools t=7", updated.Pricing)
	}

	var nilPricing *agent.Pricing
	cleared, err := s.Update(context.Background(), tenantA, a.ID, agent.UpdateFields{Pricing: &nilPricing})
	if err != nil {
		t.Fatalf("Update clear: %v", err)
	}
	if cleared.Pricing != nil {
		t.Errorf("Pricing = %+v, want nil after clear", cleared.Pricing)
	}
}
