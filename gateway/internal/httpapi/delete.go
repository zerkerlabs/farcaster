package httpapi

import (
	"errors"
	"net/http"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/auth"
)

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())

	// Defense in depth: invariants #1 and #3, AGENTS.md §3.
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	id := r.PathValue("id")

	if err := h.store.Delete(r.Context(), tenant, id); err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.logger.Error("delete agent: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
