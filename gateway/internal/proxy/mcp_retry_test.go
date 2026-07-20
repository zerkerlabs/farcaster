package proxy

import "testing"

// TestMcpRetryAllowed covers the retry-safety allowlist gate directly (spec
// 0004, Decision 6): only the four known-idempotent methods are retriable,
// tools/call and any unrecognised method are not, and malformed/batch bodies
// fail closed (no retry) rather than defaulting to allow.
func TestMcpRetryAllowed(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, true},
		{"tools/list", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, true},
		{"resources/list", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, true},
		{"prompts/list", `{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`, true},
		{"tools/call", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`, false},
		{"unrecognised method", `{"jsonrpc":"2.0","id":1,"method":"notifications/cancelled"}`, false},
		{"empty method", `{"jsonrpc":"2.0","id":1,"method":""}`, false},
		{"missing method", `{"jsonrpc":"2.0","id":1}`, false},
		{"malformed json", `{not json`, false},
		{"batch array", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, false},
		{"empty body", ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mcpRetryAllowed([]byte(tt.body)); got != tt.want {
				t.Errorf("mcpRetryAllowed(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}
