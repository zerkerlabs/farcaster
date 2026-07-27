package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/farcaster/rooms/internal/httpapi"
	"github.com/zerkerlabs/farcaster/rooms/internal/room"
	"github.com/zerkerlabs/farcaster/rooms/internal/tenant"
)

const (
	tenantA = "tenant-alpha"
	tenantB = "tenant-beta"
)

// newMux returns a mux serving the four v1 room routes, backed by a fresh
// in-memory store.
func newMux(t *testing.T) (*http.ServeMux, *room.Store) {
	t.Helper()
	store := room.NewStore()
	h := httpapi.NewHandler(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux, store
}

// requestAs returns a request to path with body, carrying tenantID on its
// context as the tenant seam would once auth lands. A nil body sends no
// request body; a []byte body is sent raw (for malformed-JSON cases); any
// other value is JSON-marshaled.
func requestAs(t *testing.T, method, path string, body any, tenantID string) *http.Request {
	t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
		rdr = nil
	case []byte:
		rdr = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if tenantID != "" {
		req = req.WithContext(tenant.WithTenant(req.Context(), tenantID))
	}
	return req
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body: %v; raw = %s", err, rec.Body.String())
	}
	return body
}

// mustCreateRoom creates a room under tenantA, the tenant every handler test
// treats as the room's owner; cross-tenant cases look the room up as tenantB.
func mustCreateRoom(t *testing.T, s *room.Store, goal string) *room.Room {
	t.Helper()
	r, err := s.CreateRoom(context.Background(), tenantA, goal)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	return r
}

func mustAddMember(t *testing.T, s *room.Store, roomID, agentID string) *room.Member {
	t.Helper()
	m, err := s.AddMember(context.Background(), tenantA, roomID, agentID, tenantA)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	return m
}
