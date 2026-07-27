package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/zerkerlabs/farcaster/rooms/internal/room"
	"github.com/zerkerlabs/farcaster/rooms/internal/tenant"
)

// postMessageRequest is the request body for POST /v1/rooms/{rom_id}/messages.
type postMessageRequest struct {
	MemberID string `json:"member_id"`
	Body     string `json:"body"`
}

func (h *Handler) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	tenantID := tenant.FromContext(r.Context())
	if tenantID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	roomID := r.PathValue("rom_id")

	var req postMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.MemberID == "" {
		writeError(w, http.StatusBadRequest, "member_id is required")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}

	msg, err := h.store.AppendMessage(r.Context(), tenantID, roomID, req.MemberID, req.Body)
	if err != nil {
		switch {
		case errors.Is(err, room.ErrNotFound):
			writeError(w, http.StatusNotFound, "room not found")
		case errors.Is(err, room.ErrRoomTerminated):
			writeError(w, http.StatusConflict, "room is terminated")
		case errors.Is(err, room.ErrTurnBudgetExceeded):
			writeError(w, http.StatusConflict, "room turn budget exceeded")
		case errors.Is(err, room.ErrMemberNotFound):
			writeError(w, http.StatusBadRequest, "member not found in room")
		default:
			h.logger.Error("post message: store error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}

	writeJSON(w, http.StatusCreated, toMessageResponse(msg))
}
