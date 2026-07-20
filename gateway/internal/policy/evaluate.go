package policy

import (
	"strconv"
	"strings"
)

// Protocol values a RequestContext may carry, mirroring agent.Agent.Protocol
// (spec 0004): "http" agents are route-level only, "mcp" agents additionally
// carry a parsed tool. Declared here (rather than imported from the agent
// package) to keep this package's only dependency the standard library.
const (
	ProtocolHTTP = "http"
	ProtocolMCP  = "mcp"
)

// RequestContext is the deterministic, in-flight-call context an Evaluator
// resolves against a Policy (spec 0009 §Behavior "Deterministic inputs"):
// tenant/caller identity plus the call's structural shape. Every field is
// available mid-request with no network I/O or model call to populate it.
type RequestContext struct {
	TenantID string
	// Scopes carries the caller's OAuth scopes (auth.ScopesFromContext).
	// No v1 Match condition keys on scope (scope-based rules are not part of
	// the T1 schema); it is threaded through so the interface already carries
	// the full context this surface's request context is defined over,
	// leaving scope-aware rules a forward-compatible addition rather than a
	// signature change.
	Scopes []string
	// AgentID is the calling agent's ID (Match.Agents).
	AgentID string
	// Protocol is the agent's wire protocol: ProtocolHTTP or ProtocolMCP.
	Protocol string
	// MCPMethod is the parsed JSON-RPC method (parseMCPRequest); nil for
	// protocol=http calls or when parsing found none.
	MCPMethod *string
	// MCPTool is the parsed tool name for a protocol=mcp "tools/call"
	// (parseMCPRequest's params.name; Match.Tools). Nil for protocol=http
	// calls, for non-"tools/call" methods, and when parsing found none.
	MCPTool *string
	// BodyBytes is the request body size in bytes (Match.MaxBodyBytes).
	BodyBytes int64
	// RatePerMin is the caller's observed request rate, precomputed by the
	// caller from the existing rate-limit machinery (internal/ratelimit) and
	// compared against Match.RatePerMin. The engine measures nothing itself —
	// it stays a pure function of its inputs (spec 0009 "no network I/O").
	RatePerMin int
}

// Decision is the outcome of evaluating a Policy against a RequestContext
// (spec 0009 §Surface: "{action, matched_rule, reason}").
type Decision struct {
	Action Action
	// MatchedRule identifies the rule that produced Action, as its 1-based
	// position in Policy.Rules (e.g. "3") — stable, deterministic, and safe to
	// surface to the caller (e.g. X-Farcaster-Policy-Warning) without leaking
	// match conditions. Empty when Action came from Policy.Default because no
	// rule matched.
	MatchedRule string
	// Reason is a short, human-readable explanation of Action. For a deny
	// this is intentionally coarse (invariant #3, AGENTS.md — a 403 must never
	// leak ruleset detail); callers enforcing deny should prefer their own
	// fixed message over echoing Reason externally.
	Reason string
}

// Evaluator resolves a Policy + RequestContext to a Decision. Implementations
// must be deterministic and side-effect-free — no network I/O, no model call
// (spec 0009 §Behavior) — so the same (policy, ctx) pair always yields the
// same Decision. The interface exists so the evaluation strategy is swappable
// (spec 0009 Decision 2); BuiltinEvaluator is the v1 implementation.
type Evaluator interface {
	Evaluate(p *Policy, ctx RequestContext) Decision
}

// BuiltinEvaluator is the v1 Evaluator: a minimal, dependency-free, pure-Go
// implementation that walks Policy.Rules in order and applies the first
// match, falling back to Policy.Default (spec 0009 Decision 2 — adopt Cedar
// only if a dependency-light, pure-Go evaluator fits; this rule set is a flat
// first-match-wins list over five scalar/slice conditions with a non-binary
// action (allow/warn/deny, where Cedar's permit/forbid has no third state),
// so a ~100-line built-in evaluator is simpler and lighter than adopting and
// translating into Cedar's entity/schema model for no behavioral gain).
// Zero-value ready; construct with NewBuiltinEvaluator for clarity at call
// sites.
type BuiltinEvaluator struct{}

// NewBuiltinEvaluator returns a ready-to-use BuiltinEvaluator.
func NewBuiltinEvaluator() *BuiltinEvaluator {
	return &BuiltinEvaluator{}
}

// Evaluate implements Evaluator. A nil policy allows unconditionally: the
// primary "tenant has no policy" path is the store's ErrNotFound, handled by
// the caller before Evaluate is ever invoked (spec 0009 "No policy configured
// = no behavior change"); this is a defensive fallback for that same posture,
// not a code path v1 wiring is expected to exercise.
func (BuiltinEvaluator) Evaluate(p *Policy, ctx RequestContext) Decision {
	if p == nil {
		return Decision{Action: ActionAllow, Reason: "no policy configured"}
	}

	for i, rule := range p.Rules {
		if rule.Match.matches(ctx) {
			ruleNum := strconv.Itoa(i + 1)
			return Decision{
				Action:      rule.Action,
				MatchedRule: ruleNum,
				Reason:      "matched rule " + ruleNum,
			}
		}
	}

	return Decision{
		Action: p.Default,
		Reason: "no rule matched; applied policy default",
	}
}

// matches reports whether every condition m carries applies to ctx. An
// unpopulated field is not a condition — it matches any value (Match's
// doc comment) — so a zero-value Match matches every RequestContext.
func (m Match) matches(ctx RequestContext) bool {
	if len(m.Agents) > 0 && !containsString(m.Agents, ctx.AgentID) {
		return false
	}
	if len(m.Tools) > 0 {
		// Tool-level matching only applies to protocol=mcp calls (spec 0009
		// Decision 6); protocol=http calls have no tool to compare against,
		// so a rule with a Tools condition never matches one.
		if ctx.Protocol != ProtocolMCP || ctx.MCPTool == nil || !matchesTool(m.Tools, *ctx.MCPTool) {
			return false
		}
	}
	if m.MaxBodyBytes != nil && ctx.BodyBytes < *m.MaxBodyBytes {
		return false
	}
	if m.RatePerMin != nil && ctx.RatePerMin <= *m.RatePerMin {
		return false
	}
	return true
}

// containsString reports whether needle is present in haystack.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// matchesTool reports whether tool matches any of patterns. A pattern ending
// in "*" matches any tool sharing its prefix (spec 0009 §Surface example:
// "read_*"); any other pattern must match tool exactly.
func matchesTool(patterns []string, tool string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(tool, prefix) {
				return true
			}
			continue
		}
		if p == tool {
			return true
		}
	}
	return false
}
