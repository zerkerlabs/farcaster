package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/auth"
	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
)

const (
	invocationDefaultLimit = 20
	invocationMaxLimit     = 100

	// invocationReadBodyScope is the OAuth scope required to receive captured
	// request/response bodies from GET /v1/invocations/{id} (spec 0003 §5).
	invocationReadBodyScope = "invocations:read_body"

	// bodyCapBytes is the maximum number of raw bytes returned per body field
	// in GET /v1/invocations/{id}. Bodies larger than this are silently
	// truncated before base64 encoding. 1 MiB is the cap for v1 (spec 0003 §4).
	bodyCapBytes = 1 << 20 // 1 MiB
)

// invocationSettlement is the settlement sub-record (spec 0006) — the
// canonical receipt. Returned only when the invocation has a settlement
// status; unpriced/gate-only invocations omit the block entirely rather than
// returning it with all-null fields.
type invocationSettlement struct {
	Status         string     `json:"status"`
	TxHash         *string    `json:"tx_hash,omitempty"`
	SettledAmount  *string    `json:"settled_amount,omitempty"`
	OperatorAmount *string    `json:"operator_amount,omitempty"`
	FacilitatorFee *string    `json:"facilitator_fee,omitempty"`
	Attempts       *int       `json:"attempts,omitempty"`
	Reason         *string    `json:"reason,omitempty"`
	SettledAt      *time.Time `json:"settled_at,omitempty"`
}

// toInvocationSettlement builds the settlement sub-record from inv, or nil if
// the invocation has no settlement status set (spec 0006 acceptance:
// "unpriced/gate-only invocations show no settlement block").
func toInvocationSettlement(inv *invocation.Invocation) *invocationSettlement {
	if inv.SettlementStatus == nil {
		return nil
	}
	return &invocationSettlement{
		Status:         string(*inv.SettlementStatus),
		TxHash:         inv.SettlementTxHash,
		SettledAmount:  inv.SettledAmount,
		OperatorAmount: inv.OperatorAmount,
		FacilitatorFee: inv.FacilitatorFee,
		Attempts:       inv.SettlementAttempts,
		Reason:         inv.SettlementReason,
		SettledAt:      inv.SettledAt,
	}
}

// invocationListItem is the per-row shape returned by GET /v1/invocations.
// Bodies are never included on the list endpoint (spec 0003).
type invocationListItem struct {
	ID             string                `json:"id"`
	AgentID        string                `json:"agent_id"`
	Mode           string                `json:"mode"`
	Status         string                `json:"status"`
	ErrorClass     *string               `json:"error_class"`
	Model          *string               `json:"model"`
	MCPMethod      *string               `json:"mcp_method"`
	MCPTool        *string               `json:"mcp_tool"`
	PaymentNetwork *string               `json:"payment_network"`
	PaymentAsset   *string               `json:"payment_asset"`
	PaymentAmount  *string               `json:"payment_amount"`
	PaymentPayer   *string               `json:"payment_payer"`
	PaymentNonce   *string               `json:"payment_nonce"`
	Settlement     *invocationSettlement `json:"settlement,omitempty"`
	UpstreamStatus *int                  `json:"upstream_status"`
	LatencyMS      *int64                `json:"latency_ms"`
	TTFTMS         *int64                `json:"ttft_ms"`
	ReqSize        *int64                `json:"req_size"`
	RespSize       *int64                `json:"resp_size"`
	CreatedAt      time.Time             `json:"created_at"`
	CompletedAt    *time.Time            `json:"completed_at"`
}

type invocationListResponse struct {
	Data   []invocationListItem `json:"data"`
	Total  int                  `json:"total"`
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
}

func toInvocationListItem(inv *invocation.Invocation) invocationListItem {
	item := invocationListItem{
		ID:             inv.ID,
		AgentID:        inv.AgentID,
		Mode:           string(inv.Mode),
		Status:         string(inv.Status),
		UpstreamStatus: inv.UpstreamStatus,
		LatencyMS:      inv.LatencyMS,
		TTFTMS:         inv.TTFTMS,
		ReqSize:        inv.RequestSize,
		RespSize:       inv.ResponseSize,
		CreatedAt:      inv.CreatedAt,
		CompletedAt:    inv.CompletedAt,
		Model:          inv.Model,
		MCPMethod:      inv.MCPMethod,
		MCPTool:        inv.MCPTool,
		PaymentNetwork: inv.PaymentNetwork,
		PaymentAsset:   inv.PaymentAsset,
		PaymentAmount:  inv.PaymentAmount,
		PaymentPayer:   inv.PaymentPayer,
		PaymentNonce:   inv.PaymentNonce,
		Settlement:     toInvocationSettlement(inv),
	}
	if inv.ErrorClass != nil {
		ec := string(*inv.ErrorClass)
		item.ErrorClass = &ec
	}
	return item
}

// handleListInvocations handles GET /v1/invocations. It returns a paginated,
// tenant-scoped list of invocation records with optional filters. Bodies are
// never returned on this endpoint (spec 0003 — metadata only on the list).
func (h *Handler) handleListInvocations(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	if tenant == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	q := r.URL.Query()

	// Validate status filter — only known values are accepted (spec 0003).
	var status *invocation.Status
	if sv := q.Get("status"); sv != "" {
		s := invocation.Status(sv)
		switch s {
		case invocation.StatusPending, invocation.StatusRunning,
			invocation.StatusSucceeded, invocation.StatusFailed:
			status = &s
		default:
			writeError(w, http.StatusBadRequest,
				"invalid status: must be one of pending, running, succeeded, failed")
			return
		}
	}

	// Validate mode filter — only known values are accepted (spec 0003).
	var mode *invocation.Mode
	if mv := q.Get("mode"); mv != "" {
		m := invocation.Mode(mv)
		switch m {
		case invocation.ModeTransactional, invocation.ModeStreaming:
			mode = &m
		default:
			writeError(w, http.StatusBadRequest,
				"invalid mode: must be one of transactional, streaming")
			return
		}
	}

	// Parse RFC 3339 time bounds (spec 0003 filter contract).
	var since, until *time.Time
	if sv := q.Get("since"); sv != "" {
		t, err := time.Parse(time.RFC3339, sv)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid since: must be RFC 3339 timestamp")
			return
		}
		since = &t
	}
	if uv := q.Get("until"); uv != "" {
		t, err := time.Parse(time.RFC3339, uv)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid until: must be RFC 3339 timestamp")
			return
		}
		until = &t
	}

	// Offset pagination: limit clamped to max 100 (spec 0003 / spec 0001 convention).
	limit := queryInt(r, "limit", invocationDefaultLimit)
	offset := queryInt(r, "offset", 0)
	if limit < 1 {
		limit = invocationDefaultLimit
	}
	if limit > invocationMaxLimit {
		limit = invocationMaxLimit
	}
	if offset < 0 {
		offset = 0
	}

	// error_class is passed through without validation: invalid values simply
	// return empty results rather than 400, matching the treatment of unknown
	// model values — neither is an enumerated contract type on the wire.
	var errorClass *invocation.ErrorClass
	if ecv := q.Get("error_class"); ecv != "" {
		ec := invocation.ErrorClass(ecv)
		errorClass = &ec
	}

	// Validate the settlement filter — an enumerated contract type (spec 0006
	// sub-record status), only known values are accepted.
	var settlementStatus *invocation.SettlementStatus
	if sv := q.Get("settlement"); sv != "" {
		s := invocation.SettlementStatus(sv)
		switch s {
		case invocation.SettlementStatusPending, invocation.SettlementStatusSettled,
			invocation.SettlementStatusSettlementFailed, invocation.SettlementStatusSettledUpstreamFailed:
			settlementStatus = &s
		default:
			writeError(w, http.StatusBadRequest,
				"invalid settlement: must be one of pending, settled, settlement_failed, settled_upstream_failed")
			return
		}
	}

	filter := invocation.ListFilter{
		AgentID:          q.Get("agent_id"),
		Status:           status,
		Mode:             mode,
		ErrorClass:       errorClass,
		Model:            q.Get("model"),
		SettlementStatus: settlementStatus,
		Since:            since,
		Until:            until,
		Limit:            limit,
		Offset:           offset,
	}

	invs, total, err := h.invocations.ListFiltered(r.Context(), tenant, filter)
	if err != nil {
		h.logger.Error("list invocations: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	items := make([]invocationListItem, len(invs))
	for i, inv := range invs {
		items[i] = toInvocationListItem(inv)
	}

	writeJSON(w, http.StatusOK, invocationListResponse{
		Data:   items,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// invocationDetailResponse is the shape returned by GET /v1/invocations/{id}.
// BodyCaptured is always present. ReqBody and RespBody are only included when
// both capture was on for the row AND the caller holds the
// invocations:read_body scope (spec 0003 §4, §5).
type invocationDetailResponse struct {
	ID             string                `json:"id"`
	AgentID        string                `json:"agent_id"`
	Mode           string                `json:"mode"`
	Status         string                `json:"status"`
	ErrorClass     *string               `json:"error_class"`
	Model          *string               `json:"model"`
	MCPMethod      *string               `json:"mcp_method"`
	MCPTool        *string               `json:"mcp_tool"`
	PaymentNetwork *string               `json:"payment_network"`
	PaymentAsset   *string               `json:"payment_asset"`
	PaymentAmount  *string               `json:"payment_amount"`
	PaymentPayer   *string               `json:"payment_payer"`
	PaymentNonce   *string               `json:"payment_nonce"`
	Settlement     *invocationSettlement `json:"settlement,omitempty"`
	UpstreamStatus *int                  `json:"upstream_status"`
	LatencyMS      *int64                `json:"latency_ms"`
	TTFTMS         *int64                `json:"ttft_ms"`
	ReqSize        *int64                `json:"req_size"`
	RespSize       *int64                `json:"resp_size"`
	BodyCaptured   bool                  `json:"body_captured"`
	ReqBody        *string               `json:"req_body,omitempty"`
	RespBody       *string               `json:"resp_body,omitempty"`
	CreatedAt      time.Time             `json:"created_at"`
	CompletedAt    *time.Time            `json:"completed_at"`
}

// handleGetInvocation handles GET /v1/invocations/{id}. It returns full
// invocation metadata for the caller's tenant; cross-tenant IDs return 404
// (not 403) per the established convention (AGENTS.md invariant #2).
//
// req_body and resp_body are included only when both of the following hold:
//  1. Capture was on for that invocation row (RequestBody/ResponseBody non-nil).
//  2. The caller's token carries the invocations:read_body OAuth scope.
//
// When either condition is absent the body fields are omitted; body_captured
// still reflects whether capture was on.
func (h *Handler) handleGetInvocation(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	if tenant == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	id := r.PathValue("id")
	inv, err := h.invocations.Get(r.Context(), tenant, id)
	if err != nil {
		if errors.Is(err, invocation.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.logger.Error("get invocation: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasScope := auth.HasScope(r.Context(), invocationReadBodyScope)
	writeJSON(w, http.StatusOK, toInvocationDetailResponse(inv, hasScope))
}

func toInvocationDetailResponse(inv *invocation.Invocation, hasReadBody bool) invocationDetailResponse {
	bodyCaptured := inv.RequestBody != nil || inv.ResponseBody != nil

	resp := invocationDetailResponse{
		ID:             inv.ID,
		AgentID:        inv.AgentID,
		Mode:           string(inv.Mode),
		Status:         string(inv.Status),
		UpstreamStatus: inv.UpstreamStatus,
		LatencyMS:      inv.LatencyMS,
		TTFTMS:         inv.TTFTMS,
		ReqSize:        inv.RequestSize,
		RespSize:       inv.ResponseSize,
		BodyCaptured:   bodyCaptured,
		CreatedAt:      inv.CreatedAt,
		CompletedAt:    inv.CompletedAt,
		Model:          inv.Model,
		MCPMethod:      inv.MCPMethod,
		MCPTool:        inv.MCPTool,
		PaymentNetwork: inv.PaymentNetwork,
		PaymentAsset:   inv.PaymentAsset,
		PaymentAmount:  inv.PaymentAmount,
		PaymentPayer:   inv.PaymentPayer,
		PaymentNonce:   inv.PaymentNonce,
		Settlement:     toInvocationSettlement(inv),
	}

	if inv.ErrorClass != nil {
		ec := string(*inv.ErrorClass)
		resp.ErrorClass = &ec
	}

	if bodyCaptured && hasReadBody {
		if inv.RequestBody != nil {
			s := encodeBodyCapped(inv.RequestBody)
			resp.ReqBody = &s
		}
		if inv.ResponseBody != nil {
			s := encodeBodyCapped(inv.ResponseBody)
			resp.RespBody = &s
		}
	}

	return resp
}

// encodeBodyCapped base64-encodes b after silently truncating to bodyCapBytes.
func encodeBodyCapped(b []byte) string {
	if len(b) > bodyCapBytes {
		b = b[:bodyCapBytes]
	}
	return base64.StdEncoding.EncodeToString(b)
}
