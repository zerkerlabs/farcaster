package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/auth"
)

type createRequest struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Tags          []string       `json:"tags"`
	Metadata      map[string]any `json:"metadata"`
	UpstreamURL   *string        `json:"upstream_url"`
	CredentialRef *string        `json:"credential_ref"`
	EmitReceipts  bool           `json:"emit_receipts"`
	CaptureBody   bool           `json:"capture_body"`
	// Protocol is "http" or "mcp"; empty/absent defaults to "http" (spec 0004
	// decision 3).
	Protocol           string  `json:"protocol"`
	MCPTransport       *string `json:"mcp_transport"`
	MCPProtocolVersion *string `json:"mcp_protocol_version"`
	// Pricing is the optional x402 metering configuration (spec 0005); absent
	// leaves the agent unpriced.
	Pricing *pricingPayload `json:"pricing"`
}

type agentResponse struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	Description         string          `json:"description"`
	Tags                []string        `json:"tags"`
	Metadata            map[string]any  `json:"metadata"`
	Status              string          `json:"status"`
	Suspended           bool            `json:"suspended"`
	CreatedBy           string          `json:"created_by"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	UpstreamURL         *string         `json:"upstream_url"`
	CredentialRef       *string         `json:"credential_ref"`
	InvocationRateLimit *float64        `json:"invocation_rate_limit"`
	InvocationBurst     *int            `json:"invocation_burst"`
	EmitReceipts        bool            `json:"emit_receipts"`
	CaptureBody         bool            `json:"capture_body"`
	Protocol            string          `json:"protocol"`
	MCPTransport        *string         `json:"mcp_transport"`
	MCPProtocolVersion  *string         `json:"mcp_protocol_version"`
	Pricing             *pricingPayload `json:"pricing"`
}

// toAgentResponse maps an *agent.Agent to its JSON representation.
func toAgentResponse(a *agent.Agent) agentResponse {
	return agentResponse{
		ID:                  a.ID,
		Name:                a.Name,
		Description:         a.Description,
		Tags:                a.Tags,
		Metadata:            a.Metadata,
		Status:              string(a.Status),
		Suspended:           a.Suspended,
		CreatedBy:           a.CreatedBy,
		CreatedAt:           a.CreatedAt,
		UpdatedAt:           a.UpdatedAt,
		UpstreamURL:         a.UpstreamURL,
		CredentialRef:       a.CredentialRef,
		InvocationRateLimit: a.InvocationRateLimit,
		InvocationBurst:     a.InvocationBurst,
		EmitReceipts:        a.EmitReceipts,
		CaptureBody:         a.CaptureBody,
		Protocol:            a.Protocol,
		MCPTransport:        a.MCPTransport,
		MCPProtocolVersion:  a.MCPProtocolVersion,
		Pricing:             pricingToPayload(a.Pricing),
	}
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())

	// Defense in depth: reject immediately if auth middleware did not run or
	// produced empty identity claims (invariants #1 and #3, AGENTS.md §3).
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	if req.UpstreamURL != nil {
		if err := validateUpstreamURL(*req.UpstreamURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	protocol := req.Protocol
	if protocol == "" {
		protocol = "http"
	}
	if err := validateProtocolFields(protocol, req.MCPTransport, req.MCPProtocolVersion); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	pricing := req.Pricing.toPricing()
	if err := validatePricing(pricing, protocol); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	a := &agent.Agent{
		Name:               req.Name,
		Description:        req.Description,
		Tags:               req.Tags,
		Metadata:           req.Metadata,
		CreatedBy:          user,
		UpstreamURL:        req.UpstreamURL,
		CredentialRef:      req.CredentialRef,
		EmitReceipts:       req.EmitReceipts,
		CaptureBody:        req.CaptureBody,
		Protocol:           protocol,
		MCPTransport:       req.MCPTransport,
		MCPProtocolVersion: req.MCPProtocolVersion,
		Pricing:            pricing,
	}

	if err := h.store.Create(r.Context(), tenant, a); err != nil {
		if errors.Is(err, agent.ErrNameConflict) {
			writeError(w, http.StatusConflict, "agent name already exists in this tenant")
			return
		}
		h.logger.Error("create agent: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, toAgentResponse(a))
}
