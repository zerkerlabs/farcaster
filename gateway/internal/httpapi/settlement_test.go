package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/gateway/gateway/internal/credential"
	"github.com/zerkerlabs/gateway/gateway/internal/httpapi"
	"github.com/zerkerlabs/gateway/gateway/internal/settlement"
)

// newSettlementHandler returns a Handler wired with fresh in-memory
// credential and settlement stores, plus the credential Service used to seed
// owned credentials in test setup.
func newSettlementHandler(t *testing.T) (*httpapi.Handler, *credential.Service) {
	t.Helper()
	credSvc := newCredService(t)
	h := httpapi.NewHandler(agent.NewMemoryStore(), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentials(credSvc).
		WithSettlement(settlement.NewMemoryStore())
	return h, credSvc
}

// settlementReq builds a request to /v1/settlement/config with body and auth
// context; passing tenant and user both empty produces an unauthenticated
// request.
func settlementReq(t *testing.T, method string, body any, tenant, user string) *http.Request {
	t.Helper()
	var b []byte
	if body != nil {
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, "/v1/settlement/config", bytes.NewReader(b))
	if len(b) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if tenant == "" && user == "" {
		return req
	}
	return req.WithContext(authtest.WithIdentity(req.Context(), tenant, user))
}

func TestGetSettlementConfig_AbsentReturns404(t *testing.T) {
	t.Parallel()

	h, _ := newSettlementHandler(t)
	rec := serve(t, h, settlementReq(t, http.MethodGet, nil, testTenant, testUser))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestGetSettlementConfig_NoAuth(t *testing.T) {
	t.Parallel()

	h, _ := newSettlementHandler(t)
	rec := serve(t, h, settlementReq(t, http.MethodGet, nil, "", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPatchSettlementConfig_RoundTrip(t *testing.T) {
	t.Parallel()

	h, credSvc := newSettlementHandler(t)
	cred := seedCredential(t, credSvc, "facilitator-auth", "facilitator-secret-value")

	body := map[string]any{
		"facilitator_url":            "https://settle.example.com",
		"facilitator_credential_ref": cred.ID,
	}
	rec := serve(t, h, settlementReq(t, http.MethodPatch, body, testTenant, testUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["facilitator_url"] != "https://settle.example.com" {
		t.Errorf("facilitator_url = %v", resp["facilitator_url"])
	}
	if resp["facilitator_credential_ref"] != cred.ID {
		t.Errorf("facilitator_credential_ref = %v, want %v", resp["facilitator_credential_ref"], cred.ID)
	}

	// GET must round-trip the same values.
	getRec := serve(t, h, settlementReq(t, http.MethodGet, nil, testTenant, testUser))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d, body = %s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	var getResp map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if getResp["facilitator_url"] != "https://settle.example.com" {
		t.Errorf("GET facilitator_url = %v", getResp["facilitator_url"])
	}
}

func TestPatchSettlementConfig_InvalidURL(t *testing.T) {
	t.Parallel()

	h, credSvc := newSettlementHandler(t)
	cred := seedCredential(t, credSvc, "facilitator-auth", "facilitator-secret-value")

	tests := []struct {
		name string
		url  string
	}{
		{"http not https", "http://settle.example.com"},
		{"not a url", "not-a-url"},
		{"loopback", "https://127.0.0.1/settle"},
		{"private ip", "https://10.0.0.5/settle"},
		{"no host", "https:///settle"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := map[string]any{
				"facilitator_url":            tc.url,
				"facilitator_credential_ref": cred.ID,
			}
			rec := serve(t, h, settlementReq(t, http.MethodPatch, body, testTenant, testUser))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

func TestPatchSettlementConfig_NonOwnedCredentialRef(t *testing.T) {
	t.Parallel()

	h, _ := newSettlementHandler(t)

	// facilitator_credential_ref below is a deliberately unresolvable ID, not a
	// real secret (gosec G101 false positive).
	body := map[string]any{ //nolint:gosec // not a credential value, an unresolvable ID
		"facilitator_url":            "https://settle.example.com",
		"facilitator_credential_ref": "cred_does_not_exist",
	}
	rec := serve(t, h, settlementReq(t, http.MethodPatch, body, testTenant, testUser))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestPatchSettlementConfig_CredentialFromAnotherTenantRejected(t *testing.T) {
	t.Parallel()

	h, credSvc := newSettlementHandler(t)
	// Owned by a different tenant than the caller.
	other := seedCredential(t, credSvc, "other-tenant-cred", "secret-value-xyz")

	body := map[string]any{
		"facilitator_url":            "https://settle.example.com",
		"facilitator_credential_ref": other.ID,
	}
	rec := serve(t, h, settlementReq(t, http.MethodPatch, body, "some-other-tenant", testUser))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestPatchSettlementConfig_FirstConfigureRequiresBothFields(t *testing.T) {
	t.Parallel()

	h, credSvc := newSettlementHandler(t)
	cred := seedCredential(t, credSvc, "facilitator-auth", "facilitator-secret-value")

	rec := serve(t, h, settlementReq(t, http.MethodPatch,
		map[string]any{"facilitator_url": "https://settle.example.com"}, testTenant, testUser))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	rec = serve(t, h, settlementReq(t, http.MethodPatch,
		map[string]any{"facilitator_credential_ref": cred.ID}, testTenant, testUser))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestPatchSettlementConfig_PartialUpdateAfterConfigured(t *testing.T) {
	t.Parallel()

	h, credSvc := newSettlementHandler(t)
	cred1 := seedCredential(t, credSvc, "facilitator-auth-1", "facilitator-secret-value-1")
	cred2 := seedCredential(t, credSvc, "facilitator-auth-2", "facilitator-secret-value-2")

	rec := serve(t, h, settlementReq(t, http.MethodPatch, map[string]any{
		"facilitator_url":            "https://settle.example.com",
		"facilitator_credential_ref": cred1.ID,
	}, testTenant, testUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("initial PATCH status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Only rotate the credential ref; the URL must be left unchanged.
	rec = serve(t, h, settlementReq(t, http.MethodPatch, map[string]any{
		"facilitator_credential_ref": cred2.ID,
	}, testTenant, testUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("partial PATCH status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["facilitator_url"] != "https://settle.example.com" {
		t.Errorf("facilitator_url changed unexpectedly: %v", resp["facilitator_url"])
	}
	if resp["facilitator_credential_ref"] != cred2.ID {
		t.Errorf("facilitator_credential_ref = %v, want %v", resp["facilitator_credential_ref"], cred2.ID)
	}
}

func TestPatchSettlementConfig_NoAuth(t *testing.T) {
	t.Parallel()

	h, _ := newSettlementHandler(t)
	body := map[string]any{
		"facilitator_url":            "https://settle.example.com",
		"facilitator_credential_ref": "cred_x", //nolint:gosec // not a credential value, a placeholder ID
	}
	rec := serve(t, h, settlementReq(t, http.MethodPatch, body, "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestSettlementConfig_TenantIsolation(t *testing.T) {
	t.Parallel()

	h, credSvc := newSettlementHandler(t)
	cred := seedCredential(t, credSvc, "facilitator-auth", "facilitator-secret-value")

	rec := serve(t, h, settlementReq(t, http.MethodPatch, map[string]any{
		"facilitator_url":            "https://settle.example.com",
		"facilitator_credential_ref": cred.ID,
	}, testTenant, testUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// A different tenant must not see tenant-1's config.
	getRec := serve(t, h, settlementReq(t, http.MethodGet, nil, "tenant-2", testUser))
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant GET status = %d, want %d, body = %s", getRec.Code, http.StatusNotFound, getRec.Body.String())
	}
}
