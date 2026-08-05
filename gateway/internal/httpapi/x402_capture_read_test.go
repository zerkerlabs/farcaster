package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
)

// TestHandleTransact_UnpricedAgentLeavesPaymentFieldsNull is the regression
// guard for "unpriced/http calls leave fields NULL" (spec 0005 T5 acceptance):
// an unpriced agent's invocation must carry no payment metadata.
func TestHandleTransact_UnpricedAgentLeavesPaymentFieldsNull(t *testing.T) {
	t.Parallel()

	fwd := &mockForwarder{result: fakeResult(200, `{"ok":true}`)}
	mux, agentStore, invStore := mcpTestHandler(t, fwd)
	agentID := seedActiveAgent(t, agentStore, "https://upstream.example.com/invoke")

	post := authedPostRequest(t, "/v1/proxy/"+agentID, []byte(`{}`), testTenant, testUser)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, post)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	invID := rec.Header().Get("X-Zerker-Invocation-ID")
	waitForStatus(t, invStore, invID, invocation.StatusSucceeded)

	got := pollInvocation(t, mux, agentID, invID)
	for _, key := range []string{"payment_network", "payment_asset", "payment_amount", "payment_payer", "payment_nonce"} {
		if v, ok := got[key]; !ok || v != nil {
			t.Errorf("%s = %v (present=%v), want present and null for an unpriced call", key, v, ok)
		}
	}
}

// TestInvocationReads_ExposePaymentFields verifies the list and detail read
// endpoints surface the x402 payment metadata straight from the stored record
// (spec 0005 T5, mirroring surface-4's mcp_method/mcp_tool exposure).
func TestInvocationReads_ExposePaymentFields(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	network, asset, amount, payer, nonce := "base", "USDC", "10000", "0x2222222222222222222222222222222222222222", "0x00"
	inv := seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID:        "agt_x",
		Mode:           invocation.ModeTransactional,
		Status:         invocation.StatusSucceeded,
		PaymentNetwork: &network,
		PaymentAsset:   &asset,
		PaymentAmount:  &amount,
		PaymentPayer:   &payer,
		PaymentNonce:   &nonce,
	})

	// Detail endpoint.
	detail := decodeDetailResponse(t, getInvocation(t, mux, inv.ID, nil).Body.Bytes())
	checkPaymentFields := func(where string, m map[string]any) {
		t.Helper()
		if m["payment_network"] != network {
			t.Errorf("%s payment_network = %v, want %q", where, m["payment_network"], network)
		}
		if m["payment_asset"] != asset {
			t.Errorf("%s payment_asset = %v, want %q", where, m["payment_asset"], asset)
		}
		if m["payment_amount"] != amount {
			t.Errorf("%s payment_amount = %v, want %q", where, m["payment_amount"], amount)
		}
		if m["payment_payer"] != payer {
			t.Errorf("%s payment_payer = %v, want %q", where, m["payment_payer"], payer)
		}
		if m["payment_nonce"] != nonce {
			t.Errorf("%s payment_nonce = %v, want %q", where, m["payment_nonce"], nonce)
		}
	}
	checkPaymentFields("detail", detail)

	// List endpoint.
	list := decodeListResponse(t, listInvocations(t, mux, "").Body.Bytes())
	if len(list.Data) != 1 {
		t.Fatalf("list returned %d rows, want 1", len(list.Data))
	}
	checkPaymentFields("list", list.Data[0])
}

// TestInvocationReads_PaymentFieldsNullWhenAbsent locks the JSON contract for a
// non-priced invocation: all five payment fields must be present-and-null on
// the list and detail reads, not omitted.
func TestInvocationReads_PaymentFieldsNullWhenAbsent(t *testing.T) {
	t.Parallel()

	h, invStore := newInvocationListHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	inv := seedInvocation(t, invStore, testTenant, &invocation.Invocation{
		AgentID: "agt_http",
		Mode:    invocation.ModeTransactional,
		Status:  invocation.StatusSucceeded,
		// Payment* fields left nil — an unpriced invocation.
	})

	assertPresentNull := func(where string, m map[string]any) {
		t.Helper()
		for _, key := range []string{"payment_network", "payment_asset", "payment_amount", "payment_payer", "payment_nonce"} {
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
