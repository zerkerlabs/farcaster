package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/zerkerlabs/farcaster/rooms/internal/room"
	"github.com/zerkerlabs/farcaster/rooms/internal/tenant"
)

// addMemberRequest is the request body for POST /v1/rooms/{rom_id}/members.
type addMemberRequest struct {
	AgentID string `json:"agent_id"`
}

// handleAddMember seats an agent in a room. Rooms are single-tenant in v1
// (there is no gateway client yet to resolve an agent's owning tenant — that
// lands with the proxy client), so the added agent is assumed to belong to
// the caller's own tenant, same as the room it joins.
func (h *Handler) handleAddMember(w http.ResponseWriter, r *http.Request) {
	tenantID := tenant.FromContext(r.Context())
	if tenantID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	roomID := r.PathValue("rom_id")

	var req addMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}

	member, err := h.store.AddMember(r.Context(), tenantID, roomID, req.AgentID, tenantID)
	if err != nil {
		switch {
		case errors.Is(err, room.ErrNotFound):
			writeError(w, http.StatusNotFound, "room not found")
		case errors.Is(err, room.ErrRoomTerminated):
			writeError(w, http.StatusConflict, "room is terminated")
		case errors.Is(err, room.ErrTenantMismatch):
			writeError(w, http.StatusBadRequest, "agent belongs to a different tenant")
		default:
			h.logger.Error("add member: store error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}

	writeJSON(w, http.StatusCreated, toMemberResponse(member))
}
