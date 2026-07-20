package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/auth"
)

// updateRequest carries only the client-settable fields for a PATCH operation.
// Immutable fields (id, status, created_by, tenant, timestamps) are not present;
// any such keys in the request body are silently ignored by the JSON decoder.
//
// UpstreamURL, CredentialRef, InvocationRateLimit, and InvocationBurst are
// nullable: a JSON null clears the field, a missing key leaves it unchanged, and
// a value sets it. These use optional[T] which detects presence. See
// optional.toUpdatePointer for the mapping onto UpdateFields' double-pointer
// contract.
type updateRequest struct {
	Name                *string           `json:"name"`
	Description         *string           `json:"description"`
	Tags                *[]string         `json:"tags"`
	Metadata            *map[string]any   `json:"metadata"`
	UpstreamURL         optional[string]  `json:"upstream_url"`
	CredentialRef       optional[string]  `json:"credential_ref"`
	Suspended           *bool             `json:"suspended"`
	InvocationRateLimit optional[float64] `json:"invocation_rate_limit"`
	InvocationBurst     optional[int]     `json:"invocation_burst"`
	// EmitReceipts and CaptureBody are non-nullable toggles: absent leaves them
	// unchanged, a bool sets them. (*bool, not optional[bool], since there is no
	// "clear to null".)
	EmitReceipts *bool `json:"emit_receipts"`
	CaptureBody  *bool `json:"capture_body"`

	// Protocol is not nullable (empty normalizes to "http" on set), so it
	// follows the same *string convention as Name: absent leaves it unchanged.
	Protocol           *string          `json:"protocol"`
	MCPTransport       optional[string] `json:"mcp_transport"`
	MCPProtocolVersion optional[string] `json:"mcp_protocol_version"`

	// Pricing is nullable: a JSON null clears it (unprice the agent), a missing
	// key leaves it unchanged, and an object sets it (spec 0005).
	Pricing optional[pricingPayload] `json:"pricing"`
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())

	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "agent id is required")
		return
	}

	var req updateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name != nil && *req.Name == "" {
		writeError(w, http.StatusBadRequest, "name must not be empty")
		return
	}

	// Validate only when upstream_url is being set to a value; clearing it
	// (present + null) or leaving it (absent) needs no URL check.
	if req.UpstreamURL.Present && req.UpstreamURL.Value != nil {
		if err := validateUpstreamURL(*req.UpstreamURL.Value); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.InvocationRateLimit.Present && req.InvocationRateLimit.Value != nil && *req.InvocationRateLimit.Value <= 0 {
		writeError(w, http.StatusBadRequest, "invocation_rate_limit must be greater than zero")
		return
	}
	if req.InvocationBurst.Present && req.InvocationBurst.Value != nil && *req.InvocationBurst.Value < 1 {
		writeError(w, http.StatusBadRequest, "invocation_burst must be at least 1")
		return
	}

	// protocol / mcp_transport / mcp_protocol_version / pricing form a
	// combination (spec 0004 decision 3, spec 0005 decision 5) that has to be
	// validated against the *resulting* record, not just the fields present in
	// this PATCH — e.g. setting mcp_transport alone while the stored protocol is
	// already "mcp" must still reject stdio/sse, transitioning to protocol=http
	// must not leave a stale mcp_transport behind, and adding per-tool pricing
	// must reject an agent whose resulting protocol is not "mcp". Only fetch the
	// current record when one of these fields is actually being touched.
	if req.Protocol != nil || req.MCPTransport.Present || req.MCPProtocolVersion.Present || req.Pricing.Present {
		current, err := h.store.Get(r.Context(), tenant, id)
		if err != nil {
			if errors.Is(err, agent.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not found")
				return
			}
			h.logger.Error("update agent: store error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		protocol := current.Protocol
		if req.Protocol != nil {
			protocol = *req.Protocol
			if protocol == "" {
				protocol = "http"
			}
		}
		mcpTransport := current.MCPTransport
		if req.MCPTransport.Present {
			mcpTransport = req.MCPTransport.Value
		}
		mcpProtocolVersion := current.MCPProtocolVersion
		if req.MCPProtocolVersion.Present {
			mcpProtocolVersion = req.MCPProtocolVersion.Value
		}
		if err := validateProtocolFields(protocol, mcpTransport, mcpProtocolVersion); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		pricing := current.Pricing
		if req.Pricing.Present {
			pricing = req.Pricing.Value.toPricing()
		}
		if err := validatePricing(pricing, protocol); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// Map the pricing PATCH state onto the store's **Pricing convention:
	// nil=absent, &nil=clear, &p=set.
	var pricingUpdate **agent.Pricing
	if req.Pricing.Present {
		p := req.Pricing.Value.toPricing()
		pricingUpdate = &p
	}

	updated, err := h.store.Update(r.Context(), tenant, id, agent.UpdateFields{
		Name:                req.Name,
		Description:         req.Description,
		Tags:                req.Tags,
		Metadata:            req.Metadata,
		UpstreamURL:         req.UpstreamURL.toUpdatePointer(),
		CredentialRef:       req.CredentialRef.toUpdatePointer(),
		Suspended:           req.Suspended,
		InvocationRateLimit: req.InvocationRateLimit.toUpdatePointer(),
		InvocationBurst:     req.InvocationBurst.toUpdatePointer(),
		EmitReceipts:        req.EmitReceipts,
		CaptureBody:         req.CaptureBody,
		Protocol:            req.Protocol,
		MCPTransport:        req.MCPTransport.toUpdatePointer(),
		MCPProtocolVersion:  req.MCPProtocolVersion.toUpdatePointer(),
		Pricing:             pricingUpdate,
	})
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if errors.Is(err, agent.ErrNameConflict) {
			writeError(w, http.StatusConflict, "agent name already exists in this tenant")
			return
		}
		h.logger.Error("update agent: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, toAgentResponse(updated))
}
