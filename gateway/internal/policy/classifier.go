package policy

import "context"

// ClassifierHook is a rule's opt-in configuration for the optional semantic
// rail (spec 0009 Decision 4, ticket T6): delegate this rule's decision to an
// operator-configured external classifier webhook (e.g. Llama Guard / Lakera /
// an LLM-judge) instead of a fixed Action. Farcaster bundles no classifier and
// calls nothing unless a rule sets this.
type ClassifierHook struct {
	// URL is the operator-configured classifier endpoint. It is egress the
	// same way a tenant's upstream_url is, so it is subject to the same SSRF
	// guard (internal/ssrf) at both the authoring boundary and dial time (T6
	// acceptance).
	URL string `json:"url"`
}

// ClassifierRequest is the minimal request context POSTed to a classifier
// webhook when a rule with a Classifier hook matches. It carries only the
// call's structural identity, mirroring the deterministic engine's own inputs
// (RequestContext) — never request or response body content (T6 scope
// boundary excludes response-body inspection).
type ClassifierRequest struct {
	TenantID  string  `json:"tenant_id"`
	AgentID   string  `json:"agent_id"`
	Protocol  string  `json:"protocol"`
	MCPMethod *string `json:"mcp_method,omitempty"`
	MCPTool   *string `json:"mcp_tool,omitempty"`
}

// ClassifierVerdict is a classifier webhook's JSON response body: the Action
// its verdict maps to (spec 0009 Decision 4 — "its verdict feeds a
// warn/deny"), plus an optional coarse reason. Action must be one of the
// three well-formed Action values; any other value is treated as malformed —
// the caller falls back to the policy's on_error, exactly as a timeout or
// non-2xx response would.
type ClassifierVerdict struct {
	Action Action `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// ClassifierClient calls an operator-configured classifier webhook on behalf
// of a matched rule. Implementations must be best-effort: a bounded timeout
// and no retry, so a slow or unreachable webhook can never hang the request
// path (spec 0009 Decision 4), and must apply the same SSRF guard used for
// tenant-supplied upstream URLs before dialing hookURL (T6 acceptance).
type ClassifierClient interface {
	Classify(ctx context.Context, hookURL string, req ClassifierRequest) (ClassifierVerdict, error)
}
