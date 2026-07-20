package proxy

import "encoding/json"

// mcpRetryAllowedMethods is the set of MCP JSON-RPC methods known to be
// idempotent and therefore safe to retry after a transient upstream failure
// (spec 0004, Decision 6). tools/call is deliberately absent: it can execute a
// side-effecting tool, and an automatic retry risks double-executing it.
var mcpRetryAllowedMethods = map[string]bool{
	"initialize":     true,
	"tools/list":     true,
	"resources/list": true,
	"prompts/list":   true,
}

// mcpRequestMethod extracts the JSON-RPC "method" field from an MCP request
// body for retry-safety gating. It mirrors the method extraction performed for
// observability capture (httpapi.parseMCPRequest, spec 0004 Decision 7) but is
// re-implemented here because internal/proxy cannot import internal/httpapi
// (httpapi already imports proxy; importing back would cycle). Returns
// ("", false) on any parse failure or an absent/empty method.
func mcpRequestMethod(body []byte) (string, bool) {
	var req struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		return "", false
	}
	return req.Method, true
}

// mcpRetryAllowed reports whether a Protocol=mcp request may be retried after a
// retriable upstream error. It fails closed: a method that cannot be
// determined (parse failure, batch request, empty body) is treated as
// non-idempotent, matching the "never retry tools/call" intent for any
// non-allowlisted or unrecognised method (spec 0004, Decision 6).
func mcpRetryAllowed(body []byte) bool {
	method, ok := mcpRequestMethod(body)
	return ok && mcpRetryAllowedMethods[method]
}
