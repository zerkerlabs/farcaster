package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/farcaster/facilitator/internal/chain"
)

type stubChecker struct {
	err error
}

func (s stubChecker) Ready(context.Context) error { return s.err }

func TestSupported_ReturnsConfiguredKinds(t *testing.T) {
	kinds := []Kind{{Scheme: "exact", Network: "base", Asset: "0xUSDC"}}

	req := httptest.NewRequest(http.MethodGet, "/supported", nil)
	rec := httptest.NewRecorder()
	Supported(kinds).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Kinds []Kind `json:"kinds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body.Kinds) != 1 || body.Kinds[0] != kinds[0] {
		t.Fatalf("kinds = %+v, want %+v", body.Kinds, kinds)
	}
}

func TestHealthz_Ready(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	Healthz(stubChecker{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status field = %q, want ok", body["status"])
	}
}

func TestHealthz_RPCUnreachable(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	Healthz(stubChecker{err: chain.ErrRPCUnreachable}).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["status"] != "unavailable" || body["reason"] != "rpc_unreachable" {
		t.Fatalf("body = %+v, want unavailable/rpc_unreachable", body)
	}
}

func TestHealthz_GasBalanceLow(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	Healthz(stubChecker{err: chain.ErrGasBalanceLow}).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["reason"] != "gas_balance_low" {
		t.Fatalf("reason = %q, want gas_balance_low", body["reason"])
	}
}

func TestHealthz_ResponseHasNoConfigValues(t *testing.T) {
	// AGENTS.md invariant #9: /healthz surfaces subsystem health only, never
	// internal addresses or config values. A generic "not ready" reason
	// (rather than the underlying error's text) is how this handler upholds
	// that even as new readiness checks are added.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	Healthz(stubChecker{err: errors.New("dial tcp 10.0.0.5:8545: connection refused")}).ServeHTTP(rec, req)

	if got := rec.Body.String(); got == "" {
		t.Fatalf("empty body")
	} else if containsAny(got, "10.0.0.5", "8545") {
		t.Fatalf("response leaked internal detail: %s", got)
	}
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(sub) > 0 && (len(s) >= len(sub)) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

func TestGateSettle_BlocksWhenNotReady(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodPost, "/settle", nil)
	rec := httptest.NewRecorder()
	GateSettle(stubChecker{err: chain.ErrGasBalanceLow}, logger, next).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if called {
		t.Fatalf("next handler was called despite not-ready checker")
	}
}

func TestGateSettle_PassesThroughWhenReady(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})

	req := httptest.NewRequest(http.MethodPost, "/settle", nil)
	rec := httptest.NewRecorder()
	GateSettle(stubChecker{}, logger, next).ServeHTTP(rec, req)

	if !called {
		t.Fatalf("next handler was not called despite ready checker")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d (from next handler)", rec.Code, http.StatusTeapot)
	}
}
