package httpapi

import (
	"net/http"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/auth"
	"github.com/zerkerlabs/gateway/gateway/internal/policy"
)

const (
	policyDecisionDefaultLimit = 20
	policyDecisionMaxLimit     = 100
)

// policyDecisionItem is the per-row shape returned by GET /v1/policy/decisions
// (spec 0009 §Behavior "Audit capture": action, matched rule, reason, agent,
// tool). reason is the coarse, rule-position explanation — never call content
// (invariant #3). mcp_tool is null for protocol=http calls.
type policyDecisionItem struct {
	ID          string        `json:"id"`
	AgentID     string        `json:"agent_id"`
	Protocol    string        `json:"protocol"`
	MCPTool     *string       `json:"mcp_tool"`
	Action      policy.Action `json:"action"`
	MatchedRule string        `json:"matched_rule"`
	Reason      string        `json:"reason"`
	CreatedAt   time.Time     `json:"created_at"`
}

type policyDecisionListResponse struct {
	Data  []policyDecisionItem `json:"data"`
	Limit int                  `json:"limit"`
}

func toPolicyDecisionItem(d *policy.StoredDecision) policyDecisionItem {
	return policyDecisionItem{
		ID:          d.ID,
		AgentID:     d.AgentID,
		Protocol:    d.Protocol,
		MCPTool:     d.MCPTool,
		Action:      d.Action,
		MatchedRule: d.MatchedRule,
		Reason:      d.Reason,
		CreatedAt:   d.CreatedAt,
	}
}

// handleListPolicyDecisions handles GET /v1/policy/decisions: the calling
// tenant's most-recent policy decisions, newest first (spec 0009, ticket T5 —
// the "OSS captures" read side). Tenant-scoped by the store; a tenant only ever
// sees its own decisions, so there is no cross-tenant record to leak.
func (h *Handler) handleListPolicyDecisions(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Offset-free recent feed: limit clamped to [1, max], mirroring the
	// invocations list convention.
	limit := queryInt(r, "limit", policyDecisionDefaultLimit)
	if limit < 1 {
		limit = policyDecisionDefaultLimit
	}
	if limit > policyDecisionMaxLimit {
		limit = policyDecisionMaxLimit
	}

	decisions, err := h.decisionStore.ListRecent(r.Context(), tenant, limit)
	if err != nil {
		h.logger.Error("list policy decisions: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	items := make([]policyDecisionItem, len(decisions))
	for i, d := range decisions {
		items[i] = toPolicyDecisionItem(d)
	}

	writeJSON(w, http.StatusOK, policyDecisionListResponse{Data: items, Limit: limit})
}
