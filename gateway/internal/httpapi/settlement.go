package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/auth"
	"github.com/zerkerlabs/gateway/gateway/internal/credential"
	"github.com/zerkerlabs/gateway/gateway/internal/settlement"
	"github.com/zerkerlabs/gateway/gateway/internal/ssrf"
)

// settlementConfigRequest carries the PATCH /v1/settlement/config fields. Both
// are optional: an absent field leaves the current value unchanged. The first
// configure call for a tenant must supply both (spec 0006 Decision 1).
type settlementConfigRequest struct {
	FacilitatorURL           *string `json:"facilitator_url"`
	FacilitatorCredentialRef *string `json:"facilitator_credential_ref"`
}

// settlementConfigResponse is the JSON shape returned by PATCH/GET
// /v1/settlement/config. facilitator_credential_ref is the opaque reference
// only — the credential's plaintext never appears here (invariant #4,
// AGENTS.md).
type settlementConfigResponse struct {
	FacilitatorURL           string    `json:"facilitator_url"`
	FacilitatorCredentialRef string    `json:"facilitator_credential_ref"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

func toSettlementConfigResponse(c *settlement.Config) settlementConfigResponse {
	return settlementConfigResponse{
		FacilitatorURL:           c.FacilitatorURL,
		FacilitatorCredentialRef: c.FacilitatorCredentialRef,
		CreatedAt:                c.CreatedAt,
		UpdatedAt:                c.UpdatedAt,
	}
}

// validateFacilitatorURL requires a well-formed https URL with a host (spec
// 0006 acceptance). The facilitator URL is tenant-supplied egress — the same
// class of risk as upstream_url — so the same private/loopback/link-local
// hygiene as validateUpstreamURL applies here, restricted to https only
// (facilitator auth travels over this connection).
func validateFacilitatorURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid facilitator_url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("facilitator_url must be an https URL")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("facilitator_url must include a host")
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("facilitator_url must not target localhost")
	}
	if ip := net.ParseIP(host); ip != nil && ssrf.IsBlockedIP(ip) {
		return fmt.Errorf("facilitator_url must not target a private, loopback, or link-local address")
	}
	return nil
}

func (h *Handler) handleGetSettlementConfig(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	cfg, err := h.settlementStore.Get(r.Context(), tenant)
	if err != nil {
		if errors.Is(err, settlement.ErrNotFound) {
			writeError(w, http.StatusNotFound, "settlement not configured")
			return
		}
		h.logger.Error("get settlement config: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, toSettlementConfigResponse(cfg))
}

func (h *Handler) handlePatchSettlementConfig(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req settlementConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.FacilitatorURL != nil {
		if *req.FacilitatorURL == "" {
			writeError(w, http.StatusBadRequest, "facilitator_url must not be empty")
			return
		}
		if err := validateFacilitatorURL(*req.FacilitatorURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if req.FacilitatorCredentialRef != nil {
		if *req.FacilitatorCredentialRef == "" {
			writeError(w, http.StatusBadRequest, "facilitator_credential_ref must not be empty")
			return
		}
		if _, err := h.credSvc.Get(r.Context(), tenant, *req.FacilitatorCredentialRef); err != nil {
			if errors.Is(err, credential.ErrNotFound) {
				writeError(w, http.StatusBadRequest, "facilitator_credential_ref does not resolve to a credential owned by this tenant")
				return
			}
			h.logger.Error("patch settlement config: credential lookup error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	cfg, err := h.settlementStore.Upsert(r.Context(), tenant, settlement.UpdateFields{
		FacilitatorURL:           req.FacilitatorURL,
		FacilitatorCredentialRef: req.FacilitatorCredentialRef,
	})
	if err != nil {
		if errors.Is(err, settlement.ErrIncomplete) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.logger.Error("patch settlement config: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, toSettlementConfigResponse(cfg))
}
