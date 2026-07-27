package httpapi

import (
	"errors"
	"net/http"

	"github.com/zerkerlabs/farcaster/rooms/internal/room"
	"github.com/zerkerlabs/farcaster/rooms/internal/tenant"
)

func (h *Handler) handleGetRoom(w http.ResponseWriter, r *http.Request) {
	tenantID := tenant.FromContext(r.Context())
	if tenantID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	roomID := r.PathValue("rom_id")

	got, err := h.store.GetRoom(r.Context(), tenantID, roomID)
	if err != nil {
		if errors.Is(err, room.ErrNotFound) {
			writeError(w, http.StatusNotFound, "room not found")
			return
		}
		h.logger.Error("get room: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// The store's own ErrNotFound scoping means transcript lookup cannot fail
	// here for a reason a caller could hit: the room was just found under the
	// same tenant and ID.
	transcript, err := h.store.Messages(r.Context(), tenantID, roomID)
	if err != nil {
		h.logger.Error("get room: transcript replay error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, toRoomResponse(got, transcript))
}
