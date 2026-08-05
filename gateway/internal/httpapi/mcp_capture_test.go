package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/httpapi"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
	"github.com/zerkerlabs/farcaster/gateway/internal/proxy"
)

// mcpTestHandler wires a proxy handler around the given forwarder plus a fresh
// agent + invocation store, returning a routed mux and the invocation store so
// tests can both drive the endpoints and inspect the persisted record.
func mcpTestHandler(t *testing.T, fwd httpapi.ProxyForwarder) (*http.ServeMux, agent.AgentStore, *invocation.MemoryStore) {
	t.Helper()
	agentStore := agent.NewMemoryStore()
	invStore := invocation.NewMemoryStore()
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(fwd, invStore)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux, agentStore, invStore
}

// pollInvocation issues GET /v1/proxy/{agentID}/invocations/{invID} and decodes
// the response body into a generic map so tests can assert both a field's value
// and its presence (a null field must still appear in the JSON).
func pollInvocation(t *testing.T, mux *http.ServeMux, agentID, invID string) map[string]any {
	t.Helper()
	req := authedGETRequest(t, "/v1/proxy/"+agentID+"/invocations/"+invID, testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	return decodeDetailResponse(t, rec.Body.Bytes())
}

// TestHandleTransact_MCPToolsCallCaptured verifies the transactional path parses
// a tools/call JSON-RPC body for an mcp agent and exposes mcp_method/mcp_tool on
// the poll read (spec 0004, Decision 7 acceptance).
func TestHandleTransact_MCPToolsCallCaptured(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, `{"jsonrpc":"2.0","id":2,"result":{}}`)}
	mux, agentStore, invStore := mcpTestHandler(t, fwd)
	agentID := seedMCPAgent(t, agentStore, "https://mcp-upstream.example.com/")

	body := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_docs","arguments":{"query":"SSRF"}}}`)
	post := authedPostRequest(t, "/v1/proxy/"+agentID, body, testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, post)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

	got := pollInvocation(t, mux, agentID, invID)
	if got["mcp_method"] != "tools/call" {
		t.Errorf("mcp_method = %v, want tools/call", got["mcp_method"])
	}
	if got["mcp_tool"] != "search_docs" {
		t.Errorf("mcp_tool = %v, want search_docs", got["mcp_tool"])
	}
}

// TestHandleTransact_MCPToolsListNullTool verifies a discovery call records the
// method with a null tool — and that the null field is still present on the read
// (the spec's poll example shows "mcp_tool": null explicitly).
func TestHandleTransact_MCPToolsListNullTool(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, `{"jsonrpc":"2.0","id":1,"result":{}}`)}
	mux, agentStore, invStore := mcpTestHandler(t, fwd)
	agentID := seedMCPAgent(t, agentStore, "https://mcp-upstream.example.com/")

	post := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, post)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", rec.Code)
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

	got := pollInvocation(t, mux, agentID, invID)
	if got["mcp_method"] != "tools/list" {
		t.Errorf("mcp_method = %v, want tools/list", got["mcp_method"])
	}
	if v, ok := got["mcp_tool"]; !ok || v != nil {
		t.Errorf("mcp_tool = %v (present=%v), want present and null", v, ok)
	}
}

// TestHandleTransact_HTTPAgentLeavesMCPFieldsNull is the regression guard for
// "http agents unaffected": an http agent whose body happens to look like MCP
// JSON-RPC must not have the fields populated (parsing is gated on protocol).
func TestHandleTransact_HTTPAgentLeavesMCPFieldsNull(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, "ok")}
	mux, agentStore, invStore := mcpTestHandler(t, fwd)
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/")

	// A body that would parse as tools/call if this were an mcp agent.
	body := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_docs"}}`)
	post := authedPostRequest(t, "/v1/proxy/"+agentID, body, testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, post)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", rec.Code)
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	inv := waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

	if inv.MCPMethod != nil {
		t.Errorf("MCPMethod = %q, want nil for http agent", *inv.MCPMethod)
	}
	if inv.MCPTool != nil {
		t.Errorf("MCPTool = %q, want nil for http agent", *inv.MCPTool)
	}
}

// streamBodyForwarder records the exact request body handed to DoStream so a
// test can prove the streaming MCP peek reconstructs the body byte-for-byte
// before forwarding (the peek must never alter the pipe).
type streamBodyForwarder struct {
	mockForwarder
	gotBody []byte
}

func (f *streamBodyForwarder) DoStream(ctx context.Context, tenantID, agentID, invocationID string, r *http.Request) (*proxy.Result, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	f.gotBody = b
	return f.mockForwarder.DoStream(ctx, tenantID, agentID, invocationID, r)
}

// TestHandleStream_MCPCaptureAndForwardIntact verifies the streaming path both
// captures mcp_method/mcp_tool and forwards the original body verbatim — the
// bounded peek + MultiReader reconstruction must be transparent to the pipe.
func TestHandleStream_MCPCaptureAndForwardIntact(t *testing.T) {
	t.Parallel()

	fwd := &streamBodyForwarder{mockForwarder: mockForwarder{streamResult: fakeResult(200, "event: message\ndata: {}\n\n")}}
	mux, agentStore, invStore := mcpTestHandler(t, fwd)
	agentID := seedMCPAgent(t, agentStore, "https://mcp-upstream.example.com/stream")

	body := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_docs","arguments":{"query":"SSRF"}}}`)
	post := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", body, testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, post) // handleStream runs inline, so the record is terminal on return.
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if string(fwd.gotBody) != string(body) {
		t.Errorf("forwarded body = %q, want verbatim %q", fwd.gotBody, body)
	}

	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	inv, err := invStore.Get(context.Background(), testTenant, invID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if inv.MCPMethod == nil || *inv.MCPMethod != "tools/call" {
		t.Errorf("MCPMethod = %v, want tools/call", inv.MCPMethod)
	}
	if inv.MCPTool == nil || *inv.MCPTool != "search_docs" {
		t.Errorf("MCPTool = %v, want search_docs", inv.MCPTool)
	}
}

// TestInvocationReads_ExposeMCPFields verifies the list and detail read
// endpoints surface mcp_method/mcp_tool straight from the stored record.
func TestInvocationReads_ExposeMCPFields(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	method := "tools/call"
	tool := "search_docs"
	inv := seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID:   "agt_x",
		Mode:      invocation.ModeStreaming,
		Status:    invocation.StatusSucceeded,
		MCPMethod: &method,
		MCPTool:   &tool,
	})

	// Detail endpoint.
	detail := decodeDetailResponse(t, getInvocation(t, mux, inv.ID, nil).Body.Bytes())
	if detail["mcp_method"] != "tools/call" || detail["mcp_tool"] != "search_docs" {
		t.Errorf("detail mcp fields = (%v, %v), want (tools/call, search_docs)", detail["mcp_method"], detail["mcp_tool"])
	}

	// List endpoint.
	list := decodeListResponse(t, listInvocations(t, mux, "").Body.Bytes())
	if len(list.Data) != 1 {
		t.Fatalf("list returned %d rows, want 1", len(list.Data))
	}
	if list.Data[0]["mcp_method"] != "tools/call" || list.Data[0]["mcp_tool"] != "search_docs" {
		t.Errorf("list mcp fields = (%v, %v), want (tools/call, search_docs)", list.Data[0]["mcp_method"], list.Data[0]["mcp_tool"])
	}
}

// TestInvocationReads_MCPFieldsNullWhenAbsent locks the JSON contract for a
// non-MCP invocation: both fields must be present-and-null on the list and
// detail reads, not omitted (a client reading the shape must see the keys).
func TestInvocationReads_MCPFieldsNullWhenAbsent(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	inv := seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_http",
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusSucceeded,
		// MCPMethod / MCPTool left nil — a plain http invocation.
	})

	assertPresentNull := func(where string, m map[string]any) {
		t.Helper()
		for _, key := range []string{"mcp_method", "mcp_tool"} {
			v, ok := m[key]
			if !ok || v != nil {
				t.Errorf("%s: %s = %v (present=%v), want present and null", where, key, v, ok)
			}
		}
	}

	assertPresentNull("detail", decodeDetailResponse(t, getInvocation(t, mux, inv.ID, nil).Body.Bytes()))

	list := decodeListResponse(t, listInvocations(t, mux, "").Body.Bytes())
	if len(list.Data) != 1 {
		t.Fatalf("list returned %d rows, want 1", len(list.Data))
	}
	assertPresentNull("list", list.Data[0])
}
