package invocation_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
)

const (
	tenantA = "tenant-alpha"
	tenantB = "tenant-beta"
	agentID = "agt_00000000-0000-7000-8000-000000000001"
	agentB  = "agt_00000000-0000-7000-8000-000000000002"
)

func newStore(t *testing.T) *invocation.MemoryStore {
	t.Helper()
	return invocation.NewMemoryStore()
}

func mustCreate(t *testing.T, s *invocation.MemoryStore, tenantID string, inv *invocation.Invocation) {
	t.Helper()
	if err := s.Create(context.Background(), tenantID, inv); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// ------------------------------------------------------------------ Create ---

func TestCreate_AssignsServerFields(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv := &invocation.Invocation{
		AgentID: agentID,
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusPending,
	}
	mustCreate(t, s, tenantA, inv)

	if !strings.HasPrefix(inv.ID, "inv_") {
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

func TestCreate_BodyFieldsNilByDefault(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv := &invocation.Invocation{
		AgentID: agentID,
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusPending,
	}
	mustCreate(t, s, tenantA, inv)

	if inv.RequestBody != nil {
		t.Errorf("RequestBody = %v, want nil (body capture off by default)", inv.RequestBody)
	}
	if inv.ResponseBody != nil {
		t.Errorf("ResponseBody = %v, want nil (body capture off by default)", inv.ResponseBody)
	}
	if inv.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil", inv.CompletedAt)
	}
	if inv.UpstreamStatus != nil {
		t.Errorf("UpstreamStatus = %v, want nil", inv.UpstreamStatus)
	}
	if inv.LatencyMS != nil {
		t.Errorf("LatencyMS = %v, want nil", inv.LatencyMS)
	}
}

func TestCreate_WithRequestBody(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	body := []byte(`{"input":"hello"}`)
	inv := &invocation.Invocation{
		AgentID:     agentID,
		Mode:        invocation.ModeTransactional,
		Status:      invocation.StatusPending,
		RequestBody: body,
	}
	mustCreate(t, s, tenantA, inv)

	got, err := s.Get(context.Background(), tenantA, inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got.RequestBody, body) {
		t.Errorf("RequestBody = %q, want %q", got.RequestBody, body)
	}
}

func TestCreate_StreamingMode(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv := &invocation.Invocation{
		AgentID: agentID,
		Mode:    invocation.ModeStreaming,
		Status:  invocation.StatusRunning,
	}
	mustCreate(t, s, tenantA, inv)

	got, err := s.Get(context.Background(), tenantA, inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Mode != invocation.ModeStreaming {
		t.Errorf("Mode = %q, want %q", got.Mode, invocation.ModeStreaming)
	}
	if got.Status != invocation.StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, invocation.StatusRunning)
	}
}

// --------------------------------------------------------------------- Get ---

func TestGet_Found(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv := &invocation.Invocation{
		AgentID: agentID,
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusPending,
	}
	mustCreate(t, s, tenantA, inv)

	got, err := s.Get(context.Background(), tenantA, inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != inv.ID {
		t.Errorf("ID = %q, want %q", got.ID, inv.ID)
	}
	if got.AgentID != agentID {
		t.Errorf("AgentID = %q, want %q", got.AgentID, agentID)
	}
}

func TestGet_NotFound(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	_, err := s.Get(context.Background(), tenantA, "inv_nonexistent")
	if !errors.Is(err, invocation.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGet_CrossTenantBlocked(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending}
	mustCreate(t, s, tenantA, inv)

	_, err := s.Get(context.Background(), tenantB, inv.ID)
	if !errors.Is(err, invocation.ErrNotFound) {
		t.Errorf("cross-tenant get: err = %v, want ErrNotFound", err)
	}
}

// -------------------------------------------------------------------- List ---

func TestList_Empty(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	invs, total, err := s.List(context.Background(), tenantA, agentID, 1, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if len(invs) != 0 {
		t.Errorf("len = %d, want 0", len(invs))
	}
}

func TestList_ByAgent(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv1 := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending}
	inv2 := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusSucceeded}
	invOther := &invocation.Invocation{AgentID: agentB, Mode: invocation.ModeTransactional, Status: invocation.StatusPending}

	mustCreate(t, s, tenantA, inv1)
	mustCreate(t, s, tenantA, inv2)
	mustCreate(t, s, tenantA, invOther)

	invs, total, err := s.List(context.Background(), tenantA, agentID, 1, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(invs) != 2 {
		t.Errorf("len = %d, want 2", len(invs))
	}
	for _, i := range invs {
		if i.AgentID != agentID {
			t.Errorf("unexpected AgentID %q in list results", i.AgentID)
		}
	}
}

func TestList_CrossTenantIsolation(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	mustCreate(t, s, tenantA, &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending})
	mustCreate(t, s, tenantB, &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending})

	invsA, totalA, err := s.List(context.Background(), tenantA, agentID, 1, 50)
	if err != nil {
		t.Fatalf("List tenantA: %v", err)
	}
	if totalA != 1 || len(invsA) != 1 {
		t.Errorf("tenantA: total=%d len=%d, want 1/1", totalA, len(invsA))
	}

	invsB, totalB, err := s.List(context.Background(), tenantB, agentID, 1, 50)
	if err != nil {
		t.Fatalf("List tenantB: %v", err)
	}
	if totalB != 1 || len(invsB) != 1 {
		t.Errorf("tenantB: total=%d len=%d, want 1/1", totalB, len(invsB))
	}
}

func TestList_Pagination(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	for range 5 {
		mustCreate(t, s, tenantA, &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending})
	}

	page1, total, err := s.List(context.Background(), tenantA, agentID, 1, 2)
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(page1) != 2 {
		t.Errorf("page 1 len = %d, want 2", len(page1))
	}

	page3, _, err := s.List(context.Background(), tenantA, agentID, 3, 2)
	if err != nil {
		t.Fatalf("List page 3: %v", err)
	}
	if len(page3) != 1 {
		t.Errorf("page 3 len = %d, want 1", len(page3))
	}

	beyond, _, err := s.List(context.Background(), tenantA, agentID, 10, 2)
	if err != nil {
		t.Fatalf("List page beyond: %v", err)
	}
	if len(beyond) != 0 {
		t.Errorf("page beyond len = %d, want 0", len(beyond))
	}
}

func TestList_MostRecentFirst(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	for range 3 {
		mustCreate(t, s, tenantA, &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending})
	}

	result, _, err := s.List(context.Background(), tenantA, agentID, 1, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("len = %d, want 3", len(result))
	}
	// Verify descending order (most recent first) by checking CreatedAt monotonicity.
	for i := 1; i < len(result); i++ {
		if result[i].CreatedAt.After(result[i-1].CreatedAt) {
			t.Errorf("result[%d].CreatedAt > result[%d].CreatedAt: not sorted most-recent-first", i, i-1)
		}
	}
}

// ------------------------------------------------------------------ Update ---

func TestUpdate_StatusTransition(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending}
	mustCreate(t, s, tenantA, inv)

	running := invocation.StatusRunning
	updated, err := s.Update(context.Background(), tenantA, inv.ID, invocation.UpdateFields{
		Status: &running,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != invocation.StatusRunning {
		t.Errorf("Status = %q, want running", updated.Status)
	}
}

func TestUpdate_CompletionFields(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusRunning}
	mustCreate(t, s, tenantA, inv)

	succeeded := invocation.StatusSucceeded
	completedAt := time.Now().UTC()
	upstreamStatus := 200
	latencyMS := int64(42)
	reqSize := int64(100)
	respSize := int64(200)

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
	if updated.LatencyMS == nil || *updated.LatencyMS != 42 {
		t.Errorf("LatencyMS = %v, want 42", updated.LatencyMS)
	}
	if updated.RequestSize == nil || *updated.RequestSize != 100 {
		t.Errorf("RequestSize = %v, want 100", updated.RequestSize)
	}
	if updated.ResponseSize == nil || *updated.ResponseSize != 200 {
		t.Errorf("ResponseSize = %v, want 200", updated.ResponseSize)
	}
}

func TestUpdate_WithBodies(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	reqBody := []byte(`{"input":"question"}`)
	inv := &invocation.Invocation{
		AgentID:     agentID,
		Mode:        invocation.ModeTransactional,
		Status:      invocation.StatusRunning,
		RequestBody: reqBody,
	}
	mustCreate(t, s, tenantA, inv)

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

func TestUpdate_PartialFields(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending}
	mustCreate(t, s, tenantA, inv)

	running := invocation.StatusRunning
	_, err := s.Update(context.Background(), tenantA, inv.ID, invocation.UpdateFields{Status: &running})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := s.Get(context.Background(), tenantA, inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Only status changed; all other completion fields remain nil.
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil (not set in partial update)", got.CompletedAt)
	}
	if got.UpstreamStatus != nil {
		t.Errorf("UpstreamStatus = %v, want nil", got.UpstreamStatus)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	running := invocation.StatusRunning
	_, err := s.Update(context.Background(), tenantA, "inv_missing", invocation.UpdateFields{Status: &running})
	if !errors.Is(err, invocation.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCreate_NewObservabilityFieldsNilByDefault(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending}
	mustCreate(t, s, tenantA, inv)

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

func TestUpdate_NewObservabilityFields(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeStreaming, Status: invocation.StatusRunning}
	mustCreate(t, s, tenantA, inv)

	failed := invocation.StatusFailed
	ttft := int64(120)
	ec := invocation.ErrorClassTimeout
	model := "gpt-4o"
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
	if updated.TTFTMS == nil || *updated.TTFTMS != 120 {
		t.Errorf("TTFTMS = %v, want 120", updated.TTFTMS)
	}
	if updated.ErrorClass == nil || *updated.ErrorClass != invocation.ErrorClassTimeout {
		t.Errorf("ErrorClass = %v, want %q", updated.ErrorClass, invocation.ErrorClassTimeout)
	}
	if updated.Model == nil || *updated.Model != "gpt-4o" {
		t.Errorf("Model = %v, want %q", updated.Model, "gpt-4o")
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

	// Confirm round-trip from storage, not just the returned struct.
	got, err := s.Get(context.Background(), tenantA, inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TTFTMS == nil || *got.TTFTMS != 120 {
		t.Errorf("stored TTFTMS = %v, want 120", got.TTFTMS)
	}
	if got.ErrorClass == nil || *got.ErrorClass != invocation.ErrorClassTimeout {
		t.Errorf("stored ErrorClass = %v, want %q", got.ErrorClass, invocation.ErrorClassTimeout)
	}
	if got.Model == nil || *got.Model != "gpt-4o" {
		t.Errorf("stored Model = %v, want %q", got.Model, "gpt-4o")
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

func TestUpdate_CrossTenantBlocked(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	inv := &invocation.Invocation{AgentID: agentID, Mode: invocation.ModeTransactional, Status: invocation.StatusPending}
	mustCreate(t, s, tenantA, inv)

	running := invocation.StatusRunning
	_, err := s.Update(context.Background(), tenantB, inv.ID, invocation.UpdateFields{Status: &running})
	if !errors.Is(err, invocation.ErrNotFound) {
		t.Errorf("cross-tenant update: err = %v, want ErrNotFound", err)
	}
}

// ------------------------------------------------------- Return isolation ---

func TestReturnedValuesAreIsolated(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	body := []byte("original")
	inv := &invocation.Invocation{
		AgentID:     agentID,
		Mode:        invocation.ModeTransactional,
		Status:      invocation.StatusPending,
		RequestBody: body,
	}
	mustCreate(t, s, tenantA, inv)

	got, err := s.Get(context.Background(), tenantA, inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Mutate the returned slice.
	got.RequestBody[0] = 'X'

	got2, err := s.Get(context.Background(), tenantA, inv.ID)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if !bytes.Equal(got2.RequestBody, []byte("original")) {
		t.Errorf("stored body = %q; caller mutation leaked into store", got2.RequestBody)
	}
}
