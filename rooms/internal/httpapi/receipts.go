package httpapi

import (
	"errors"
	"net/http"

	"github.com/zerkerlabs/gateway/rooms/internal/auth"
	"github.com/zerkerlabs/gateway/rooms/internal/room"
)

// handleGetReceipts serves GET /v1/rooms/{rom_id}/receipts: the receipts
// recorded for that room, read back through the receipt seam
// (rooms/internal/receipt). It confirms the room exists for the caller's
// tenant before reading receipts, so a cross-tenant request 404s exactly as
// handleGetRoom does — identically to a genuinely-missing room — rather than
// reaching the receipt seam with a tenant it does not own the room under.
func (h *Handler) handleGetReceipts(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantFromContext(r.Context())
	if tenantID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	roomID := r.PathValue("rom_id")

	if _, err := h.store.GetRoom(r.Context(), tenantID, roomID); err != nil {
		if errors.Is(err, room.ErrNotFound) {
			writeError(w, http.StatusNotFound, "room not found")
			return
		}
		h.logger.Error("get receipts: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	receipts, err := h.emitter.ListByRoom(r.Context(), tenantID, roomID)
	if err != nil {
		h.logger.Error("get receipts: emitter error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, toReceiptsResponse(receipts))
}
