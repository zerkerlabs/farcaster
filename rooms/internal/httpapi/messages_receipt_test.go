package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/gateway"
	"github.com/zerkerlabs/farcaster/rooms/internal/receipt"
)

// recordingEmitter is a receipt.Emitter test double that records every
// receipt it is handed and can simulate a permanently failing or a hanging
// backend.
type recordingEmitter struct {
	mu       sync.Mutex
	receipts []receipt.Receipt
	// err is returned from every Emit call, simulating a backend that is up
	// but always errors.
	err error
	// emitted signals once per Emit call, so a test can wait for emission
	// deterministically instead of sleeping.
	emitted chan struct{}
	// block, when non-nil, is read from before Emit records anything and
	// returns — a stand-in for a backend that never answers. Only ctx
	// cancellation (e.g. from Handler.Shutdown) unblocks it.
	block chan struct{}
}

func (e *recordingEmitter) Emit(ctx context.Context, r receipt.Receipt) error {
	if e.block != nil {
		select {
		case <-e.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	e.mu.Lock()
	e.receipts = append(e.receipts, r)
	e.mu.Unlock()

	if e.emitted != nil {
		select {
		case e.emitted <- struct{}{}:
		default:
		}
	}
	return e.err
}

func (e *recordingEmitter) Receipts() []receipt.Receipt {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]receipt.Receipt(nil), e.receipts...)
}

// waitForEmit fails the test if em does not record an Emit call within 2s —
// generous enough to absorb scheduler noise without masking a bug that never
// emits at all.
func waitForEmit(t *testing.T, em *recordingEmitter) {
	t.Helper()
	select {
	case <-em.emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("no receipt emitted within 2s")
	}
}

// An addressed message is the only kind of room operation that calls another
// member's agent through the gateway, so it is the only kind that must leave
// a receipt behind — one per call, carrying who called whom, in which room,
// and how it turned out.
func TestHandlePostMessage_EmitsReceiptOnSuccessfulDelivery(t *testing.T) {
	t.Parallel()

	gw := newCountingGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"inv_test"}`))
	})
	em := &recordingEmitter{emitted: make(chan struct{}, 1)}

	mux, store, _ := newMuxWithReceipts(t, nil, gw, em)
	roomID, memberID := roomAndMember(t, store, 0)
	recipient := mustAddMember(t, store, roomID, "agt_recipient")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello"}, tenantA))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	waitForEmit(t, em)

	got := em.Receipts()
	if len(got) != 1 {
		t.Fatalf("len(Receipts()) = %d, want exactly 1", len(got))
	}
	r := got[0]
	if r.RoomID != roomID {
		t.Errorf("RoomID = %q, want %q", r.RoomID, roomID)
	}
	if r.TenantID != tenantA {
		t.Errorf("TenantID = %q, want %q", r.TenantID, tenantA)
	}
	if r.FromMemberID != memberID {
		t.Errorf("FromMemberID = %q, want %q", r.FromMemberID, memberID)
	}
	if r.ToMemberID != recipient.ID {
		t.Errorf("ToMemberID = %q, want %q", r.ToMemberID, recipient.ID)
	}
	if r.ToAgentID != "agt_recipient" {
		t.Errorf("ToAgentID = %q, want %q", r.ToAgentID, "agt_recipient")
	}
	if r.InvocationID != "inv_test" {
		t.Errorf("InvocationID = %q, want %q", r.InvocationID, "inv_test")
	}
	if r.Outcome != receipt.OutcomeSucceeded {
		t.Errorf("Outcome = %q, want %q", r.Outcome, receipt.OutcomeSucceeded)
	}
	if r.FailureClass != "" {
		t.Errorf("FailureClass = %q, want empty for a succeeded receipt", r.FailureClass)
	}
}

// A rejected or otherwise failed call is exactly as auditable as a delivered
// one — the outcome, not just the success, has to be receipted.
func TestHandlePostMessage_EmitsReceiptOnFailedDelivery(t *testing.T) {
	t.Parallel()

	gw := newCountingGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	em := &recordingEmitter{emitted: make(chan struct{}, 1)}

	mux, store, _ := newMuxWithReceipts(t, nil, gw, em)
	roomID, memberID := roomAndMember(t, store, 0)
	recipient := mustAddMember(t, store, roomID, "agt_recipient")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello"}, tenantA))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}

	waitForEmit(t, em)

	got := em.Receipts()
	if len(got) != 1 {
		t.Fatalf("len(Receipts()) = %d, want exactly 1", len(got))
	}
	r := got[0]
	if r.Outcome != receipt.OutcomeFailed {
		t.Errorf("Outcome = %q, want %q", r.Outcome, receipt.OutcomeFailed)
	}
	if r.FailureClass != string(gateway.ErrorClassCallerError) {
		t.Errorf("FailureClass = %q, want %q", r.FailureClass, gateway.ErrorClassCallerError)
	}
	if r.InvocationID != "" {
		t.Errorf("InvocationID = %q, want empty — the gateway never accepted this call", r.InvocationID)
	}
}

// A broadcast message never calls another member's agent, so it is out of
// scope for receipts: only cross-agent interactions get one.
func TestHandlePostMessage_NoReceiptForBroadcastMessage(t *testing.T) {
	t.Parallel()

	em := &recordingEmitter{}
	mux, store, _ := newMuxWithReceipts(t, nil, unreachableGateway{t}, em)
	roomID, memberID := roomAndMember(t, store, 0)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "body": "hello everyone"}, tenantA))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if got := em.Receipts(); len(got) != 0 {
		t.Errorf("Receipts() = %+v, want empty — a broadcast message never calls the gateway", got)
	}
}

// Emission is asynchronous: a hung receipt backend must not add its latency
// to the request the receipt describes.
func TestHandlePostMessage_ReceiptEmissionIsAsyncAndDoesNotBlockRequest(t *testing.T) {
	t.Parallel()

	gw := newCountingGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"inv_test"}`))
	})
	// block is never closed in this test: the emitter hangs until Shutdown
	// cancels it below, which is exactly the case the request path must not
	// wait on.
	em := &recordingEmitter{block: make(chan struct{})}

	mux, store, h := newMuxWithReceipts(t, nil, gw, em)
	roomID, memberID := roomAndMember(t, store, 0)
	recipient := mustAddMember(t, store, roomID, "agt_recipient")

	start := time.Now()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello"}, tenantA))
	elapsed := time.Since(start)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if elapsed > time.Second {
		t.Errorf("request took %s while its receipt emission was hung — the request path blocked on the emitter", elapsed)
	}

	// Unblocks the still-running emission goroutine via Shutdown's
	// cancellation, so it drains instead of leaking past the test.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// Emission is fail-open: a permanently failing receipt backend must not fail
// the room message it describes.
func TestHandlePostMessage_ReceiptEmitterFailureIsFailOpen(t *testing.T) {
	t.Parallel()

	gw := newCountingGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"inv_test"}`))
	})
	em := &recordingEmitter{err: errors.New("receipt backend down"), emitted: make(chan struct{}, 1)}

	mux, store, _ := newMuxWithReceipts(t, nil, gw, em)
	roomID, memberID := roomAndMember(t, store, 0)
	recipient := mustAddMember(t, store, roomID, "agt_recipient")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello"}, tenantA))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d despite a permanently failing emitter; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	// The emitter was still invoked (fail-open ≠ skipped).
	waitForEmit(t, em)

	msgs, err := store.Messages(context.Background(), tenantA, roomID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("transcript = %+v, want exactly the one delivered message", msgs)
	}
}

// Handler.Shutdown must cancel an in-flight receipt emission rather than
// waiting out its own timeout (receiptEmitTimeout, 10s) — an emitter
// goroutine must not outlive the service.
func TestHandler_ShutdownDrainsReceiptGoroutines(t *testing.T) {
	t.Parallel()

	gw := newCountingGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"inv_test"}`))
	})
	em := &recordingEmitter{block: make(chan struct{})}

	mux, store, h := newMuxWithReceipts(t, nil, gw, em)
	roomID, memberID := roomAndMember(t, store, 0)
	recipient := mustAddMember(t, store, roomID, "agt_recipient")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello"}, tenantA))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	start := time.Now()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Shutdown took %s, want well under the 10s receipt emit timeout — the emission goroutine was not cancelled promptly", elapsed)
	}
}
