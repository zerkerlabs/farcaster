package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/zerkerlabs/gateway/gateway/internal/policy"
)

// maxClassifierReasonLen bounds how much of an external classifier's
// freeform verdict.Reason is echoed into caller-facing responses (the
// policyWarningHeader and the 403 body's reason field), matching the coarse,
// fixed-format Reason every other Decision in this file already carries.
const maxClassifierReasonLen = 200

// policyWarningHeader carries a coarse description of a warn decision on the
// call's transactional response (spec 0009 §Behavior, §Surface example).
const policyWarningHeader = "X-Zerker-Policy-Warning"

// enforcePolicy is the policy enforcement point (PEP, spec 0009 ticket T4):
// wired inline into handleTransact/handleStream after parseMCPRequest and
// before the x402 gate, so a policy-denied call is never told to pay (spec
// 0009 §Behavior "Enforcement point").
//
// It returns proceed=false once it has already written a response to w (a
// deny, or an internal error) — the caller must return immediately without
// touching x402 or the forwarder. When proceed is true and warnHeader is
// non-empty, a warn rule (or a warn default) matched; the caller carries
// warnHeader onto its eventual response via policyWarningHeader once that
// response is otherwise ready to send (spec 0009 §Surface: "forwarded ...
// flagged").
//
// A tenant with no policy document configured (Get returns ErrNotFound) is a
// no-op: proceed=true, warnHeader="" — the call behaves exactly as it did
// before this surface existed (spec 0009 "No policy configured = no behavior
// change"). h.policyStore being nil (the surface not wired at all, e.g. in
// tests that only call WithProxy) is the same no-op, one layer earlier.
//
// A genuine policy *store* failure (any other Get error) is not a policy
// decision to make with a missing policy document — the tenant's on_error
// posture cannot be read without it. It is reported the same way every other
// store-read failure in this handler already is (see the agent store Get in
// handleTransact/handleStream): logged and a 500, never guessed as a coarse
// policy denial.
func (h *Handler) enforcePolicy(w http.ResponseWriter, r *http.Request, reqCtx policy.RequestContext) (proceed bool, warnHeader string) {
	if h.policyStore == nil {
		return true, ""
	}

	p, err := h.policyStore.Get(r.Context(), reqCtx.TenantID)
	if err != nil {
		if errors.Is(err, policy.ErrNotFound) {
			return true, ""
		}
		h.logger.Error("policy enforcement: load policy", "tenant", reqCtx.TenantID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return false, ""
	}

	d := h.evaluator.Evaluate(p, reqCtx)
	if !isValidAction(d.Action) {
		// The engine returned something the PEP does not know how to enforce
		// (Decision 3's "engine ... errors"). Unlike a store failure, a Policy
		// *was* loaded here, so its own operator-set on_error is knowable —
		// apply it rather than guessing (Decision 3: not hardcoded fail-closed).
		d = policy.Decision{Action: p.OnError, Reason: "policy engine returned an invalid decision; applied on_error"}
	}

	if rule := matchedClassifierRule(p, d); rule != nil {
		d = h.invokeClassifier(r.Context(), reqCtx, p.OnError, *rule.Classifier, d)
	}

	h.recordPolicyDecision(reqCtx, d)

	switch d.Action {
	case policy.ActionDeny:
		// Coarse denial (invariant #3): a single reason, never internal match
		// conditions. d.Reason is already coarse — a rule's 1-based position
		// and a fixed explanation, never rule content (policy.Decision doc).
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "denied by policy", "reason": d.Reason})
		return false, ""
	case policy.ActionWarn:
		return true, d.Reason
	default: // policy.ActionAllow
		return true, ""
	}
}

// recordPolicyDecision hands d off to the configured DecisionRecorder (T5's
// capture seam — spec 0009 "each decision ... is recorded"), async and
// fail-open: capture must never add latency to, or fail, the proxied call it
// describes. A nil recorder (the default until T5 lands) makes this a no-op.
func (h *Handler) recordPolicyDecision(reqCtx policy.RequestContext, d policy.Decision) {
	if h.decisionRecorder == nil {
		return
	}
	rec := policy.RecordedDecision{
		TenantID: reqCtx.TenantID,
		AgentID:  reqCtx.AgentID,
		Protocol: reqCtx.Protocol,
		MCPTool:  reqCtx.MCPTool,
		Decision: d,
	}
	go h.decisionRecorder.Record(context.Background(), rec)
}

// matchedClassifierRule returns the Rule d.MatchedRule identifies, if that
// rule opted into the optional semantic rail (spec 0009 Decision 4, ticket
// T6). It returns nil when d came from Policy.Default (d.MatchedRule == "",
// no rule matched) or when the matched rule has no Classifier configured —
// the "pure-deterministic policies make zero external calls" path (T6
// acceptance) that every existing policy, authored before this ticket, takes.
func matchedClassifierRule(p *policy.Policy, d policy.Decision) *policy.Rule {
	if d.MatchedRule == "" {
		return nil
	}
	idx, err := strconv.Atoi(d.MatchedRule)
	if err != nil || idx < 1 || idx > len(p.Rules) {
		return nil
	}
	rule := p.Rules[idx-1]
	if rule.Classifier == nil {
		return nil
	}
	return &rule
}

// invokeClassifier calls a matched rule's opt-in classifier webhook and maps
// its verdict onto the Decision's Action (spec 0009 Decision 4: "its verdict
// feeds a warn/deny"). It is best-effort: any failure — timeout, unreachable,
// non-2xx, malformed body — falls back to onError, the policy's own
// operator-set on_error posture, exactly like an evaluation engine error
// (Decision 3), never a hang or a guess (T6 acceptance).
func (h *Handler) invokeClassifier(ctx context.Context, reqCtx policy.RequestContext, onError policy.Action, hook policy.ClassifierHook, d policy.Decision) policy.Decision {
	classifyCtx, cancel := context.WithTimeout(ctx, defaultClassifierTimeout)
	defer cancel()

	verdict, err := h.classifierClient.Classify(classifyCtx, hook.URL, policy.ClassifierRequest{
		TenantID:  reqCtx.TenantID,
		AgentID:   reqCtx.AgentID,
		Protocol:  reqCtx.Protocol,
		MCPMethod: reqCtx.MCPMethod,
		MCPTool:   reqCtx.MCPTool,
	})
	if err != nil {
		h.logger.Warn("policy enforcement: classifier hook failed; applied on_error", "tenant", reqCtx.TenantID, "err", err)
		return policy.Decision{Action: onError, MatchedRule: d.MatchedRule, Reason: "classifier hook failed; applied on_error"}
	}
	if !isValidAction(verdict.Action) {
		// Defense in depth: the production ClassifierClient (httpClassifierClient)
		// already rejects a malformed verdict action, but the interface admits any
		// implementation — treat an invalid action exactly like any other
		// malformed-response failure (T6 acceptance: "malformed → on_error").
		h.logger.Warn("policy enforcement: classifier returned an invalid verdict action; applied on_error", "tenant", reqCtx.TenantID, "action", verdict.Action)
		return policy.Decision{Action: onError, MatchedRule: d.MatchedRule, Reason: "classifier returned an invalid verdict; applied on_error"}
	}

	reason := sanitizeClassifierReason(verdict.Reason)
	if reason == "" {
		reason = "matched rule " + d.MatchedRule + " via classifier hook"
	}
	return policy.Decision{Action: verdict.Action, MatchedRule: d.MatchedRule, Reason: reason}
}

// sanitizeClassifierReason strips control characters (which could otherwise
// smuggle a header/response injection via policyWarningHeader) and caps
// length, since verdict.Reason is freeform, unbounded operator-webhook text,
// unlike every other Decision.Reason in this file.
func sanitizeClassifierReason(reason string) string {
	var b strings.Builder
	n := 0
	for _, r := range reason {
		if unicode.IsControl(r) {
			continue
		}
		if n >= maxClassifierReasonLen {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
