package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
}

func newCountingGateway(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *countingGateway {
	t.Helper()
	g := &countingGateway{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.reqPath = append(g.reqPath, r.URL.Path)
		respond(w, r)
	}))
	t.Cleanup(g.srv.Close)

	client, err := gateway.New(gateway.Config{BaseURL: g.srv.URL, Credential: "test-credential"}) //nolint:gosec // test fixture, not a real credential
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	g.client = client
	return g
}

func (g *countingGateway) Call(ctx context.Context, agentID string, body []byte) error {
	return g.client.Call(ctx, agentID, body)
}

func TestHandlePostMessage_Addressed(t *testing.T) {
	t.Parallel()

	t.Run("201 delivers exactly one proxied call to the recipient's agent", func(t *testing.T) {
		t.Parallel()

		var requestCount int32
		gw := newCountingGateway(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			w.WriteHeader(http.StatusAccepted)
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
