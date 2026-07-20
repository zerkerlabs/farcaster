package httpapi

import "fmt"

// mcpTransportStreamableHTTP is the only MCPTransport value v1 accepts for
// protocol=mcp agents (spec 0004 decision 2). "stdio" and "sse" are rejected
// at write time — stdio because it is a client-side, local-process transport
// that does not fit the gateway's stateless HTTP forwarder (decision 1); sse
// (legacy HTTP+SSE) is reserved for a later fast-follow.
const mcpTransportStreamableHTTP = "streamable_http"

// validateProtocolFields checks that protocol, mcpTransport, and
// mcpProtocolVersion form a valid combination (spec 0004 decision 3). protocol
// must already be normalized (empty mapped to "http") before calling.
func validateProtocolFields(protocol string, mcpTransport, mcpProtocolVersion *string) error {
	switch protocol {
	case "http":
		// The MCP fields are only meaningful when protocol=mcp. Reject them on
		// an http agent rather than silently storing a stale/nonsensical value
		// that the proxy surface (spec 0004, #92/#93) would later read.
		if mcpTransport != nil {
			return fmt.Errorf("mcp_transport is only valid when protocol=mcp")
		}
		if mcpProtocolVersion != nil {
			return fmt.Errorf("mcp_protocol_version is only valid when protocol=mcp")
		}
		return nil
	case "mcp":
		if mcpTransport == nil {
			return fmt.Errorf("mcp_transport is required when protocol=mcp")
		}
		if *mcpTransport != mcpTransportStreamableHTTP {
			return fmt.Errorf("mcp_transport: %s is not supported", *mcpTransport)
		}
		return nil
	default:
		return fmt.Errorf("protocol must be %q or %q", "http", "mcp")
	}
}

// isKnownProtocol reports whether p is a valid, non-empty protocol filter
// value ("http" or "mcp"). Used to validate the ?protocol= query param on
// GET /v1/agents (spec 0004 decision 3); the empty string is handled
// separately by callers as "no filter".
func isKnownProtocol(p string) bool {
	return p == "http" || p == "mcp"
}
