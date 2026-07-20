package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"

	"github.com/zerkerlabs/farcaster/facilitator/internal/account"
	"github.com/zerkerlabs/farcaster/facilitator/internal/chain"
	"github.com/zerkerlabs/farcaster/facilitator/internal/guardrail"
	"github.com/zerkerlabs/farcaster/facilitator/internal/health"
	"github.com/zerkerlabs/farcaster/facilitator/internal/mtls/mtlstest"
	"github.com/zerkerlabs/farcaster/facilitator/internal/settle"
	"github.com/zerkerlabs/farcaster/facilitator/internal/settlement"
	"github.com/zerkerlabs/farcaster/facilitator/internal/sweep"
	"github.com/zerkerlabs/farcaster/x402types"
)

// stubChecker is a controllable health.Checker for exercising the /settle
// readiness gate and /healthz without a live chain RPC.
type stubChecker struct {
	err error
}

func (s stubChecker) Ready(context.Context) error { return s.err }

var testKinds = []health.Kind{{Scheme: "exact", Network: "base", Asset: "0xUSDC"}}

// okSubmitter is a minimal settle.Submitter reporting a fixed tx hash, so the
// routing tests exercise the mounted handler without a real chain.
type okSubmitter struct{}

func (okSubmitter) Submit(context.Context, x402types.Authorization, string) (ethcommon.Hash, error) {
	return ethcommon.HexToHash("0xabc"), nil
}

// testSettleHandler builds a real settle.Handler with a stubbed (all-pass)
// verifier and a fake submitter, so main's routing/gate/middleware wiring can
// be tested without a chain or KMS. The orchestration itself is covered in the
// settle package's own tests.
func testSettleHandler() http.Handler {
	return settle.NewHandler(settle.Config{
		Verify:     func(x402types.SettleRequest, time.Time) error { return nil },
		Submitters: map[string]settle.Submitter{"base": okSubmitter{}},
		Store:      settlement.NewMemoryStore(),
		GuardrailDefaults: guardrail.Limits{
			AllowedKinds: []guardrail.Kind{
				{Network: "base", Asset: "0x3333333333333333333333333333333333333333"},
			},
			MaxSettleAmount: big.NewInt(1_000_000),
			DailyCeiling:    big.NewInt(1_000_000_000),
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func newTestStoreAndCert(t *testing.T) (account.Store, *x509.Certificate) {
	t.Helper()

	ca := mtlstest.NewCA(t)
	cert := ca.IssueLeaf(t, "test-client", x509.ExtKeyUsageClientAuth)
	store := account.NewMemoryStore()
	if err := store.Create(context.Background(), &account.Account{
		Name:            "test-operator",
		CertFingerprint: account.Fingerprint(cert.Cert),
		Active:          true,
	}); err != nil {
		t.Fatalf("seed account store: %v", err)
	}
	return store, cert.Cert
}

func withPeerCert(req *http.Request, cert *x509.Certificate) *http.Request {
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	return req
}

func settleRequestBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(x402types.SettleRequest{
		X402Version: 1,
		PaymentPayload: x402types.Payment{
			Network: "base", Scheme: "exact",
			Payload: x402types.PaymentPayload{
				Authorization: x402types.Authorization{
					From:  "0x1111111111111111111111111111111111111111",
					To:    "0x2222222222222222222222222222222222222222",
					Value: "10000", ValidAfter: "0", ValidBefore: "9999999999",
					Nonce: "0xabc",
				},
			},
		},
		PaymentRequirements: x402types.PaymentRequirements{
			Network: "base", Asset: "0x3333333333333333333333333333333333333333",
			MaxAmountRequired: "10000", PayTo: "0x2222222222222222222222222222222222222222",
			Scheme: "exact",
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return body
}

func TestStaleAfterFromEnv_EmptyUsesDefault(t *testing.T) {
	t.Setenv("FACILITATOR_STALE_AFTER", "")
	d, err := staleAfterFromEnv()
	if err != nil {
		t.Fatalf("staleAfterFromEnv() error = %v", err)
	}
	if d != settle.DefaultStaleAfter {
		t.Fatalf("d = %v, want %v", d, settle.DefaultStaleAfter)
	}
}

func TestStaleAfterFromEnv_Valid(t *testing.T) {
	t.Setenv("FACILITATOR_STALE_AFTER", "90s")
	d, err := staleAfterFromEnv()
	if err != nil {
		t.Fatalf("staleAfterFromEnv() error = %v", err)
	}
	if d != 90*time.Second {
		t.Fatalf("d = %v, want 90s", d)
	}
}

func TestStaleAfterFromEnv_InvalidErrors(t *testing.T) {
	for _, v := range []string{"not-a-duration", "-1m", "0s"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("FACILITATOR_STALE_AFTER", v)
			if _, err := staleAfterFromEnv(); err == nil {
				t.Fatalf("staleAfterFromEnv() = nil error, want error for %q", v)
			}
		})
	}
}

func TestSweepIntervalFromEnv_EmptyUsesDefault(t *testing.T) {
	t.Setenv("FACILITATOR_SWEEP_INTERVAL", "")
	d, err := sweepIntervalFromEnv()
	if err != nil {
		t.Fatalf("sweepIntervalFromEnv() error = %v", err)
	}
	if d != sweep.DefaultInterval {
		t.Fatalf("d = %v, want %v", d, sweep.DefaultInterval)
	}
}

func TestSweepIntervalFromEnv_Valid(t *testing.T) {
	t.Setenv("FACILITATOR_SWEEP_INTERVAL", "15s")
	d, err := sweepIntervalFromEnv()
	if err != nil {
		t.Fatalf("sweepIntervalFromEnv() error = %v", err)
	}
	if d != 15*time.Second {
		t.Fatalf("d = %v, want 15s", d)
	}
}

func TestSweepIntervalFromEnv_InvalidErrors(t *testing.T) {
	for _, v := range []string{"not-a-duration", "-5s", "0s"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("FACILITATOR_SWEEP_INTERVAL", v)
			if _, err := sweepIntervalFromEnv(); err == nil {
				t.Fatalf("sweepIntervalFromEnv() = nil error, want error for %q", v)
			}
		})
	}
}

func TestReconcileLookbackBlocksFromEnv_EmptyUsesZero(t *testing.T) {
	t.Setenv("FACILITATOR_RECONCILE_LOOKBACK_BLOCKS", "")
	n, err := reconcileLookbackBlocksFromEnv()
	if err != nil {
		t.Fatalf("reconcileLookbackBlocksFromEnv() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0 (chain.Config fills its own default)", n)
	}
}

func TestReconcileLookbackBlocksFromEnv_Valid(t *testing.T) {
	t.Setenv("FACILITATOR_RECONCILE_LOOKBACK_BLOCKS", "50000")
	n, err := reconcileLookbackBlocksFromEnv()
	if err != nil {
		t.Fatalf("reconcileLookbackBlocksFromEnv() error = %v", err)
	}
	if n != 50000 {
		t.Fatalf("n = %d, want 50000", n)
	}
}

func TestReconcileLookbackBlocksFromEnv_InvalidErrors(t *testing.T) {
	for _, v := range []string{"not-a-number", "-1", "0"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("FACILITATOR_RECONCILE_LOOKBACK_BLOCKS", v)
			if _, err := reconcileLookbackBlocksFromEnv(); err == nil {
				t.Fatalf("reconcileLookbackBlocksFromEnv() = nil error, want error for %q", v)
			}
		})
	}
}

func TestReadinessFromEnv_MissingConfigErrors(t *testing.T) {
	for _, key := range []string{
		"FACILITATOR_RPC_URL",
		"FACILITATOR_NETWORK",
		"FACILITATOR_USDC_ADDRESS",
		"FACILITATOR_GAS_WALLET_ADDRESS",
		"FACILITATOR_GAS_LOW_WATER_MARK_WEI",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("FACILITATOR_NETWORK", "base")
	t.Setenv("FACILITATOR_USDC_ADDRESS", "0x036CbD53842c5426634e7929541eC2318f3dCF7e")
	t.Setenv("FACILITATOR_GAS_WALLET_ADDRESS", "0x036CbD53842c5426634e7929541eC2318f3dCF7e")
	t.Setenv("FACILITATOR_GAS_LOW_WATER_MARK_WEI", "1000000000000000")

	if _, _, err := readinessFromEnv(); err == nil {
		t.Fatalf("readinessFromEnv() = nil error, want error for missing FACILITATOR_RPC_URL")
	}
}

func TestReadinessFromEnv_InvalidAddress(t *testing.T) {
	t.Setenv("FACILITATOR_RPC_URL", "http://127.0.0.1:1")
	t.Setenv("FACILITATOR_NETWORK", "base")
	t.Setenv("FACILITATOR_USDC_ADDRESS", "not-an-address")
	t.Setenv("FACILITATOR_GAS_WALLET_ADDRESS", "0x036CbD53842c5426634e7929541eC2318f3dCF7e")
	t.Setenv("FACILITATOR_GAS_LOW_WATER_MARK_WEI", "1000000000000000")

	if _, _, err := readinessFromEnv(); err == nil {
		t.Fatalf("readinessFromEnv() = nil error, want error for invalid FACILITATOR_USDC_ADDRESS")
	}
}

func TestReadinessFromEnv_InvalidLowWaterMark(t *testing.T) {
	t.Setenv("FACILITATOR_RPC_URL", "http://127.0.0.1:1")
	t.Setenv("FACILITATOR_NETWORK", "base")
	t.Setenv("FACILITATOR_USDC_ADDRESS", "0x036CbD53842c5426634e7929541eC2318f3dCF7e")
	t.Setenv("FACILITATOR_GAS_WALLET_ADDRESS", "0x036CbD53842c5426634e7929541eC2318f3dCF7e")
	t.Setenv("FACILITATOR_GAS_LOW_WATER_MARK_WEI", "not-a-number")

	if _, _, err := readinessFromEnv(); err == nil {
		t.Fatalf("readinessFromEnv() = nil error, want error for invalid FACILITATOR_GAS_LOW_WATER_MARK_WEI")
	}
}

func TestReadinessFromEnv_Valid(t *testing.T) {
	t.Setenv("FACILITATOR_RPC_URL", "http://127.0.0.1:1")
	t.Setenv("FACILITATOR_NETWORK", "base")
	t.Setenv("FACILITATOR_USDC_ADDRESS", "0x036CbD53842c5426634e7929541eC2318f3dCF7e")
	t.Setenv("FACILITATOR_GAS_WALLET_ADDRESS", "0x036CbD53842c5426634e7929541eC2318f3dCF7e")
	t.Setenv("FACILITATOR_GAS_LOW_WATER_MARK_WEI", "1000000000000000")

	checker, kinds, err := readinessFromEnv()
	if err != nil {
		t.Fatalf("readinessFromEnv() error = %v", err)
	}
	if checker == nil {
		t.Fatalf("checker is nil")
	}
	want := []health.Kind{{Scheme: "exact", Network: "base", Asset: "0x036CbD53842c5426634e7929541eC2318f3dCF7e"}}
	if len(kinds) != 1 || kinds[0] != want[0] {
		t.Fatalf("kinds = %+v, want %+v", kinds, want)
	}
}

func TestGuardrailDefaultsFromEnv_MissingConfigErrors(t *testing.T) {
	t.Setenv("FACILITATOR_GUARDRAIL_MAX_SETTLE_AMOUNT", "")
	t.Setenv("FACILITATOR_GUARDRAIL_DAILY_CEILING", "1000000")

	if _, err := guardrailDefaultsFromEnv("base", "0xUSDC"); err == nil {
		t.Fatalf("guardrailDefaultsFromEnv() = nil error, want error for missing FACILITATOR_GUARDRAIL_MAX_SETTLE_AMOUNT")
	}
}

func TestGuardrailDefaultsFromEnv_InvalidAmount(t *testing.T) {
	t.Setenv("FACILITATOR_GUARDRAIL_MAX_SETTLE_AMOUNT", "not-a-number")
	t.Setenv("FACILITATOR_GUARDRAIL_DAILY_CEILING", "1000000")

	if _, err := guardrailDefaultsFromEnv("base", "0xUSDC"); err == nil {
		t.Fatalf("guardrailDefaultsFromEnv() = nil error, want error for a non-integer max settle amount")
	}
}

func TestGuardrailDefaultsFromEnv_MaxExceedsCeiling(t *testing.T) {
	t.Setenv("FACILITATOR_GUARDRAIL_MAX_SETTLE_AMOUNT", "2000000")
	t.Setenv("FACILITATOR_GUARDRAIL_DAILY_CEILING", "1000000")

	if _, err := guardrailDefaultsFromEnv("base", "0xUSDC"); err == nil {
		t.Fatalf("guardrailDefaultsFromEnv() = nil error, want error when the per-tx max exceeds the daily ceiling")
	}
}

func TestGuardrailDefaultsFromEnv_Valid(t *testing.T) {
	t.Setenv("FACILITATOR_GUARDRAIL_MAX_SETTLE_AMOUNT", "500000")
	t.Setenv("FACILITATOR_GUARDRAIL_DAILY_CEILING", "1000000")

	limits, err := guardrailDefaultsFromEnv("base", "0xUSDC")
	if err != nil {
		t.Fatalf("guardrailDefaultsFromEnv() error = %v", err)
	}
	if len(limits.AllowedKinds) != 1 || limits.AllowedKinds[0] != (guardrail.Kind{Network: "base", Asset: "0xUSDC"}) {
		t.Fatalf("AllowedKinds = %+v, want the configured (network, asset)", limits.AllowedKinds)
	}
	if limits.MaxSettleAmount.String() != "500000" || limits.DailyCeiling.String() != "1000000" {
		t.Fatalf("limits = %+v, want max=500000 ceiling=1000000", limits)
	}
}

// A valid, authenticated, ready request reaches the mounted settle handler and
// settles (no more 501).
func TestRoutes_SettleMounted(t *testing.T) {
	store, cert := newTestStoreAndCert(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/settle", bytes.NewReader(settleRequestBody(t))), cert)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	routes(store, testSettleHandler(), stubChecker{}, testKinds, logger).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp x402types.SettleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.Success || resp.Transaction == "" {
		t.Fatalf("expected a successful settle with a tx hash, got %+v", resp)
	}
}

func TestRoutes_InvalidBody(t *testing.T) {
	store, cert := newTestStoreAndCert(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/settle", bytes.NewReader([]byte("not json"))), cert)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	routes(store, testSettleHandler(), stubChecker{}, testKinds, logger).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestRoutes_UnknownAccountRejected(t *testing.T) {
	store, _ := newTestStoreAndCert(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	unknown := mtlstest.SelfSigned(t, "not-registered", x509.ExtKeyUsageClientAuth)

	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/settle", bytes.NewReader(settleRequestBody(t))), unknown.Cert)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	routes(store, testSettleHandler(), stubChecker{}, testKinds, logger).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestRoutes_NoClientCertRejected(t *testing.T) {
	store, _ := newTestStoreAndCert(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	req := httptest.NewRequest(http.MethodPost, "/settle", bytes.NewReader(settleRequestBody(t)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	routes(store, testSettleHandler(), stubChecker{}, testKinds, logger).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// The readiness gate rejects /settle with 503 when the facilitator is not ready
// (e.g. gas balance below the low-water mark) — before any settlement runs.
func TestRoutes_NotReadyRejectsSettle(t *testing.T) {
	store, cert := newTestStoreAndCert(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/settle", bytes.NewReader(settleRequestBody(t))), cert)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	routes(store, testSettleHandler(), stubChecker{err: chain.ErrGasBalanceLow}, testKinds, logger).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestSupported_ReturnsConfiguredKinds(t *testing.T) {
	store, _ := newTestStoreAndCert(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	req := httptest.NewRequest(http.MethodGet, "/supported", nil)
	rec := httptest.NewRecorder()

	routes(store, testSettleHandler(), stubChecker{}, testKinds, logger).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Kinds []health.Kind `json:"kinds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body.Kinds) != 1 || body.Kinds[0] != testKinds[0] {
		t.Fatalf("kinds = %+v, want %+v", body.Kinds, testKinds)
	}
}

func TestHealthz_ReadyAndNotReady(t *testing.T) {
	store, _ := newTestStoreAndCert(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tc := range []struct {
		name    string
		checker health.Checker
		want    int
	}{
		{"ready", stubChecker{}, http.StatusOK},
		{"rpc unreachable", stubChecker{err: chain.ErrRPCUnreachable}, http.StatusServiceUnavailable},
		{"gas balance low", stubChecker{err: chain.ErrGasBalanceLow}, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()

			routes(store, testSettleHandler(), tc.checker, testKinds, logger).ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
