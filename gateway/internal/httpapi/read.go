package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/auth"
)

type listResponse struct {
	Agents  []agentResponse `json:"agents"`
	Page    int             `json:"page"`
	PerPage int             `json:"per_page"`
	Total   int             `json:"total"`
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	if tenant == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	page := queryInt(r, "page", 1)
	perPage := queryInt(r, "per_page", 50)
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 50
	}

	protocol := r.URL.Query().Get("protocol")
	if protocol != "" && !isKnownProtocol(protocol) {
		writeError(w, http.StatusBadRequest, `protocol must be "http" or "mcp"`)
		return
	}

	agents, total, err := h.store.List(r.Context(), tenant, page, perPage, protocol)
	if err != nil {
		h.logger.Error("list agents: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	resp := listResponse{
		Agents:  make([]agentResponse, len(agents)),
		Page:    page,
		PerPage: perPage,
		Total:   total,
	}
	for i, a := range agents {
		resp.Agents[i] = toAgentResponse(a)
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	if tenant == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	id := r.PathValue("id")
	a, err := h.store.Get(r.Context(), tenant, id)
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.logger.Error("get agent: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, toAgentResponse(a))
}

func queryInt(r *http.Request, key string, def int) int {
	s := r.URL.Query().Get(key)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}
