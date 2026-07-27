// Package httpapi implements the Rooms v1 HTTP API: the four routes that
// create a room, read it back, seat a member, and post a message. Each
// handler lives in its own file, following the shape established in the
// gateway module's httpapi package.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/zerkerlabs/farcaster/rooms/internal/room"
)

// Handler holds the shared dependencies for the Rooms HTTP handlers.
type Handler struct {
	store  *room.Store
	logger *slog.Logger
}

// NewHandler returns a Handler backed by store, logging to logger.
func NewHandler(store *room.Store, logger *slog.Logger) *Handler {
	return &Handler{store: store, logger: logger}
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
