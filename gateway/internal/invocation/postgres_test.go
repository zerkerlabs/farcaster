//go:build integration

package invocation_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/gateway/gateway/db"
	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
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

	if _, err := pool.Exec(ctx, `TRUNCATE TABLE invocations, agents`); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}

	// Insert a minimal agent so FK constraints are satisfied.
	for _, ag := range []struct{ id, tenant string }{
		{agentID, tenantA},
		{agentB, tenantA},
		{agentID, tenantB},
	} {
		if _, err := pool.Exec(
			ctx,
			`INSERT INTO agents (id, tenant_id, name, status) VALUES ($1, $2, $3, 'active')
			 ON CONFLICT (id) DO NOTHING`,
			ag.id, ag.tenant, "test-agent-"+ag.id,
		); err != nil {
			t.Fatalf("insert test agent: %v", err)
		}
	}

	return pool
}

func newPGStore(t *testing.T) *invocation.PostgresStore {
	t.Helper()
	return invocation.NewPostgresStore(openTestPool(t))
}

func pgMustCreate(t *testing.T, s *invocation.PostgresStore, tenantID string, inv *invocation.Invocation) {
	t.Helper()
	if err := s.Create(context.Background(), tenantID, inv); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// ------------------------------------------------------------------ Create ---

func TestPG_Create_AssignsServerFields(t *testing.T) {
	s := newPGStore(t)
	inv := &invocation.Invocation{
		AgentID: agentID,
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusPending,
	}
	pgMustCreate(t, s, tenantA, inv)

	if inv.ID == "" || inv.ID[:4] != "inv_" {
		t.Errorf("ID %q does not start with inv_", inv.ID)
	}
	if inv.TenantID != tenantA {
		t.Errorf("TenantID = %q, want %q", inv.TenantID, tenantA)
	}
	if inv.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if inv.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
}

// Regression: Create must persist RequestSize (the INSERT previously omitted the
// req_size column, so the caller's value was silently dropped).
func TestPG_Create_PreservesRequestSize(t *testing.T) {
	s := newPGStore(t)
	size := int64(2048)
	inv := &invocation.Invocation{
		AgentID:     agentID,
		Mode:        invocation.ModeTransactional,
		Status:      invocation.StatusPending,
		RequestSize: &size,
	}
	pgMustCreate(t, s, tenantA, inv)

	if inv.RequestSize == nil || *inv.RequestSize != size {
		t.Fatalf("RequestSize after Create = %v, want %d", inv.RequestSize, size)
	}
	// Confirm it round-trips from storage, not just the returned struct.
	got, err := s.Get(context.Background(), tenantA, inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RequestSize == nil || *got.RequestSize != size {
		t.Errorf("RequestSize after Get = %v, want %d", got.RequestSize, size)
	}
}

func TestPG_Create_BodyFieldsNilByDefault(t *testing.T) {
	s := newPGStore(t)
	inv := &invocation.Invocation{
		AgentID: agentID,
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusPending,
	}
	pgMustCreate(t, s, tenantA, inv)

	if inv.RequestBody != nil {
		t.Errorf("RequestBody = %v, want nil", inv.RequestBody)
	}
	if inv.ResponseBody != nil {
		t.Errorf("ResponseBody = %v, want nil", inv.ResponseBody)
	}
	if inv.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil", inv.CompletedAt)
	}
}

func TestPG_Create_WithRequestBody(t *testing.T) {
	s := newPGStore(t)
	body := []byte(`{"input":"hello"}`)
	inv := &invocation.Invocation{
		AgentID:     agentID,
		Mode:        invocation.ModeTransactional,
		Status:      invocation.StatusPending,
		RequestBody: body,
	}
	pgMustCreate(t, s, tenantA, inv)

	got, err := s.Get(context.Background(), tenantA, inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got.RequestBody, body) {
		t.Errorf("RequestBody = %q, want %q", got.RequestBody, body)
	}
}

// --------------------------------------------------------------------- Get ---

func TestPG_Get_Found(t *testing.T) {
	s := newPGStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending}
	pgMustCreate(t, s, tenantA, inv)

	got, err := s.Get(context.Background(), tenantA, inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != inv.ID {
		t.Errorf("ID = %q, want %q", got.ID, inv.ID)
	}
}

func TestPG_Get_NotFound(t *testing.T) {
	s := newPGStore(t)
	_, err := s.Get(context.Background(), tenantA, "inv_nonexistent")
	if !errors.Is(err, invocation.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPG_Get_CrossTenantBlocked(t *testing.T) {
	s := newPGStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending}
	pgMustCreate(t, s, tenantA, inv)

	_, err := s.Get(context.Background(), tenantB, inv.ID)
	if !errors.Is(err, invocation.ErrNotFound) {
		t.Errorf("cross-tenant get: err = %v, want ErrNotFound", err)
	}
}

// -------------------------------------------------------------------- List ---

func TestPG_List_Empty(t *testing.T) {
	s := newPGStore(t)
	invs, total, err := s.List(context.Background(), tenantA, agentID, 1, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 0 || len(invs) != 0 {
		t.Errorf("total=%d len=%d, want 0/0", total, len(invs))
	}
}

func TestPG_List_ByAgent(t *testing.T) {
	s := newPGStore(t)
	pgMustCreate(t, s, tenantA, &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending})
	pgMustCreate(t, s, tenantA, &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending})
	pgMustCreate(t, s, tenantA, &invocation.Invocation{AgentID: agentB, Mode: invocation.ModeTransactional, Status: invocation.StatusPending})

	invs, total, err := s.List(context.Background(), tenantA, agentID, 1, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 || len(invs) != 2 {
		t.Errorf("total=%d len=%d, want 2/2", total, len(invs))
	}
}

func TestPG_List_Pagination(t *testing.T) {
	s := newPGStore(t)
	for range 5 {
		pgMustCreate(t, s, tenantA, &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending})
	}

	page1, total, err := s.List(context.Background(), tenantA, agentID, 1, 2)
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if total != 5 || len(page1) != 2 {
		t.Errorf("total=%d len=%d, want 5/2", total, len(page1))
	}

	page3, _, err := s.List(context.Background(), tenantA, agentID, 3, 2)
	if err != nil {
		t.Fatalf("List page 3: %v", err)
	}
	if len(page3) != 1 {
		t.Errorf("page 3 len = %d, want 1", len(page3))
	}
}

// ------------------------------------------------------------------ Update ---

func TestPG_Update_CompletionFields(t *testing.T) {
	s := newPGStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusRunning}
	pgMustCreate(t, s, tenantA, inv)

	succeeded := invocation.StatusSucceeded
	completedAt := time.Now().UTC().Truncate(time.Microsecond) // Postgres TIMESTAMPTZ precision
	upstreamStatus := 200
	latencyMS := int64(55)
	reqSize := int64(128)
	respSize := int64(256)

	updated, err := s.Update(context.Background(), tenantA, inv.ID, invocation.UpdateFields{
		Status:         &succeeded,
		CompletedAt:    &completedAt,
		UpstreamStatus: &upstreamStatus,
		LatencyMS:      &latencyMS,
		RequestSize:    &reqSize,
		ResponseSize:   &respSize,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != invocation.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", updated.Status)
	}
	if updated.CompletedAt == nil || !updated.CompletedAt.Equal(completedAt) {
		t.Errorf("CompletedAt = %v, want %v", updated.CompletedAt, completedAt)
	}
	if updated.UpstreamStatus == nil || *updated.UpstreamStatus != 200 {
		t.Errorf("UpstreamStatus = %v, want 200", updated.UpstreamStatus)
	}
	if updated.LatencyMS == nil || *updated.LatencyMS != 55 {
		t.Errorf("LatencyMS = %v, want 55", updated.LatencyMS)
	}
}

func TestPG_Update_WithBodies(t *testing.T) {
	s := newPGStore(t)
	reqBody := []byte(`{"input":"question"}`)
	inv := &invocation.Invocation{
		AgentID:     agentID,
		Mode:        invocation.ModeTransactional,
		Status:      invocation.StatusRunning,
		RequestBody: reqBody,
	}
	pgMustCreate(t, s, tenantA, inv)

	succeeded := invocation.StatusSucceeded
	respBody := []byte(`{"output":"answer"}`)
	updated, err := s.Update(context.Background(), tenantA, inv.ID, invocation.UpdateFields{
		Status:       &succeeded,
		ResponseBody: &respBody,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !bytes.Equal(updated.RequestBody, reqBody) {
		t.Errorf("RequestBody = %q, want %q", updated.RequestBody, reqBody)
	}
	if !bytes.Equal(updated.ResponseBody, respBody) {
		t.Errorf("ResponseBody = %q, want %q", updated.ResponseBody, respBody)
	}
}

func TestPG_Update_NotFound(t *testing.T) {
	s := newPGStore(t)
	running := invocation.StatusRunning
	_, err := s.Update(context.Background(), tenantA, "inv_missing", invocation.UpdateFields{Status: &running})
	if !errors.Is(err, invocation.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPG_Create_NewObservabilityFieldsNilByDefault(t *testing.T) {
	s := newPGStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending}
	pgMustCreate(t, s, tenantA, inv)

	if inv.TTFTMS != nil {
		t.Errorf("TTFTMS = %v, want nil", inv.TTFTMS)
	}
	if inv.ErrorClass != nil {
		t.Errorf("ErrorClass = %v, want nil", inv.ErrorClass)
	}
	if inv.Model != nil {
		t.Errorf("Model = %v, want nil", inv.Model)
	}
	if inv.MCPMethod != nil {
		t.Errorf("MCPMethod = %v, want nil", inv.MCPMethod)
	}
	if inv.MCPTool != nil {
		t.Errorf("MCPTool = %v, want nil", inv.MCPTool)
	}
	if inv.PaymentNetwork != nil {
		t.Errorf("PaymentNetwork = %v, want nil", inv.PaymentNetwork)
	}
	if inv.PaymentAsset != nil {
		t.Errorf("PaymentAsset = %v, want nil", inv.PaymentAsset)
	}
	if inv.PaymentAmount != nil {
		t.Errorf("PaymentAmount = %v, want nil", inv.PaymentAmount)
	}
	if inv.PaymentPayer != nil {
		t.Errorf("PaymentPayer = %v, want nil", inv.PaymentPayer)
	}
	if inv.PaymentNonce != nil {
		t.Errorf("PaymentNonce = %v, want nil", inv.PaymentNonce)
	}
	if inv.SettlementStatus != nil {
		t.Errorf("SettlementStatus = %v, want nil", inv.SettlementStatus)
	}
	if inv.SettlementTxHash != nil {
		t.Errorf("SettlementTxHash = %v, want nil", inv.SettlementTxHash)
	}
	if inv.SettledAmount != nil {
		t.Errorf("SettledAmount = %v, want nil", inv.SettledAmount)
	}
	if inv.OperatorAmount != nil {
		t.Errorf("OperatorAmount = %v, want nil", inv.OperatorAmount)
	}
	if inv.FacilitatorFee != nil {
		t.Errorf("FacilitatorFee = %v, want nil", inv.FacilitatorFee)
	}
	if inv.SettlementAttempts != nil {
		t.Errorf("SettlementAttempts = %v, want nil", inv.SettlementAttempts)
	}
	if inv.SettlementReason != nil {
		t.Errorf("SettlementReason = %v, want nil", inv.SettlementReason)
	}
	if inv.SettledAt != nil {
		t.Errorf("SettledAt = %v, want nil", inv.SettledAt)
	}
}

func TestPG_Update_NewObservabilityFields(t *testing.T) {
	s := newPGStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeStreaming, Status: invocation.StatusRunning}
	pgMustCreate(t, s, tenantA, inv)

	failed := invocation.StatusFailed
	ttft := int64(150)
	ec := invocation.ErrorClassUpstream5xx
	model := "claude-sonnet-4-6"
	mcpMethod := "tools/call"
	mcpTool := "search_docs"
	paymentNetwork := "base"
	paymentAsset := "USDC"
	paymentAmount := "10000"
	paymentPayer := "0xPayer..."
	paymentNonce := "0xNonce..."
	settlementStatus := invocation.SettlementStatusSettled
	settlementTxHash := "0xTx..."
	settledAmount := "10000"
	operatorAmount := "9900"
	facilitatorFee := "100"
	settlementAttempts := 1
	settlementReason := ""
	settledAt := time.Now().UTC().Truncate(time.Second)

	updated, err := s.Update(context.Background(), tenantA, inv.ID, invocation.UpdateFields{
		Status:             &failed,
		TTFTMS:             &ttft,
		ErrorClass:         &ec,
		Model:              &model,
		MCPMethod:          &mcpMethod,
		MCPTool:            &mcpTool,
		PaymentNetwork:     &paymentNetwork,
		PaymentAsset:       &paymentAsset,
		PaymentAmount:      &paymentAmount,
		PaymentPayer:       &paymentPayer,
		PaymentNonce:       &paymentNonce,
		SettlementStatus:   &settlementStatus,
		SettlementTxHash:   &settlementTxHash,
		SettledAmount:      &settledAmount,
		OperatorAmount:     &operatorAmount,
		FacilitatorFee:     &facilitatorFee,
		SettlementAttempts: &settlementAttempts,
		SettlementReason:   &settlementReason,
		SettledAt:          &settledAt,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.TTFTMS == nil || *updated.TTFTMS != 150 {
		t.Errorf("TTFTMS = %v, want 150", updated.TTFTMS)
	}
	if updated.ErrorClass == nil || *updated.ErrorClass != invocation.ErrorClassUpstream5xx {
		t.Errorf("ErrorClass = %v, want %q", updated.ErrorClass, invocation.ErrorClassUpstream5xx)
	}
	if updated.Model == nil || *updated.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %v, want %q", updated.Model, "claude-sonnet-4-6")
	}
	if updated.MCPMethod == nil || *updated.MCPMethod != "tools/call" {
		t.Errorf("MCPMethod = %v, want %q", updated.MCPMethod, "tools/call")
	}
	if updated.MCPTool == nil || *updated.MCPTool != "search_docs" {
		t.Errorf("MCPTool = %v, want %q", updated.MCPTool, "search_docs")
	}
	if updated.PaymentNetwork == nil || *updated.PaymentNetwork != "base" {
		t.Errorf("PaymentNetwork = %v, want %q", updated.PaymentNetwork, "base")
	}
	if updated.PaymentAsset == nil || *updated.PaymentAsset != "USDC" {
		t.Errorf("PaymentAsset = %v, want %q", updated.PaymentAsset, "USDC")
	}
	if updated.PaymentAmount == nil || *updated.PaymentAmount != "10000" {
		t.Errorf("PaymentAmount = %v, want %q", updated.PaymentAmount, "10000")
	}
	if updated.PaymentPayer == nil || *updated.PaymentPayer != "0xPayer..." {
		t.Errorf("PaymentPayer = %v, want %q", updated.PaymentPayer, "0xPayer...")
	}
	if updated.PaymentNonce == nil || *updated.PaymentNonce != "0xNonce..." {
		t.Errorf("PaymentNonce = %v, want %q", updated.PaymentNonce, "0xNonce...")
	}
	if updated.SettlementStatus == nil || *updated.SettlementStatus != invocation.SettlementStatusSettled {
		t.Errorf("SettlementStatus = %v, want %q", updated.SettlementStatus, invocation.SettlementStatusSettled)
	}
	if updated.SettlementTxHash == nil || *updated.SettlementTxHash != "0xTx..." {
		t.Errorf("SettlementTxHash = %v, want %q", updated.SettlementTxHash, "0xTx...")
	}
	if updated.SettledAmount == nil || *updated.SettledAmount != "10000" {
		t.Errorf("SettledAmount = %v, want %q", updated.SettledAmount, "10000")
	}
	if updated.OperatorAmount == nil || *updated.OperatorAmount != "9900" {
		t.Errorf("OperatorAmount = %v, want %q", updated.OperatorAmount, "9900")
	}
	if updated.FacilitatorFee == nil || *updated.FacilitatorFee != "100" {
		t.Errorf("FacilitatorFee = %v, want %q", updated.FacilitatorFee, "100")
	}
	if updated.SettlementAttempts == nil || *updated.SettlementAttempts != 1 {
		t.Errorf("SettlementAttempts = %v, want 1", updated.SettlementAttempts)
	}
	if updated.SettlementReason == nil || *updated.SettlementReason != settlementReason {
		t.Errorf("SettlementReason = %v, want %q", updated.SettlementReason, settlementReason)
	}
	if updated.SettledAt == nil || !updated.SettledAt.Equal(settledAt) {
		t.Errorf("SettledAt = %v, want %v", updated.SettledAt, settledAt)
	}

	// Confirm round-trip from storage.
	got, err := s.Get(context.Background(), tenantA, inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TTFTMS == nil || *got.TTFTMS != 150 {
		t.Errorf("stored TTFTMS = %v, want 150", got.TTFTMS)
	}
	if got.ErrorClass == nil || *got.ErrorClass != invocation.ErrorClassUpstream5xx {
		t.Errorf("stored ErrorClass = %v, want %q", got.ErrorClass, invocation.ErrorClassUpstream5xx)
	}
	if got.Model == nil || *got.Model != "claude-sonnet-4-6" {
		t.Errorf("stored Model = %v, want %q", got.Model, "claude-sonnet-4-6")
	}
	if got.MCPMethod == nil || *got.MCPMethod != "tools/call" {
		t.Errorf("stored MCPMethod = %v, want %q", got.MCPMethod, "tools/call")
	}
	if got.MCPTool == nil || *got.MCPTool != "search_docs" {
		t.Errorf("stored MCPTool = %v, want %q", got.MCPTool, "search_docs")
	}
	if got.PaymentNetwork == nil || *got.PaymentNetwork != "base" {
		t.Errorf("stored PaymentNetwork = %v, want %q", got.PaymentNetwork, "base")
	}
	if got.PaymentAsset == nil || *got.PaymentAsset != "USDC" {
		t.Errorf("stored PaymentAsset = %v, want %q", got.PaymentAsset, "USDC")
	}
	if got.PaymentAmount == nil || *got.PaymentAmount != "10000" {
		t.Errorf("stored PaymentAmount = %v, want %q", got.PaymentAmount, "10000")
	}
	if got.PaymentPayer == nil || *got.PaymentPayer != "0xPayer..." {
		t.Errorf("stored PaymentPayer = %v, want %q", got.PaymentPayer, "0xPayer...")
	}
	if got.PaymentNonce == nil || *got.PaymentNonce != "0xNonce..." {
		t.Errorf("stored PaymentNonce = %v, want %q", got.PaymentNonce, "0xNonce...")
	}
	if got.SettlementStatus == nil || *got.SettlementStatus != invocation.SettlementStatusSettled {
		t.Errorf("stored SettlementStatus = %v, want %q", got.SettlementStatus, invocation.SettlementStatusSettled)
	}
	if got.SettlementTxHash == nil || *got.SettlementTxHash != "0xTx..." {
		t.Errorf("stored SettlementTxHash = %v, want %q", got.SettlementTxHash, "0xTx...")
	}
	if got.SettledAmount == nil || *got.SettledAmount != "10000" {
		t.Errorf("stored SettledAmount = %v, want %q", got.SettledAmount, "10000")
	}
	if got.OperatorAmount == nil || *got.OperatorAmount != "9900" {
		t.Errorf("stored OperatorAmount = %v, want %q", got.OperatorAmount, "9900")
	}
	if got.FacilitatorFee == nil || *got.FacilitatorFee != "100" {
		t.Errorf("stored FacilitatorFee = %v, want %q", got.FacilitatorFee, "100")
	}
	if got.SettlementAttempts == nil || *got.SettlementAttempts != 1 {
		t.Errorf("stored SettlementAttempts = %v, want 1", got.SettlementAttempts)
	}
	if got.SettlementReason == nil || *got.SettlementReason != settlementReason {
		t.Errorf("stored SettlementReason = %v, want %q", got.SettlementReason, settlementReason)
	}
	if got.SettledAt == nil || !got.SettledAt.Equal(settledAt) {
		t.Errorf("stored SettledAt = %v, want %v", got.SettledAt, settledAt)
	}
}

func TestPG_Update_CrossTenantBlocked(t *testing.T) {
	s := newPGStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending}
	pgMustCreate(t, s, tenantA, inv)

	running := invocation.StatusRunning
	_, err := s.Update(context.Background(), tenantB, inv.ID, invocation.UpdateFields{Status: &running})
	if !errors.Is(err, invocation.ErrNotFound) {
		t.Errorf("cross-tenant update: err = %v, want ErrNotFound", err)
	}
}
