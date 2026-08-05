package httpapi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/httpapi"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
)

// seedPricedAgent creates an active agent priced at the given flat amount,
// with optional per-tool overrides, and returns its ID.
func seedPricedAgent(t *testing.T, store agent.AgentStore, upstreamURL, protocol, amount string, tools map[string]string) string {
	t.Helper()
	a := &agent.Agent{
		Name:        "priced-agent",
		CreatedBy:   testUser,
		UpstreamURL: &upstreamURL,
		Protocol:    protocol,
		Pricing: &agent.Pricing{
			Amount:  amount,
			Asset:   "USDC",
			Network: "base",
			PayTo:   "0x1111111111111111111111111111111111111111",
			Tools:   tools,
		},
	}
	if err := store.Create(context.Background(), testTenant, a); err != nil {
		t.Fatalf("seedPricedAgent: %v", err)
	}
	return a.ID
}

// decodeX402Challenge unmarshals a 402 response body into a generic map for
// field-by-field assertions against the x402 wire shape (spec 0005 Surface).
func decodeX402Challenge(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal challenge: %v; body = %s", err, body)
	}
	return resp
}

// seededPayTo is the pay_to address seedPricedAgent configures. Well-formed test
// payments target it so the parameter checks pass and the outcome turns on the
// verifier alone.
const seededPayTo = "0x1111111111111111111111111111111111111111"

// wellFormedX402Header returns a base64-encoded X-PAYMENT value that decodes and
// passes checkPaymentParams for a seedPricedAgent (network "base", recipient
// seededPayTo, value >= the route price, valid for the next hour). It carries a
// placeholder signature: whether the request is accepted then depends solely on
// the injected PaymentVerifier.
func wellFormedX402Header(t *testing.T, value string) string {
	t.Helper()
	now := time.Now().UTC().Unix()
	payload := map[string]any{
		"x402Version": 1,
		"scheme":      "exact",
		"network":     "base",
		"payload": map[string]any{
			"signature": "0xdeadbeef",
			"authorization": map[string]any{
				"from":        "0x2222222222222222222222222222222222222222",
				"to":          seededPayTo,
				"value":       value,
				"validAfter":  strconv.FormatInt(now-60, 10),
				"validBefore": strconv.FormatInt(now+3600, 10),
				"nonce":       "0x00",
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal x402 payload: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestHandleTransact_X402_FailsClosedOnUnverifiableSignature(t *testing.T) {
	t.Parallel()

	agentStore := agent.NewMemoryStore()
	agentID := seedPricedAgent(t, agentStore, "https://upstream.example.com/invoke", "http", "10000", nil)
	invStore := invocation.NewMemoryStore()
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(&mockForwarder{result: fakeResult(200, `{"ok":true}`)}, invStore)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
	req.Header.Set("X-PAYMENT", wellFormedX402Header(t, "10000"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// A well-formed payment passes decode + parameter checks, but the default
	// Base/USDC verifier cannot verify this dummy signature → fail-closed 402.
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (fail-closed: unverifiable signature); body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if resp["error"] != "payment verification failed" {
		t.Errorf("error = %q, want coarse %q (no internal detail)", resp["error"], "payment verification failed")
	}
	// No invocation is created when verification fails.
	if _, total, err := invStore.List(context.Background(), testTenant, agentID, 1, 50); err != nil || total != 0 {
		t.Errorf("invocations recorded for a rejected payment = %d, want 0 (err=%v)", total, err)
	}
}

func TestHandleStream_X402_FailsClosedOnUnverifiableSignature(t *testing.T) {
	t.Parallel()

	var agentID string
	h, _ := newProxyHandler(t, func(s agent.AgentStore) {
		agentID = seedPricedAgent(t, s, "https://upstream.example.com/stream", "http", "20000", nil)
	}, &mockForwarder{streamResult: fakeResult(200, `{"ok":true}`)})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser)
	req.Header.Set("X-PAYMENT", wellFormedX402Header(t, "20000"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (fail-closed: unverifiable signature); body = %s", rec.Code, rec.Body.String())
	}
	// The gate fires before the invocation is created, so no invocation-ID header.
	if got := rec.Header().Get("X-Zerker-Invocation-ID"); got != "" {
		t.Errorf("X-Zerker-Invocation-ID = %q, want empty — gate rejects before invocation", got)
	}
}

func TestHandleTransact_X402Challenge_NoPayment(t *testing.T) {
	t.Parallel()

	agentStore := agent.NewMemoryStore()
	agentID := seedPricedAgent(t, agentStore, "https://upstream.example.com/invoke", "http", "10000", nil)
	invStore := invocation.NewMemoryStore()
	h := httpapi.NewHandler(agentStore, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(&mockForwarder{result: fakeResult(200, `{}`)}, invStore)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body = %s", rec.Code, rec.Body.String())
	}

	resp := decodeX402Challenge(t, rec.Body.Bytes())
	if v, _ := resp["x402Version"].(float64); v != 1 {
		t.Errorf("x402Version = %v, want 1", resp["x402Version"])
	}
	accepts, ok := resp["accepts"].([]any)
	if !ok || len(accepts) != 1 {
		t.Fatalf("accepts = %v, want a single-element array", resp["accepts"])
	}
	entry := accepts[0].(map[string]any)
	if entry["scheme"] != "exact" {
		t.Errorf("scheme = %v, want %q", entry["scheme"], "exact")
	}
	if entry["network"] != "base" {
		t.Errorf("network = %v, want %q", entry["network"], "base")
	}
	if entry["maxAmountRequired"] != "10000" {
		t.Errorf("maxAmountRequired = %v, want %q (flat route price)", entry["maxAmountRequired"], "10000")
	}
	if entry["payTo"] != "0x1111111111111111111111111111111111111111" {
		t.Errorf("payTo = %v, want the agent's configured pay_to", entry["payTo"])
	}
	if entry["resource"] != "/v1/proxy/"+agentID {
		t.Errorf("resource = %v, want %q", entry["resource"], "/v1/proxy/"+agentID)
	}
	if entry["asset"] == "" || entry["asset"] == nil {
		t.Errorf("asset = %v, want a non-empty token contract address", entry["asset"])
	}
	if entry["maxTimeoutSeconds"] == nil {
		t.Error("maxTimeoutSeconds is missing")
	}

	// No invocation should be created for a 402 challenge — it never forwards.
	if _, total, err := invStore.List(context.Background(), testTenant, agentID, 1, 50); err != nil || total != 0 {
		t.Errorf("invocations recorded for a 402 challenge = %d, want 0 (err=%v)", total, err)
	}
}

func TestHandleTransact_X402Challenge_PerToolOverride(t *testing.T) {
	t.Parallel()

	var agentID string
	h, _ := newProxyHandler(t, func(s agent.AgentStore) {
		agentID = seedPricedAgent(t, s, "https://upstream.example.com/invoke", "mcp", "10000",
			map[string]string{"search_docs": "50000"})
	}, &mockForwarder{result: fakeResult(200, `{}`)})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_docs"}}`)
	req := authedPostRequest(t, "/v1/proxy/"+agentID, body, testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeX402Challenge(t, rec.Body.Bytes())
	entry := resp["accepts"].([]any)[0].(map[string]any)
	if entry["maxAmountRequired"] != "50000" {
		t.Errorf("maxAmountRequired = %v, want %q (per-tool override)", entry["maxAmountRequired"], "50000")
	}
}

func TestHandleTransact_X402Challenge_NoToolMatchUsesRoutePrice(t *testing.T) {
	t.Parallel()

	var agentID string
	h, _ := newProxyHandler(t, func(s agent.AgentStore) {
		agentID = seedPricedAgent(t, s, "https://upstream.example.com/invoke", "mcp", "10000",
			map[string]string{"search_docs": "50000"})
	}, &mockForwarder{result: fakeResult(200, `{}`)})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"other_tool"}}`)
	req := authedPostRequest(t, "/v1/proxy/"+agentID, body, testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeX402Challenge(t, rec.Body.Bytes())
	entry := resp["accepts"].([]any)[0].(map[string]any)
	if entry["maxAmountRequired"] != "10000" {
		t.Errorf("maxAmountRequired = %v, want %q (route price, no tool match)", entry["maxAmountRequired"], "10000")
	}
}

func TestHandleTransact_UnpricedAgentUnaffected(t *testing.T) {
	t.Parallel()

	var agentID string
	h, _ := newProxyHandler(t, func(s agent.AgentStore) {
		agentID = seedActiveAgent(t, s, "https://upstream.example.com/invoke")
	}, &mockForwarder{result: fakeResult(200, `{}`)})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for an unpriced agent with no X-PAYMENT; body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleStream_X402Challenge_NoPayment(t *testing.T) {
	t.Parallel()

	var agentID string
	h, _ := newProxyHandler(t, func(s agent.AgentStore) {
		agentID = seedPricedAgent(t, s, "https://upstream.example.com/stream", "http", "20000", nil)
	}, &mockForwarder{})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := authedPostRequest(t, "/v1/proxy/"+agentID+"/stream", []byte(`{}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeX402Challenge(t, rec.Body.Bytes())
	entry := resp["accepts"].([]any)[0].(map[string]any)
	if entry["maxAmountRequired"] != "20000" {
		t.Errorf("maxAmountRequired = %v, want %q", entry["maxAmountRequired"], "20000")
	}
	if entry["resource"] != "/v1/proxy/"+agentID+"/stream" {
		t.Errorf("resource = %v, want %q", entry["resource"], "/v1/proxy/"+agentID+"/stream")
	}
	if got := rec.Header().Get("X-Zerker-Invocation-ID"); got != "" {
		t.Errorf("X-Zerker-Invocation-ID = %q, want empty — no invocation for a 402 challenge", got)
	}
}
