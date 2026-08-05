package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerkerlabs/gateway/rooms/internal/gateway"
	"github.com/zerkerlabs/gateway/rooms/internal/room"
)

// countingGateway wraps an httptest server standing in for the Zerker
// gateway and records every request path it receives, so tests can assert
// exactly one proxied call was made and that no other outbound request
// happened.
type countingGateway struct {
	srv    *httptest.Server
	client *gateway.Client

	// invocationStatus / upstreamStatus are what the invocation poll reports —
	// the real outcome of the call, which the POST's 202 does not carry. Set
	// them before serving any request.
	invocationStatus string
	upstreamStatus   int

	mu      sync.Mutex
	reqPath []string
}

// paths returns the proxy paths POSTed so far. Guarded, because a test that
// posts to a room from two goroutines at once reaches the stand-in gateway
// concurrently.
func (g *countingGateway) paths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.reqPath...)
}

const testGatewayCredential = "test-credential" //nolint:gosec // test fixture, not a real credential

// newCountingGateway builds a stand-in gateway. respond handles the POST; the
// invocation poll is served automatically from invocationStatus/upstreamStatus,
// because the proxy is asynchronous and a fake that only answers the POST
// cannot express the outcome that actually matters.
//
// reqPath records only POSTs, so "exactly one proxied call" assertions are not
// confused by the polls that confirm it.
func newCountingGateway(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *countingGateway {
	t.Helper()
	return newCountingGatewayWithConfirmTimeout(t, 2*time.Second, respond)
}

// newCountingGatewayWithConfirmTimeout is newCountingGateway with an explicit
// confirmation budget, for tests about what happens when an accepted call never
// reaches a terminal state.
func newCountingGatewayWithConfirmTimeout(t *testing.T, confirmTimeout time.Duration, respond func(w http.ResponseWriter, r *http.Request)) *countingGateway {
	t.Helper()
	g := &countingGateway{invocationStatus: "succeeded", upstreamStatus: 200}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"` + g.invocationStatus + `","upstream_status":` + strconv.Itoa(g.upstreamStatus) + `}`))
			return
		}
		g.mu.Lock()
		g.reqPath = append(g.reqPath, r.URL.Path)
		g.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(g.srv.Close)

	client, err := gateway.New(gateway.Config{ //nolint:gosec // G101: test fixture, not a real credential
		BaseURL:    g.srv.URL,
		Credential: testGatewayCredential,
		// The client's credential acts as tenantA, the tenant every handler
		// test's room belongs to.
		Tenant:         tenantA,
		ConfirmTimeout: confirmTimeout,
		PollInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	g.client = client
	return g
}

func (g *countingGateway) Call(ctx context.Context, tenantID, agentID string, body []byte) (*gateway.Result, error) {
	return g.client.Call(ctx, tenantID, agentID, body)
}

func TestHandlePostMessage_Addressed(t *testing.T) {
	t.Parallel()

	t.Run("201 delivers exactly one proxied call to the recipient's agent", func(t *testing.T) {
		t.Parallel()

		var requestCount int32
		gw := newCountingGateway(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"invocation_id":"inv_test"}`))
		})

		mux, store := newMuxWithMemoryAndGateway(t, nil, gw)
		roomID, memberID := roomAndMember(t, store, 0)
		recipient := mustAddMember(t, store, roomID, "agt_recipient")

		req := requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
			map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello there"}, tenantA)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}
		if got := atomic.LoadInt32(&requestCount); got != 1 {
			t.Fatalf("gateway request count = %d, want exactly 1", got)
		}
		if paths := gw.paths(); len(paths) != 1 || paths[0] != "/v1/proxy/agt_recipient" {
			t.Fatalf("gateway paths = %v, want exactly [%q]", paths, "/v1/proxy/agt_recipient")
		}

		body := decodeBody(t, rec)
		if body["body"] != "hello there" {
			t.Errorf("body = %v, want %q", body["body"], "hello there")
		}
		// The recipient is carried on the message itself, so a transcript
		// reader can tell an addressed message from a broadcast one without
		// walking the event log.
		if body["to_member_id"] != recipient.ID {
			t.Errorf("to_member_id = %v, want %q", body["to_member_id"], recipient.ID)
		}

		get := requestAs(t, http.MethodGet, "/v1/rooms/"+roomID, nil, tenantA)
		getRec := httptest.NewRecorder()
		mux.ServeHTTP(getRec, get)
		transcript, _ := decodeBody(t, getRec)["transcript"].([]any)
		if len(transcript) != 1 {
			t.Fatalf("transcript = %v, want exactly one message", transcript)
		}
		if got, _ := transcript[0].(map[string]any); got["to_member_id"] != recipient.ID {
			t.Errorf("transcript to_member_id = %v, want %q", got["to_member_id"], recipient.ID)
		}
	})

	t.Run("422 when the gateway rejects the call; message is not recorded", func(t *testing.T) {
		t.Parallel()

		gw := newCountingGateway(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("agent not found: internal detail"))
		})

		mux, store := newMuxWithMemoryAndGateway(t, nil, gw)
		roomID, memberID := roomAndMember(t, store, 0)
		recipient := mustAddMember(t, store, roomID, "agt_recipient")

		req := requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
			map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello there"}, tenantA)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
		}
		if got := len(gw.paths()); got != 1 {
			t.Fatalf("gateway request count = %d, want exactly 1", got)
		}
		if strings.Contains(rec.Body.String(), "internal detail") {
			t.Errorf("response body = %q, must not leak the raw upstream body", rec.Body.String())
		}

		get := requestAs(t, http.MethodGet, "/v1/rooms/"+roomID, nil, tenantA)
		getRec := httptest.NewRecorder()
		mux.ServeHTTP(getRec, get)
		got := decodeBody(t, getRec)
		transcript, _ := got["transcript"].([]any)
		if len(transcript) != 0 {
			t.Errorf("transcript = %v, want empty after a rejected delivery", transcript)
		}
	})

	t.Run("502 when the gateway fails upstream; message is not recorded", func(t *testing.T) {
		t.Parallel()

		gw := newCountingGateway(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})

		mux, store := newMuxWithMemoryAndGateway(t, nil, gw)
		roomID, memberID := roomAndMember(t, store, 0)
		recipient := mustAddMember(t, store, roomID, "agt_recipient")

		req := requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
			map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello there"}, tenantA)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadGateway, rec.Body.String())
		}

		get := requestAs(t, http.MethodGet, "/v1/rooms/"+roomID, nil, tenantA)
		getRec := httptest.NewRecorder()
		mux.ServeHTTP(getRec, get)
		got := decodeBody(t, getRec)
		transcript, _ := got["transcript"].([]any)
		if len(transcript) != 0 {
			t.Errorf("transcript = %v, want empty after a failed delivery", transcript)
		}
	})

	t.Run("400 for an unknown to_member_id; no gateway call is made", func(t *testing.T) {
		t.Parallel()

		mux, store := newMuxWithMemoryAndGateway(t, nil, unreachableGateway{t})
		roomID, memberID := roomAndMember(t, store, 0)

		req := requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
			map[string]any{"member_id": memberID, "to_member_id": "mem_nope", "body": "hello"}, tenantA)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})
}

// A proxied call is a REAL side effect on another member's agent, with policy
// and payment consequences. It must not fire for a request the room would
// reject anyway — otherwise a terminated room, an exhausted turn budget, or a
// spoofed sender can still trigger a live call that the room has no record of.
func TestHandlePostMessage_NoGatewayCallForAnInvalidRequest(t *testing.T) {
	t.Parallel()

	t.Run("terminated room", func(t *testing.T) {
		t.Parallel()

		mux, store := newMuxWithMemoryAndGateway(t, nil, unreachableGateway{t})
		roomID, memberID := roomAndMember(t, store, 0)
		recipient := mustAddMember(t, store, roomID, "agt_recipient")
		if _, err := store.CompleteRoom(context.Background(), tenantA, roomID); err != nil {
			t.Fatalf("CompleteRoom: %v", err)
		}

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
			map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello"}, tenantA))

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusConflict, rec.Body.String())
		}
	})

	t.Run("turn budget exhausted", func(t *testing.T) {
		t.Parallel()

		mux, store := newMuxWithMemoryAndGateway(t, nil, unreachableGateway{t})
		roomID, memberID := roomAndMember(t, store, 1)
		recipient := mustAddMember(t, store, roomID, "agt_recipient")

		// Consume the room's single turn with an unaddressed message, which
		// needs no gateway call.
		first := httptest.NewRecorder()
		mux.ServeHTTP(first, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
			map[string]any{"member_id": memberID, "body": "first"}, tenantA))
		if first.Code != http.StatusCreated {
			t.Fatalf("first message status = %d, want %d", first.Code, http.StatusCreated)
		}

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
			map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "over budget"}, tenantA))

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusConflict, rec.Body.String())
		}
	})

	t.Run("sender is not a member of the room", func(t *testing.T) {
		t.Parallel()

		mux, store := newMuxWithMemoryAndGateway(t, nil, unreachableGateway{t})
		roomID, _ := roomAndMember(t, store, 0)
		recipient := mustAddMember(t, store, roomID, "agt_recipient")

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
			map[string]any{"member_id": "mem_spoofed", "to_member_id": recipient.ID, "body": "hello"}, tenantA))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("room belongs to another tenant", func(t *testing.T) {
		t.Parallel()

		mux, store := newMuxWithMemoryAndGateway(t, nil, unreachableGateway{t})
		roomID, memberID := roomAndMember(t, store, 0)
		recipient := mustAddMember(t, store, roomID, "agt_recipient")

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
			map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello"}, tenantB))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
		}
	})
}

// Confirming a proxied call takes as long as the recipient's agent takes, and
// the room stays open to other requests throughout. A concurrent post must not
// be able to spend the turn the in-flight delivery is going to need: the
// recipient really receives that call, so if the message it carried could then
// be refused, the room would end up denying a call it actually made.
//
// The reserved turn is what prevents it — the concurrent post is refused
// instead, and the delivered message lands.
func TestHandlePostMessage_ConcurrentPostCannotStealAnInFlightTurn(t *testing.T) {
	t.Parallel()

	delivering := make(chan struct{})
	release := make(chan struct{})
	gw := newCountingGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		close(delivering)
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"inv_test"}`))
	})

	// One turn, so the addressed message and the concurrent post are competing
	// for the same last turn.
	mux, store := newMuxWithMemoryAndGateway(t, nil, gw)
	roomID, memberID := roomAndMember(t, store, 1)
	recipient := mustAddMember(t, store, roomID, "agt_recipient")

	addressed := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		mux.ServeHTTP(addressed, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
			map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "delivered"}, tenantA))
	}()

	select {
	case <-delivering: // the call is out; its turn is reserved but not yet spent
	case <-time.After(2 * time.Second):
		t.Fatal("addressed request never reached the gateway stub within 2s")
	}

	concurrent := httptest.NewRecorder()
	mux.ServeHTTP(concurrent, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "body": "sneaking in"}, tenantA))

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("addressed request did not complete within 2s of releasing the gateway stub")
	}

	if concurrent.Code != http.StatusConflict {
		t.Errorf("concurrent post status = %d, want %d — it took the turn an in-flight delivery was holding; body = %s",
			concurrent.Code, http.StatusConflict, concurrent.Body.String())
	}
	if addressed.Code != http.StatusCreated {
		t.Fatalf("addressed post status = %d, want %d; body = %s",
			addressed.Code, http.StatusCreated, addressed.Body.String())
	}

	// The confirmed call is in the transcript, and it is the only thing there.
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, requestAs(t, http.MethodGet, "/v1/rooms/"+roomID, nil, tenantA))
	got := decodeBody(t, getRec)
	transcript, _ := got["transcript"].([]any)
	if len(transcript) != 1 {
		t.Fatalf("transcript = %v, want exactly the one delivered message", transcript)
	}
	if msg, _ := transcript[0].(map[string]any); msg["body"] != "delivered" {
		t.Errorf("transcript message = %v, want the delivered one", msg)
	}

	// The refused post must not have abandoned the room either: its turn was
	// reserved, not spent, and a room is only abandoned by turns it really
	// used. Here the reservation went on to commit, so the budget is spent and
	// the room is open with nothing left — not terminated by the near-miss.
	if got["state"] != string(room.StateOpen) {
		t.Errorf("state = %v, want %q — a post refused by a reserved turn abandoned the room", got["state"], room.StateOpen)
	}
}

// Every agent-to-agent call goes through the gateway — so a message addressed
// to another member must never be recorded WITHOUT one. The tempting bug is a
// refused addressed message quietly retrying as a plain broadcast: a turn
// refused because another delivery holds it is transient, so the retry can
// succeed, and the result is a 201 for a message that never left the room.
//
// Many senders contend for a single turn here, and every delivery fails, so the
// only correct outcome is that nothing at all is recorded. Anything in the
// transcript is a message that bypassed the gateway.
func TestHandlePostMessage_AddressedMessageNeverDegradesToABroadcast(t *testing.T) {
	t.Parallel()

	gw := newCountingGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"inv_test"}`))
	})
	// Accepted, then failed: every reservation is released unspent, which is
	// what frees the turn for the next contender to reserve — and, without the
	// fix, for a refused one to retry as a broadcast.
	gw.invocationStatus = "failed"
	gw.upstreamStatus = 503

	mux, store := newMuxWithMemoryAndGateway(t, nil, gw)
	roomID, memberID := roomAndMember(t, store, 1)
	recipient := mustAddMember(t, store, roomID, "agt_recipient")

	// The window is the moment between a sender being refused the turn and its
	// write: a delivery releasing its reservation right there is what used to
	// let the write through. Sustained contention reaches it in a few hundred
	// attempts — this budget is roughly ten times that, and costs ~100ms
	// because a refused attempt never reaches the gateway.
	const (
		senders           = 12
		attemptsPerSender = 500
	)
	var created atomic.Int32
	var wg sync.WaitGroup
	for range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range attemptsPerSender {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
					map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "addressed"}, tenantA))
				if rec.Code == http.StatusCreated {
					created.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := created.Load(); got != 0 {
		t.Errorf("%d posts returned 201 though every delivery failed", got)
	}

	msgs, err := store.Messages(context.Background(), tenantA, roomID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	for _, m := range msgs {
		t.Errorf("recorded message %+v (to_member_id %q) — every delivery failed, so this message reached the transcript without a proxied call",
			m, m.ToMemberID)
	}
}

// Rooms is multi-tenant; the gateway credential is not. A room whose tenant is
// not the one this deployment's credential acts as must be refused locally —
// sending the call anyway would have the gateway authorise, policy-check, and
// bill it against the wrong tenant.
func TestHandlePostMessage_RefusesRoomFromAnotherGatewayTenant(t *testing.T) {
	t.Parallel()

	gw := newCountingGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"inv_test"}`))
	})

	// The stand-in client acts as tenantA; this room belongs to tenantB.
	mux, store := newMuxWithMemoryAndGateway(t, nil, gw)
	ctx := context.Background()
	r, err := store.CreateRoom(ctx, tenantB, "goal")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	sender, err := store.AddMember(ctx, tenantB, r.ID, "agt_sender", tenantB, nil)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	recipient, err := store.AddMember(ctx, tenantB, r.ID, "agt_recipient", tenantB, nil)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+r.ID+"/messages",
		map[string]any{"member_id": sender.ID, "to_member_id": recipient.ID, "body": "hello"}, tenantB))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if paths := gw.paths(); len(paths) != 0 {
		t.Errorf("gateway paths = %v, want none — nothing may be sent for a tenant the credential cannot act for", paths)
	}

	msgs, err := store.Messages(ctx, tenantB, r.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("transcript = %v, want empty", msgs)
	}

	// The room still records why, so the refusal is auditable rather than
	// silent.
	events, err := store.Events(ctx, tenantB, r.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var failed *room.DeliveryFailedPayload
	for _, ev := range events {
		if p, ok := ev.Payload.(room.DeliveryFailedPayload); ok {
			failed = &p
		}
	}
	if failed == nil {
		t.Fatal("no delivery_failed event recorded")
	}
	if failed.Class != string(gateway.ErrorClassTenantMismatch) {
		t.Errorf("class = %q, want %q", failed.Class, gateway.ErrorClassTenantMismatch)
	}
}

// Delivery is confirmed synchronously, so the handler is bounded by the
// client's confirmation budget: an invocation that never terminates must end
// the request with a 504 rather than hanging on it. This is the tradeoff of
// confirming inline — see gateway.Config.ConfirmTimeout for the total bound.
func TestHandlePostMessage_UnconfirmedDeliveryEndsTheRequest(t *testing.T) {
	t.Parallel()

	const confirmTimeout = 100 * time.Millisecond
	gw := newCountingGatewayWithConfirmTimeout(t, confirmTimeout, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"inv_test"}`))
	})
	gw.invocationStatus = "running" // never terminal

	mux, store := newMuxWithMemoryAndGateway(t, nil, gw)
	roomID, memberID := roomAndMember(t, store, 0)
	recipient := mustAddMember(t, store, roomID, "agt_recipient")

	start := time.Now()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello"}, tenantA))
	elapsed := time.Since(start)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusGatewayTimeout, rec.Body.String())
	}
	if elapsed > 10*confirmTimeout {
		t.Errorf("request took %s, want it bounded by the ~%s confirmation budget", elapsed, confirmTimeout)
	}

	// Unknown is not delivered: nothing is recorded, and the turn it reserved
	// goes back to the room rather than being lost.
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, requestAs(t, http.MethodGet, "/v1/rooms/"+roomID, nil, tenantA))
	if transcript, _ := decodeBody(t, getRec)["transcript"].([]any); len(transcript) != 0 {
		t.Errorf("transcript = %v, want empty — an unconfirmed call was recorded as delivered", transcript)
	}
	if _, err := store.AppendMessage(context.Background(), tenantA, roomID, memberID, "next"); err != nil {
		t.Errorf("AppendMessage after an unconfirmed delivery: %v — the reserved turn was never released", err)
	}
}

// A client that hangs up mid-delivery does not un-call the recipient's agent.
// The call is already out, so Rooms sees it through and records the outcome:
// abandoning the confirmation would report a delivery that succeeded as
// unconfirmed, and abandoning the write would leave a call the room denies.
func TestHandlePostMessage_CallerDisconnectDoesNotAbandonADelivery(t *testing.T) {
	t.Parallel()

	var cancelCaller context.CancelFunc
	gw := newCountingGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		// Accepted — the recipient's agent is now running the call. The caller
		// gives up at exactly this point.
		cancelCaller()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"inv_test"}`))
	})

	mux, store := newMuxWithMemoryAndGateway(t, nil, gw)
	roomID, memberID := roomAndMember(t, store, 0)
	recipient := mustAddMember(t, store, roomID, "agt_recipient")

	req := requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "delivered anyway"}, tenantA)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	cancelCaller = cancel
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	msgs, err := store.Messages(context.Background(), tenantA, roomID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "delivered anyway" {
		t.Fatalf("transcript = %+v, want the delivered message — a hangup lost a confirmed delivery", msgs)
	}

	events, err := store.Events(context.Background(), tenantA, roomID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var delivered bool
	for _, ev := range events {
		if ev.Kind == room.EventMessageDelivered {
			delivered = true
		}
	}
	if !delivered {
		t.Error("no message_delivered event — the delivery outcome went unrecorded when the caller hung up")
	}
}

// An accepted-but-failed invocation must not be recorded as a delivered
// message: the gateway's 202 only means the call was queued.
func TestHandlePostMessage_AcceptedThenFailedIsNotRecorded(t *testing.T) {
	t.Parallel()

	gw := newCountingGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"inv_test"}`))
	})
	gw.invocationStatus = "failed"
	gw.upstreamStatus = 503

	mux, store := newMuxWithMemoryAndGateway(t, nil, gw)
	roomID, memberID := roomAndMember(t, store, 0)
	recipient := mustAddMember(t, store, roomID, "agt_recipient")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestAs(t, http.MethodPost, "/v1/rooms/"+roomID+"/messages",
		map[string]any{"member_id": memberID, "to_member_id": recipient.ID, "body": "hello"}, tenantA))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}

	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, requestAs(t, http.MethodGet, "/v1/rooms/"+roomID, nil, tenantA))
	transcript, _ := decodeBody(t, getRec)["transcript"].([]any)
	if len(transcript) != 0 {
		t.Errorf("transcript = %v, want empty — a failed invocation was recorded as delivered", transcript)
	}
}
