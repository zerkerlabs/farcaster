package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/gateway"
)

// countingGateway wraps an httptest server standing in for the Farcaster
// gateway and records every request path it receives, so tests can assert
// exactly one proxied call was made and that no other outbound request
// happened.
type countingGateway struct {
	srv     *httptest.Server
	client  *gateway.Client
	reqPath []string

	// invocationStatus / upstreamStatus are what the invocation poll reports —
	// the real outcome of the call, which the POST's 202 does not carry.
	invocationStatus string
	upstreamStatus   int
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
	g := &countingGateway{invocationStatus: "succeeded", upstreamStatus: 200}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"` + g.invocationStatus + `","upstream_status":` + strconv.Itoa(g.upstreamStatus) + `}`))
			return
		}
		g.reqPath = append(g.reqPath, r.URL.Path)
		respond(w, r)
	}))
	t.Cleanup(g.srv.Close)

	client, err := gateway.New(gateway.Config{ //nolint:gosec // G101: test fixture, not a real credential
		BaseURL:        g.srv.URL,
		Credential:     testGatewayCredential,
		ConfirmTimeout: 2 * time.Second,
		PollInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	g.client = client
	return g
}

func (g *countingGateway) Call(ctx context.Context, agentID string, body []byte) (*gateway.Result, error) {
	return g.client.Call(ctx, agentID, body)
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
		if len(gw.reqPath) != 1 || gw.reqPath[0] != "/v1/proxy/agt_recipient" {
			t.Fatalf("gateway paths = %v, want exactly [%q]", gw.reqPath, "/v1/proxy/agt_recipient")
		}

		body := decodeBody(t, rec)
		if body["body"] != "hello there" {
			t.Errorf("body = %v, want %q", body["body"], "hello there")
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
		if len(gw.reqPath) != 1 {
			t.Fatalf("gateway request count = %d, want exactly 1", len(gw.reqPath))
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
