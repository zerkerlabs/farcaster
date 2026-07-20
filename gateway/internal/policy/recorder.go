package policy

import "context"

// DecisionRecorder captures a Decision made during inline enforcement
// (ticket T4) so a persistence layer (ticket T5, GET /v1/policy/decisions)
// can later expose it to the tenant. Implementations must be safe for
// concurrent use; Record may block (e.g. a store write), so callers invoke it
// off the request path — capture is best-effort and must never affect the
// proxied call the Decision describes. A nil DecisionRecorder means capture
// is disabled (the default, until T5 wires a persisted implementation);
// there is no separate no-op type, mirroring receipt.Emitter.
type DecisionRecorder interface {
	Record(ctx context.Context, d RecordedDecision)
}

// RecordedDecision is one evaluated Decision plus the identifying fields spec
// 0009 says every decision must carry ("each decision (action, matched rule,
// reason, agent, tool) is recorded").
type RecordedDecision struct {
	TenantID string
	AgentID  string
	Protocol string
	MCPTool  *string
	Decision Decision
}
