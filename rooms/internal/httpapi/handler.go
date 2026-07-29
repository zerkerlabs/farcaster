// Package httpapi implements the Rooms v1 HTTP API: the four routes that
// create a room, read it back, seat a member, and post a message. Each
// handler lives in its own file, following the shape established in the
// gateway module's httpapi package.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/zerkerlabs/farcaster/rooms/internal/gateway"
	"github.com/zerkerlabs/farcaster/rooms/internal/memory"
	"github.com/zerkerlabs/farcaster/rooms/internal/room"
)

// GatewayCaller is the subset of *gateway.Client the message handlers need:
// delivering one proxied call to a member's agent through the Farcaster
// gateway and confirming it actually completed. *gateway.Client satisfies this
// interface.
//
// Call returns only once delivery is confirmed — the gateway's proxy is
// asynchronous, so an accepted call is not yet a delivered one. It takes the
// tenant the call is made on behalf of: the gateway attributes a proxied call
// to whichever tenant the credential belongs to, so the tenant has to be
// carried explicitly rather than assumed.
type GatewayCaller interface {
	Call(ctx context.Context, tenantID, agentID string, body []byte) (*gateway.Result, error)
}

// Handler holds the shared dependencies for the Rooms HTTP handlers.
type Handler struct {
	store       *room.Store
	memoryStore memory.Store
	gateway     GatewayCaller
	logger      *slog.Logger
}

// NewHandler returns a Handler backed by store, logging to logger. memoryStore
// is the seam onboarding a member reads from (rooms/internal/memory).
// gatewayClient delivers a message addressed to another member as a proxied
// call to that member's agent (rooms/internal/gateway) — every agent-to-agent
// call goes through it, never direct.
func NewHandler(store *room.Store, memoryStore memory.Store, gatewayClient GatewayCaller, logger *slog.Logger) *Handler {
	return &Handler{store: store, memoryStore: memoryStore, gateway: gatewayClient, logger: logger}
}

// RegisterRoutes mounts the four v1 room routes onto mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/rooms", h.handleCreateRoom)
	mux.HandleFunc("GET /v1/rooms/{rom_id}", h.handleGetRoom)
	mux.HandleFunc("POST /v1/rooms/{rom_id}/members", h.handleAddMember)
	mux.HandleFunc("POST /v1/rooms/{rom_id}/messages", h.handlePostMessage)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
