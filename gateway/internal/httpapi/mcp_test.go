package httpapi

import (
	"strings"
	"testing"
)

// deref returns the pointed-to string, or a sentinel "<nil>" so table tests can
// assert nil vs. a value in one comparison.
func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestParseMCPRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantMethod string
		wantTool   string
	}{
		{
			name:       "tools/list captures method, tool nil",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			wantMethod: "tools/list",
			wantTool:   "<nil>",
		},
		{
			name:       "tools/call captures method and params.name",
			body:       `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_docs","arguments":{"query":"SSRF"}}}`,
			wantMethod: "tools/call",
			wantTool:   "search_docs",
		},
		{
			name:       "initialize captures method, tool nil",
			body:       `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
			wantMethod: "initialize",
			wantTool:   "<nil>",
		},
		{
			name:       "tools/call without params.name leaves tool nil",
			body:       `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"arguments":{}}}`,
			wantMethod: "tools/call",
			wantTool:   "<nil>",
		},
		{
			name:       "params.name on a non-tools/call method is ignored",
			body:       `{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"name":"file://x"}}`,
			wantMethod: "resources/read",
			wantTool:   "<nil>",
		},
		{
			name:       "malformed JSON yields no capture",
			body:       `{"jsonrpc":"2.0","method":`,
			wantMethod: "<nil>",
			wantTool:   "<nil>",
		},
		{
			name:       "empty body yields no capture",
			body:       ``,
			wantMethod: "<nil>",
			wantTool:   "<nil>",
		},
		{
			name:       "valid JSON without a method yields no capture",
			body:       `{"jsonrpc":"2.0","id":5,"result":{}}`,
			wantMethod: "<nil>",
			wantTool:   "<nil>",
		},
		{
			name:       "non-object JSON yields no capture",
			body:       `"just a string"`,
			wantMethod: "<nil>",
			wantTool:   "<nil>",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			method, tool := parseMCPRequest([]byte(tc.body))
			if got := deref(method); got != tc.wantMethod {
				t.Errorf("method = %q, want %q", got, tc.wantMethod)
			}
			if got := deref(tool); got != tc.wantTool {
				t.Errorf("tool = %q, want %q", got, tc.wantTool)
			}
		})
	}
}

// TestParseMCPRequest_TruncatedPrefixYieldsNoCapture documents the streaming
// peek's graceful-degradation contract: if a bounded prefix cuts the JSON-RPC
// envelope mid-value, parsing fails cleanly (no panic, no partial capture)
// rather than affecting the request. A method whose envelope fits in the peek
// is still captured even when a trailing arguments blob is truncated away.
func TestParseMCPRequest_TruncatedPrefixYieldsNoCapture(t *testing.T) {
	t.Parallel()

	full := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"search_docs","arguments":{"blob":"` +
		strings.Repeat("x", 4096) + `"}}}`

	// Cut the body partway through the arguments blob — after params.name but
	// before the closing braces.
	truncated := full[:120]
	method, tool := parseMCPRequest([]byte(truncated))
	if method != nil || tool != nil {
		t.Errorf("truncated body: got method=%v tool=%v, want both nil", deref(method), deref(tool))
	}
}
