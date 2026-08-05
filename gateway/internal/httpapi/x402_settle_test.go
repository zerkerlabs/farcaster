package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/credential"
	"github.com/zerkerlabs/gateway/gateway/internal/settlement"
	"github.com/zerkerlabs/gateway/x402types"
)

// stubFacilitatorCred is a minimal FacilitatorCredentialResolver for tests,
// mirroring proxy_test.go's stubCredSvc.
type stubFacilitatorCred struct {
	cred      *credential.Credential
	plaintext []byte
	getErr    error
	decErr    error
}

func (s *stubFacilitatorCred) Get(_ context.Context, _, _ string) (*credential.Credential, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.cred, nil
}

func (s *stubFacilitatorCred) Decrypt(_ context.Context, _, _ string) ([]byte, error) {
	if s.decErr != nil {
		return nil, s.decErr
	}
	return s.plaintext, nil
}

func TestFacilitatorSettler_Settle_Success(t *testing.T) {
	t.Parallel()

	var gotAuth, gotAPIKey string
	var gotBody x402types.SettleRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/settle" {
			t.Errorf("path = %q, want /settle", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(x402types.SettleResponse{
			Success:        true,
			Transaction:    "0xabc123",
			Network:        "base",
			Payer:          "0x2222222222222222222222222222222222222222",
			FacilitatorFee: "5",
		})
	}))
	defer srv.Close()

	settler := NewFacilitatorSettler(srv.Client())
	pmt := samplePayment()
	req := sampleReq()

	result, err := settler.Settle(context.Background(), srv.URL, credential.AuthTypeBearer, []byte("secret-token"), pmt, req)
	if err != nil {
		t.Fatalf("Settle: unexpected error: %v", err)
	}
	if result.TxHash != "0xabc123" {
		t.Errorf("TxHash = %q, want 0xabc123", result.TxHash)
	}
	if result.Network != "base" {
		t.Errorf("Network = %q, want base", result.Network)
	}
	if result.Payer != pmt.Payload.Authorization.From {
		t.Errorf("Payer = %q, want %q", result.Payer, pmt.Payload.Authorization.From)
	}
	if result.FacilitatorFee != "5" {
		t.Errorf("FacilitatorFee = %q, want 5", result.FacilitatorFee)
	}

	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret-token")
	}
	if gotAPIKey != "" {
		t.Errorf("X-Api-Key header = %q, want empty", gotAPIKey)
	}
	if gotBody.X402Version != x402types.Version {
		t.Errorf("request x402Version = %d, want %d", gotBody.X402Version, x402types.Version)
	}
	if gotBody.PaymentPayload.Payload.Authorization.Nonce != pmt.Payload.Authorization.Nonce {
		t.Errorf("request paymentPayload not forwarded correctly")
	}
	if gotBody.PaymentRequirements.PayTo != req.PayTo {
		t.Errorf("request paymentRequirements not forwarded correctly")
	}
}

func TestFacilitatorSettler_Settle_APIKeyAuth(t *testing.T) {
	t.Parallel()

	var gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		_ = json.NewEncoder(w).Encode(x402types.SettleResponse{Success: true, Transaction: "0x1"})
	}))
	defer srv.Close()

	apiKeyValue := "sk-test-api-key" //nolint:gosec // test credential, not a real secret

	settler := NewFacilitatorSettler(srv.Client())
	_, err := settler.Settle(context.Background(), srv.URL, credential.AuthTypeAPIKey, []byte(apiKeyValue), samplePayment(), sampleReq())
	if err != nil {
		t.Fatalf("Settle: unexpected error: %v", err)
	}
	if gotAPIKey != apiKeyValue {
		t.Errorf("X-Api-Key header = %q, want %q", gotAPIKey, apiKeyValue)
	}
}

func TestFacilitatorSettler_Settle_NoCredential(t *testing.T) {
	t.Parallel()

	var gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		_ = json.NewEncoder(w).Encode(x402types.SettleResponse{Success: true, Transaction: "0x1"})
	}))
	defer srv.Close()

	settler := NewFacilitatorSettler(srv.Client())
	_, err := settler.Settle(context.Background(), srv.URL, credential.AuthTypeNone, nil, samplePayment(), sampleReq())
	if err != nil {
		t.Fatalf("Settle: unexpected error: %v", err)
	}
	if gotAuth != "" || gotAPIKey != "" {
		t.Errorf("expected no auth headers, got Authorization=%q X-Api-Key=%q", gotAuth, gotAPIKey)
	}
}

func TestFacilitatorSettler_Settle_Rejected(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(x402types.SettleResponse{
			Success:     false,
			ErrorReason: "insufficient_funds",
		})
	}))
	defer srv.Close()

	settler := NewFacilitatorSettler(srv.Client())
	_, err := settler.Settle(context.Background(), srv.URL, credential.AuthTypeNone, nil, samplePayment(), sampleReq())
	if err == nil {
		t.Fatal("Settle: expected error, got nil")
	}
	if !errors.Is(err, ErrSettleRejected) {
		t.Errorf("errors.Is(err, ErrSettleRejected) = false, err: %v", err)
	}
	var settleErr *SettleError
	if !errors.As(err, &settleErr) {
		t.Fatalf("errors.As(err, *SettleError) failed, err: %v", err)
	}
	if settleErr.Reason != "insufficient_funds" {
		t.Errorf("Reason = %q, want insufficient_funds", settleErr.Reason)
	}
}

func TestFacilitatorSettler_Settle_BadStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	settler := NewFacilitatorSettler(srv.Client())
	_, err := settler.Settle(context.Background(), srv.URL, credential.AuthTypeNone, nil, samplePayment(), sampleReq())
	if !errors.Is(err, ErrSettleBadResponse) {
		t.Errorf("errors.Is(err, ErrSettleBadResponse) = false, err: %v", err)
	}
}

func TestFacilitatorSettler_Settle_MalformedBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	settler := NewFacilitatorSettler(srv.Client())
	_, err := settler.Settle(context.Background(), srv.URL, credential.AuthTypeNone, nil, samplePayment(), sampleReq())
	if !errors.Is(err, ErrSettleBadResponse) {
		t.Errorf("errors.Is(err, ErrSettleBadResponse) = false, err: %v", err)
	}
}

func TestFacilitatorSettler_Settle_Unreachable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // closed immediately: connection refused

	settler := NewFacilitatorSettler(srv.Client())
	_, err := settler.Settle(context.Background(), url, credential.AuthTypeNone, nil, samplePayment(), sampleReq())
	if !errors.Is(err, ErrSettleUnreachable) {
		t.Errorf("errors.Is(err, ErrSettleUnreachable) = false, err: %v", err)
	}
}

// TestFacilitatorSettler_Settle_SSRFGuard exercises the production wiring
// (NewFacilitatorSettler(nil)): a facilitator URL that resolves to loopback
// must be blocked, exactly as upstream_url is at proxy forward time (spec
// 0006: "Egress passes through the SSRF guard"). Tests that want to hit a
// local httptest.Server instead inject its client (see the tests above),
// which bypasses this guard the same way proxy.Config.HTTPClient does.
func TestFacilitatorSettler_Settle_SSRFGuard(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(x402types.SettleResponse{Success: true, Transaction: "0x1"})
	}))
	defer srv.Close()

	settler := NewFacilitatorSettler(nil)
	_, err := settler.Settle(context.Background(), srv.URL, credential.AuthTypeNone, nil, samplePayment(), sampleReq())
	if !errors.Is(err, ErrSettleUnreachable) {
		t.Errorf("errors.Is(err, ErrSettleUnreachable) = false, err: %v", err)
	}
}

func TestResolveFacilitator(t *testing.T) {
	t.Parallel()

	const tenant = "tnt_resolve_facilitator"
	store := settlement.NewMemoryStore()
	url := "https://facilitator.example.com"
	ref := "cred_facilitator"
	if _, err := store.Upsert(context.Background(), tenant, settlement.UpdateFields{
		FacilitatorURL:           &url,
		FacilitatorCredentialRef: &ref,
	}); err != nil {
		t.Fatalf("seed settlement config: %v", err)
	}

	credSvc := &stubFacilitatorCred{
		cred:      &credential.Credential{ID: ref, AuthType: credential.AuthTypeBearer},
		plaintext: []byte("resolved-secret"),
	}

	gotURL, gotAuthType, gotPlaintext, err := resolveFacilitator(context.Background(), store, credSvc, tenant)
	if err != nil {
		t.Fatalf("resolveFacilitator: unexpected error: %v", err)
	}
	if gotURL != url {
		t.Errorf("facilitatorURL = %q, want %q", gotURL, url)
	}
	if gotAuthType != credential.AuthTypeBearer {
		t.Errorf("authType = %q, want bearer", gotAuthType)
	}
	if string(gotPlaintext) != "resolved-secret" {
		t.Errorf("credPlaintext = %q, want resolved-secret", gotPlaintext)
	}
}

func TestResolveFacilitator_NotConfigured(t *testing.T) {
	t.Parallel()

	store := settlement.NewMemoryStore()
	credSvc := &stubFacilitatorCred{}

	_, _, _, err := resolveFacilitator(context.Background(), store, credSvc, "tnt_unconfigured")
	if !errors.Is(err, settlement.ErrNotFound) {
		t.Errorf("errors.Is(err, settlement.ErrNotFound) = false, err: %v", err)
	}
}

func TestResolveFacilitator_AuthTypeNoneSkipsDecrypt(t *testing.T) {
	t.Parallel()

	const tenant = "tnt_resolve_facilitator_none"
	store := settlement.NewMemoryStore()
	url := "https://facilitator.example.com"
	ref := "cred_facilitator_none"
	if _, err := store.Upsert(context.Background(), tenant, settlement.UpdateFields{
		FacilitatorURL:           &url,
		FacilitatorCredentialRef: &ref,
	}); err != nil {
		t.Fatalf("seed settlement config: %v", err)
	}

	credSvc := &stubFacilitatorCred{
		cred:   &credential.Credential{ID: ref, AuthType: credential.AuthTypeNone},
		decErr: errors.New("Decrypt must not be called for AuthTypeNone"),
	}

	_, gotAuthType, gotPlaintext, err := resolveFacilitator(context.Background(), store, credSvc, tenant)
	if err != nil {
		t.Fatalf("resolveFacilitator: unexpected error: %v", err)
	}
	if gotAuthType != credential.AuthTypeNone {
		t.Errorf("authType = %q, want none", gotAuthType)
	}
	if gotPlaintext != nil {
		t.Errorf("credPlaintext = %q, want nil", gotPlaintext)
	}
}
