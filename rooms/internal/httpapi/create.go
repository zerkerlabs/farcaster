package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/zerkerlabs/gateway/rooms/internal/auth"
	"github.com/zerkerlabs/gateway/rooms/internal/room"
)

// createRoomRequest is the request body for POST /v1/rooms.
type createRoomRequest struct {
	Goal string `json:"goal"`
	// TurnBudget is optional; a nil value leaves the room's default turn
	// budget in place (room.DefaultTurnBudget).
	TurnBudget *int `json:"turn_budget"`
}

func (h *Handler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantFromContext(r.Context())
	if tenantID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req createRoomRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Goal == "" {
		writeError(w, http.StatusBadRequest, "goal is required")
		return
	}

	if req.TurnBudget != nil && *req.TurnBudget <= 0 {
		writeError(w, http.StatusBadRequest, "turn_budget must be positive")
		return
	}

	var (
		created *room.Room
		err     error
	)
	if req.TurnBudget != nil {
		created, err = h.store.CreateRoomWithBudget(r.Context(), tenantID, req.Goal, *req.TurnBudget)
	} else {
		created, err = h.store.CreateRoom(r.Context(), tenantID, req.Goal)
	}
	if err != nil {
		h.logger.Error("create room: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, toRoomResponse(created, nil))
}
