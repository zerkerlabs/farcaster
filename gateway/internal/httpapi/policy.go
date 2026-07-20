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

	"github.com/zerkerlabs/farcaster/gateway/internal/auth"
	"github.com/zerkerlabs/farcaster/gateway/internal/policy"
	"github.com/zerkerlabs/farcaster/gateway/internal/ssrf"
)

// putPolicyRequest carries the PUT /v1/policy fields. Unlike PATCH
// /v1/settlement/config, this is a wholesale replace (spec 0009 §Surface —
// "Replace the calling tenant's policy document"), so both default and
// on_error are required on every call, matching policy.PutFields.
type putPolicyRequest struct {
	Default policy.Action `json:"default"`
	OnError policy.Action `json:"on_error"`
	Rules   []policy.Rule `json:"rules"`
}

// getPolicyResponse is the JSON shape returned by GET /v1/policy. A tenant
// with no policy document gets the zero-value response (version 0, empty
// default/on_error, empty rules, no updated_at) rather than a 404 — spec 0009
// T2 acceptance: "or an empty/absent default". Rule already carries json tags
// matching the spec's authoring example, so it doubles as the response shape.
type getPolicyResponse struct {
	Version   int           `json:"version"`
	Default   policy.Action `json:"default,omitempty"`
	OnError   policy.Action `json:"on_error,omitempty"`
	Rules     []policy.Rule `json:"rules"`
	UpdatedAt *time.Time    `json:"updated_at,omitempty"`
}

// putPolicyResponse is the JSON shape returned by PUT /v1/policy (spec 0009
// §Surface authoring example) — version and updated_at only, not the full
// document the caller just sent.
type putPolicyResponse struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toGetPolicyResponse(p *policy.Policy) getPolicyResponse {
	rules := p.Rules
	if rules == nil {
		rules = []policy.Rule{}
	}
	updatedAt := p.UpdatedAt
	return getPolicyResponse{
		Version:   p.Version,
		Default:   p.Default,
		OnError:   p.OnError,
		Rules:     rules,
		UpdatedAt: &updatedAt,
	}
}

// isValidAction reports whether a is one of the three well-formed Action
// values (spec 0009 Decision 3). Used to validate a document's default,
// on_error, and every rule's action at the authoring boundary.
func isValidAction(a policy.Action) bool {
	switch a {
	case policy.ActionAllow, policy.ActionWarn, policy.ActionDeny:
		return true
	default:
		return false
	}
}

// validatePolicyDocument applies write-time validation to a PUT /v1/policy
// document, mirroring the validateUpstreamURL pattern: known actions for
// default/on_error/every rule, and well-formed match conditions. A malformed
// document must never reach the store — migration 015 validates nothing
// structurally on rules, relying entirely on this boundary check.
func validatePolicyDocument(def, onError policy.Action, rules []policy.Rule) error {
	if !isValidAction(def) {
		return fmt.Errorf("default must be one of %q, %q, %q", policy.ActionAllow, policy.ActionWarn, policy.ActionDeny)
	}
	if !isValidAction(onError) {
		return fmt.Errorf("on_error must be one of %q, %q, %q", policy.ActionAllow, policy.ActionWarn, policy.ActionDeny)
	}
	for i, rule := range rules {
		if !isValidAction(rule.Action) {
			return fmt.Errorf("rules[%d].action must be one of %q, %q, %q", i, policy.ActionAllow, policy.ActionWarn, policy.ActionDeny)
		}
		if err := validatePolicyMatch(rule.Match); err != nil {
			return fmt.Errorf("rules[%d].match: %w", i, err)
		}
		if rule.Classifier != nil {
			if err := validateClassifierURL(rule.Classifier.URL); err != nil {
				return fmt.Errorf("rules[%d].classifier: %w", i, err)
			}
		}
	}
	return nil
}

// validateClassifierURL applies the same write-time SSRF hygiene as
// validateUpstreamURL to a rule's opt-in classifier webhook URL (spec 0009
// Decision 4, ticket T6 acceptance: "same SSRF guard as upstreams"). It is a
// distinct function, mirroring validateFacilitatorURL, so error messages name
// the right field; the outbound call itself is re-checked at dial time via
// ssrf.SafeDialContext (NewHTTPClassifierClient), closing the DNS-rebinding
// TOCTOU window this input-time check alone cannot.
func validateClassifierURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid classifier url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("classifier url must be an http or https URL")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("classifier url must include a host")
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("classifier url must not target localhost")
	}
	if ip := net.ParseIP(host); ip != nil && ssrf.IsBlockedIP(ip) {
		return fmt.Errorf("classifier url must not target a private, loopback, or link-local address")
	}
	return nil
}

// validatePolicyMatch checks a single rule's match conditions are well-formed:
// no empty identifiers, tool wildcards only as a single trailing "*" (spec
// 0009 §Surface — "read_*"), and non-negative/positive size and rate bounds.
func validatePolicyMatch(m policy.Match) error {
	for _, agentID := range m.Agents {
		if agentID == "" {
			return errors.New("agents must not contain an empty value")
		}
	}
	for _, tool := range m.Tools {
		if tool == "" {
			return errors.New("tools must not contain an empty value")
		}
		if strings.Contains(tool, "*") && (!strings.HasSuffix(tool, "*") || strings.Count(tool, "*") > 1) {
			return fmt.Errorf("tools entry %q: wildcard must be a single trailing \"*\"", tool)
		}
	}
	if m.MaxBodyBytes != nil && *m.MaxBodyBytes < 0 {
		return errors.New("max_body_bytes must not be negative")
	}
	if m.RatePerMin != nil && *m.RatePerMin <= 0 {
		return errors.New("rate_per_min must be positive")
	}
	return nil
}

func (h *Handler) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	p, err := h.policyStore.Get(r.Context(), tenant)
	if err != nil {
		if errors.Is(err, policy.ErrNotFound) {
			writeJSON(w, http.StatusOK, getPolicyResponse{Rules: []policy.Rule{}})
			return
		}
		h.logger.Error("get policy: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, toGetPolicyResponse(p))
}

func (h *Handler) handlePutPolicy(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req putPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := validatePolicyDocument(req.Default, req.OnError, req.Rules); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	p, err := h.policyStore.Put(r.Context(), tenant, policy.PutFields{
		Default: req.Default,
		OnError: req.OnError,
		Rules:   req.Rules,
	})
	if err != nil {
		h.logger.Error("put policy: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, putPolicyResponse{Version: p.Version, UpdatedAt: p.UpdatedAt})
}
