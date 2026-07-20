package httpapi

import "encoding/json"

// maxMCPParseBytes bounds how many leading bytes of a streaming MCP request the
// gateway reads to parse the JSON-RPC method/tool for observability (spec 0004,
// Decision 7). The streaming path forwards request chunks verbatim (spec 0002
// Q6 — hefty payloads), so we peek only this prefix and forward the body
// unchanged afterwards.
//
// The captured fields — `method` and `params.name` — sit in the JSON-RPC
// envelope, which a well-formed MCP request emits before the (potentially large)
// `arguments` blob, so 64 KiB captures them for any realistic request while
// keeping the per-stream buffer small under concurrency. A `tools/call` whose
// `name` falls beyond this prefix simply records a nil tool; parsing is
// best-effort and never affects forwarding. (A larger cap would not help against
// a client that deliberately orders `arguments` before `name` — that evades
// capture at any bound — so there is nothing to buy by holding more in memory.)
// The transactional path parses the fully buffered body and is not bounded by
// this.
const maxMCPParseBytes = 64 << 10 // 64 KiB

// parseMCPRequest extracts the JSON-RPC method and, for a "tools/call", the
// params.name tool identifier from an MCP request body (spec 0004, Decision 7).
//
// Both returns are nil on any parse failure or when the field is absent, so a
// malformed or truncated body yields no capture rather than an error: this is
// observability metadata layered onto a transparent pipe (Decision 5) and must
// never change whether or how the request is forwarded. The tool is returned
// only for method=="tools/call"; every other method leaves it nil, matching the
// spec's `mcp_tool: null` for discovery calls like tools/list.
//
// A JSON-RPC batch (a top-level array) is intentionally not parsed: MCP dropped
// batching in the 2025-06-18 transport revision, so v1 handles a single request
// per invocation. A batch body fails the unmarshal below and records no
// method/tool rather than guessing which element to attribute the invocation to.
func parseMCPRequest(body []byte) (method, tool *string) {
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		return nil, nil
	}

	m := req.Method
	method = &m
	if req.Method == "tools/call" && req.Params.Name != "" {
		t := req.Params.Name
		tool = &t
	}
	return method, tool
}
