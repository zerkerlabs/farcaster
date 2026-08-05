package settle_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/zerkerlabs/gateway/facilitator/internal/account"
	"github.com/zerkerlabs/gateway/facilitator/internal/chain"
	"github.com/zerkerlabs/gateway/facilitator/internal/guardrail"
	"github.com/zerkerlabs/gateway/facilitator/internal/mtls/mtlstest"
	"github.com/zerkerlabs/gateway/facilitator/internal/settle"
	"github.com/zerkerlabs/gateway/facilitator/internal/settlement"
	"github.com/zerkerlabs/gateway/facilitator/internal/verify"
	"github.com/zerkerlabs/gateway/x402types"
)

const (
	addrPayer = "0x1111111111111111111111111111111111111111"
	addrPayTo = "0x2222222222222222222222222222222222222222"
	addrUSDC  = "0x3333333333333333333333333333333333333333"
	txHashHex = "0x00000000000000000000000000000000000000000000000000000000000000ab"
)

type fakeSubmitter struct {
	tx    common.Hash
	err   error
	calls int32
}

func (f *fakeSubmitter) Submit(context.Context, x402types.Authorization, string) (common.Hash, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.tx, f.err
}

// fakeReconciler is a chain-mocked settle.Reconciler: no RPC, just the outcome
// a test wants Reconcile to report.
type fakeReconciler struct {
	tx    common.Hash
	err   error
	calls int32
}

func (f *fakeReconciler) Reconcile(context.Context, x402types.Authorization, time.Time) (common.Hash, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.tx, f.err
}

// markSettledFailOnceStore fails exactly the first MarkSettled call — modeling
// spec 0007 follow-up #179's "post-submit record failure": Submit lands the
// tx on-chain, but the write that should record it transiently fails, leaving
// the row stuck in_flight even though the money already moved. Every other
// call behaves like the embedded MemoryStore.
type markSettledFailOnceStore struct {
	*settlement.MemoryStore
	failed atomic.Bool
}

func (s *markSettledFailOnceStore) MarkSettled(ctx context.Context, id, txHash string) (*settlement.Settlement, error) {
	if s.failed.CompareAndSwap(false, true) {
		return nil, errors.New("db unavailable (transient)")
	}
	return s.MemoryStore.MarkSettled(ctx, id, txHash)
}

// claimErrStore fails every Claim; embeds a MemoryStore for the other methods.
type claimErrStore struct{ *settlement.MemoryStore }

func (claimErrStore) Claim(context.Context, settlement.ClaimKey, settlement.DailyCeiling) (*settlement.Settlement, bool, error) {
	return nil, false, errors.New("db unavailable")
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// harness wires the settle handler behind the real account middleware (so the
// account is injected exactly as in production) with an all-pass stubbed
// verifier by default, and returns the pieces a test asserts on.
type harness struct {
	handler http.Handler
	sub     *fakeSubmitter
	store   settlement.Store
	cert    *x509.Certificate
}

// generousGuardrailDefaults gives every standard settleBody (value "10000" on
// base/addrUSDC) ample headroom, so tests unrelated to guardrails don't need
// to think about them. Guardrail-specific tests override GuardrailDefaults
// (or an account's own Guardrails) to exercise at-limit/over-limit/override
// behavior.
func generousGuardrailDefaults() guardrail.Limits {
	return guardrail.Limits{
		AllowedKinds:    []guardrail.Kind{{Network: "base", Asset: addrUSDC}},
		MaxSettleAmount: big.NewInt(1_000_000),
		DailyCeiling:    big.NewInt(1_000_000_000),
	}
}

func newHarness(t *testing.T, mut func(*settle.Config)) harness {
	t.Helper()
	return newHarnessWithAccount(t, mut, nil)
}

// newHarnessWithAccount is newHarness plus an acctMut hook to configure the
// seeded facilitator account itself (e.g. a per-account guardrail override)
// before it is created.
func newHarnessWithAccount(t *testing.T, cfgMut func(*settle.Config), acctMut func(*account.Account)) harness {
	t.Helper()
	sub := &fakeSubmitter{tx: common.HexToHash(txHashHex)}
	store := settlement.NewMemoryStore()
	cfg := settle.Config{
		Verify:            func(x402types.SettleRequest, time.Time) error { return nil },
		Submitters:        map[string]settle.Submitter{"base": sub},
		Store:             store,
		GuardrailDefaults: generousGuardrailDefaults(),
		Logger:            discardLogger(),
		Now:               func() time.Time { return time.Unix(1_000, 0).UTC() },
	}
	if cfgMut != nil {
		cfgMut(&cfg)
	}
	h := settle.NewHandler(cfg)

	acctStore := account.NewMemoryStore()
	ca := mtlstest.NewCA(t)
	leaf := ca.IssueLeaf(t, "client", x509.ExtKeyUsageClientAuth)
	acct := &account.Account{
		Name: "operator", CertFingerprint: account.Fingerprint(leaf.Cert), Active: true,
	}
	if acctMut != nil {
		acctMut(acct)
	}
	if err := acctStore.Create(context.Background(), acct); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	wrapped := account.Middleware(acctStore, discardLogger())(h)
	return harness{handler: wrapped, sub: sub, store: store, cert: leaf.Cert}
}

func settleBody(nonce string) []byte {
	return settleBodyFrom(nonce, addrPayer)
}

func settleBodyFrom(nonce, payer string) []byte {
	b, _ := json.Marshal(x402types.SettleRequest{
		X402Version: 1,
		PaymentPayload: x402types.Payment{
			Network: "base", Scheme: "exact",
			Payload: x402types.PaymentPayload{
				Signature: "0x" + strings.Repeat("11", 65),
				Authorization: x402types.Authorization{
					From: payer, To: addrPayTo, Value: "10000",
					ValidAfter: "0", ValidBefore: "9999999999", Nonce: nonce,
				},
			},
		},
		PaymentRequirements: x402types.PaymentRequirements{
			Network: "base", Asset: addrUSDC, MaxAmountRequired: "10000",
			PayTo: addrPayTo, Scheme: "exact",
		},
	})
	return b
}

func (h harness) post(nonce string) *httptest.ResponseRecorder {
	return h.postBody(settleBody(nonce))
}

func (h harness) postBody(body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/settle", bytes.NewReader(body))
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{h.cert}}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func decodeSettle(t *testing.T, rec *httptest.ResponseRecorder) x402types.SettleResponse {
	t.Helper()
	var resp x402types.SettleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode SettleResponse: %v (body %q)", err, rec.Body.String())
	}
	return resp
}

func TestSettle_HappyPath(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.post("0xaa")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeSettle(t, rec)
	if !resp.Success {
		t.Fatalf("success = false, want true (%+v)", resp)
	}
	if resp.Transaction != txHashHex {
		t.Fatalf("transaction = %q, want %q", resp.Transaction, txHashHex)
	}
	if resp.Network != "base" || resp.Payer != addrPayer || resp.FacilitatorFee != "0" {
		t.Fatalf("unexpected response fields: %+v", resp)
	}
	if h.sub.calls != 1 {
		t.Fatalf("submitter calls = %d, want 1", h.sub.calls)
	}
}

func TestSettle_ReverifyRejectionIsOutcomeNoSubmit(t *testing.T) {
	h := newHarness(t, func(c *settle.Config) {
		// A specific internal rejection must NOT leak: the body carries the
		// coarse reason, never the verify error text.
		c.Verify = func(x402types.SettleRequest, time.Time) error { return verify.ErrExpired }
	})
	rec := h.post("0xbb")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeSettle(t, rec)
	if resp.Success {
		t.Fatal("success = true, want false on re-verify rejection")
	}
	if resp.ErrorReason != "payment could not be collected" {
		t.Fatalf("errorReason = %q, want coarse reason", resp.ErrorReason)
	}
	if strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("response leaked the internal rejection reason: %q", rec.Body.String())
	}
	if h.sub.calls != 0 {
		t.Fatalf("submitter calls = %d, want 0 (no submission on rejection)", h.sub.calls)
	}
}

// The default (non-stubbed) verifier must actually run verify.Reverify — a
// malformed/invalid payment is rejected as an outcome, with no submission.
func TestSettle_DefaultVerifierIsWired(t *testing.T) {
	h := newHarness(t, func(c *settle.Config) {
		c.Verify = nil // use the real verify.Reverify
		c.Policy = verify.Policy{Kinds: []verify.Kind{{
			Network: "base", Asset: addrUSDC, ChainID: 8453,
			AssetName: "USD Coin", AssetVersion: "2",
		}}}
	})
	rec := h.post("0xcc") // signature is 0x11..., cannot recover to the payer

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if resp := decodeSettle(t, rec); resp.Success {
		t.Fatal("success = true, want false (real verify must reject a bogus signature)")
	}
	if h.sub.calls != 0 {
		t.Fatalf("submitter calls = %d, want 0", h.sub.calls)
	}
}

func TestSettle_CollectionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"revert", chain.ErrReverted},
		{"inclusion timeout", chain.ErrInclusionTimeout},
		{"would-revert gas estimate", chain.ErrGasEstimation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := &fakeSubmitter{err: tc.err}
			h := newHarness(t, func(c *settle.Config) {
				c.Submitters = map[string]settle.Submitter{"base": sub}
			})

			rec := h.post("0xdd")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (collection outcome)", rec.Code)
			}
			if resp := decodeSettle(t, rec); resp.Success {
				t.Fatalf("success = true, want false for %s", tc.name)
			}
			if sub.calls != 1 {
				t.Fatalf("submitter calls = %d, want 1", sub.calls)
			}
		})
	}
}

func TestSettle_TransientSubmitIsProtocol503(t *testing.T) {
	sub := &fakeSubmitter{err: chain.ErrNoBaseFee}
	h := newHarness(t, func(c *settle.Config) {
		c.Submitters = map[string]settle.Submitter{"base": sub}
	})
	rec := h.post("0xee")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (transient)", rec.Code)
	}
}

func TestSettle_IdempotentSettledReplaysSameTx(t *testing.T) {
	h := newHarness(t, nil)
	first := decodeSettle(t, h.post("0xf1"))
	second := decodeSettle(t, h.post("0xf1"))

	if !first.Success || !second.Success {
		t.Fatalf("both must succeed: %+v %+v", first, second)
	}
	if second.Transaction != first.Transaction {
		t.Fatalf("replay tx = %q, want same as first %q", second.Transaction, first.Transaction)
	}
	if h.sub.calls != 1 {
		t.Fatalf("submitter calls = %d, want 1 (no re-broadcast on duplicate nonce)", h.sub.calls)
	}
}

// A nonce collision alone is not proof of a legitimate replay: Store.Claim
// dedupes only on (AccountID, Nonce), so a distinct, independently-signed
// authorization that happens to reuse an already-settled nonce (nonces are
// attacker-choosable and observable on-chain) must never be told it succeeded
// with someone else's transaction and payer.
func TestSettle_MismatchedNonceReplayIsNotEchoedAsSuccess(t *testing.T) {
	h := newHarness(t, nil)
	first := decodeSettle(t, h.post("0xf4"))
	if !first.Success {
		t.Fatalf("first settle must succeed: %+v", first)
	}

	attacker := "0x9999999999999999999999999999999999999999"
	rec := h.postBody(settleBodyFrom("0xf4", attacker))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (collection outcome)", rec.Code)
	}
	resp := decodeSettle(t, rec)
	if resp.Success {
		t.Fatalf("mismatched-payer nonce replay must not report success: %+v", resp)
	}
	if resp.Transaction != "" || resp.Payer != "" {
		t.Fatalf("mismatched replay must not echo the unrelated tx/payer: %+v", resp)
	}
	if h.sub.calls != 1 {
		t.Fatalf("submitter calls = %d, want 1 (no re-broadcast on duplicate nonce)", h.sub.calls)
	}
}

func TestSettle_IdempotentFailedReplays(t *testing.T) {
	sub := &fakeSubmitter{err: chain.ErrReverted}
	h := newHarness(t, func(c *settle.Config) {
		c.Submitters = map[string]settle.Submitter{"base": sub}
	})
	first := h.post("0xf2")
	second := h.post("0xf2")

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("codes = %d,%d want 200,200", first.Code, second.Code)
	}
	if decodeSettle(t, second).Success {
		t.Fatal("replayed failed settlement must report success=false")
	}
	if sub.calls != 1 {
		t.Fatalf("submitter calls = %d, want 1 (failed nonce not re-submitted)", sub.calls)
	}
}

func TestSettle_InFlightDuplicateIsSingleFlighted(t *testing.T) {
	// A transient error leaves the row in-flight; a duplicate then dedupes.
	sub := &fakeSubmitter{err: chain.ErrNoBaseFee}
	h := newHarness(t, func(c *settle.Config) {
		c.Submitters = map[string]settle.Submitter{"base": sub}
	})
	first := h.post("0xf3")  // claims, submit transient -> row left in_flight, 503
	second := h.post("0xf3") // dedupe on in-flight row

	if first.Code != http.StatusServiceUnavailable || second.Code != http.StatusServiceUnavailable {
		t.Fatalf("codes = %d,%d want 503,503", first.Code, second.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(second.Body.Bytes(), &body)
	if body["error"] != "settlement in progress" {
		t.Fatalf("second error = %q, want \"settlement in progress\"", body["error"])
	}
	if sub.calls != 1 {
		t.Fatalf("submitter calls = %d, want 1 (in-flight duplicate not submitted)", sub.calls)
	}
}

func TestSettle_ProtocolFailures(t *testing.T) {
	t.Run("malformed body -> 400", func(t *testing.T) {
		h := newHarness(t, nil)
		if rec := h.postBody([]byte("not json")); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("no client cert -> 403 (middleware)", func(t *testing.T) {
		h := newHarness(t, nil)
		req := httptest.NewRequest(http.MethodPost, "/settle", bytes.NewReader(settleBody("0x01")))
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("no account in context -> 403 (handler defensive)", func(t *testing.T) {
		// Call the bare handler (unwrapped) so no account is in context.
		h := settle.NewHandler(settle.Config{
			Verify:     func(x402types.SettleRequest, time.Time) error { return nil },
			Submitters: map[string]settle.Submitter{"base": &fakeSubmitter{}},
			Store:      settlement.NewMemoryStore(),
			Logger:     discardLogger(),
		})
		req := httptest.NewRequest(http.MethodPost, "/settle", bytes.NewReader(settleBody("0x02")))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("store claim error -> 503", func(t *testing.T) {
		h := newHarness(t, func(c *settle.Config) {
			c.Store = claimErrStore{settlement.NewMemoryStore()}
		})
		if rec := h.post("0x03"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
	})

	t.Run("no submitter for network -> 503", func(t *testing.T) {
		h := newHarness(t, func(c *settle.Config) {
			c.Submitters = map[string]settle.Submitter{} // none for "base"
		})
		if rec := h.post("0x04"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
	})
}

// TestSettle_Guardrails covers spec 0007 T6's acceptance: at-limit requests
// succeed, over-limit requests (per-tx max, disallowed network/asset, daily
// ceiling) are rejected without submission, and a per-account override
// replaces the service default.
func TestSettle_Guardrails(t *testing.T) {
	t.Run("at the per-tx max succeeds", func(t *testing.T) {
		h := newHarness(t, func(c *settle.Config) {
			c.GuardrailDefaults.MaxSettleAmount = big.NewInt(10_000) // == the request's value, exactly
		})
		rec := h.post("0xg01")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 at exactly the per-tx max", rec.Code)
		}
		if resp := decodeSettle(t, rec); !resp.Success {
			t.Fatalf("success = false, want true: %+v", resp)
		}
		if h.sub.calls != 1 {
			t.Fatalf("submitter calls = %d, want 1", h.sub.calls)
		}
	})

	t.Run("over the per-tx max is rejected without submission", func(t *testing.T) {
		h := newHarness(t, func(c *settle.Config) {
			c.GuardrailDefaults.MaxSettleAmount = big.NewInt(9_999) // one below the request's 10000
		})
		rec := h.post("0xg02")
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", rec.Code)
		}
		if resp := decodeSettle(t, rec); resp.Success {
			t.Fatalf("success = true, want false over the per-tx max: %+v", resp)
		}
		if h.sub.calls != 0 {
			t.Fatalf("submitter calls = %d, want 0 (no submission over the per-tx max)", h.sub.calls)
		}
	})

	t.Run("disallowed network/asset is rejected without submission", func(t *testing.T) {
		h := newHarness(t, func(c *settle.Config) {
			c.GuardrailDefaults.AllowedKinds = []guardrail.Kind{{Network: "base-sepolia", Asset: addrUSDC}}
		})
		rec := h.post("0xg03")
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", rec.Code)
		}
		if h.sub.calls != 0 {
			t.Fatalf("submitter calls = %d, want 0 (no submission for a disallowed kind)", h.sub.calls)
		}
	})

	t.Run("at the daily ceiling succeeds, the next settle over it is rejected", func(t *testing.T) {
		h := newHarness(t, func(c *settle.Config) {
			c.GuardrailDefaults.DailyCeiling = big.NewInt(20_000) // exactly two settles of 10000
		})

		first := h.post("0xg04")
		if first.Code != http.StatusOK || !decodeSettle(t, first).Success {
			t.Fatalf("first settle should succeed: status=%d body=%s", first.Code, first.Body.String())
		}
		second := h.post("0xg05")
		if second.Code != http.StatusOK || !decodeSettle(t, second).Success {
			t.Fatalf("second settle should succeed (exactly at the daily ceiling): status=%d body=%s", second.Code, second.Body.String())
		}
		third := h.post("0xg06")
		if third.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 (over the daily ceiling)", third.Code)
		}
		if h.sub.calls != 2 {
			t.Fatalf("submitter calls = %d, want 2 (the third settle must not submit)", h.sub.calls)
		}
	})

	t.Run("a per-account override replaces the service default", func(t *testing.T) {
		h := newHarnessWithAccount(
			t,
			func(c *settle.Config) {
				// The service default alone would reject the request's 10000.
				c.GuardrailDefaults.MaxSettleAmount = big.NewInt(1)
			},
			func(a *account.Account) {
				a.Guardrails = &guardrail.Limits{
					AllowedKinds:    []guardrail.Kind{{Network: "base", Asset: addrUSDC}},
					MaxSettleAmount: big.NewInt(1_000_000),
					DailyCeiling:    big.NewInt(1_000_000_000),
				}
			},
		)
		rec := h.post("0xg07")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (the account's override should allow this)", rec.Code)
		}
		if resp := decodeSettle(t, rec); !resp.Success {
			t.Fatalf("success = false, want true under the override: %+v", resp)
		}
	})

	t.Run("an account override can also be stricter than the service default", func(t *testing.T) {
		h := newHarnessWithAccount(t, nil, func(a *account.Account) {
			a.Guardrails = &guardrail.Limits{
				AllowedKinds:    []guardrail.Kind{{Network: "base", Asset: addrUSDC}},
				MaxSettleAmount: big.NewInt(1), // stricter than the generous service default
				DailyCeiling:    big.NewInt(1_000_000_000),
			}
		})
		rec := h.post("0xg08")
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 (the account's own override is stricter)", rec.Code)
		}
		if h.sub.calls != 0 {
			t.Fatalf("submitter calls = %d, want 0", h.sub.calls)
		}
	})

	t.Run("missing guardrail limits fail closed as a protocol failure, not a panic", func(t *testing.T) {
		h := newHarness(t, func(c *settle.Config) {
			c.GuardrailDefaults = guardrail.Limits{} // no MaxSettleAmount/DailyCeiling configured
		})
		rec := h.post("0xg09")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (misconfigured guardrails)", rec.Code)
		}
		if h.sub.calls != 0 {
			t.Fatalf("submitter calls = %d, want 0", h.sub.calls)
		}
	})

	t.Run("an override allow-listing more than one asset fails closed as a protocol failure", func(t *testing.T) {
		// A mixed-asset AllowedKinds would let the daily ceiling sum amounts
		// across incompatible decimal precisions (guardrail.ErrMixedAssetKinds),
		// so resolveGuardrails must reject it the same as a missing
		// MaxSettleAmount/DailyCeiling rather than settle under it.
		h := newHarnessWithAccount(t, nil, func(a *account.Account) {
			a.Guardrails = &guardrail.Limits{
				AllowedKinds: []guardrail.Kind{
					{Network: "base", Asset: addrUSDC},
					{Network: "base", Asset: "0xDifferentAsset"},
				},
				MaxSettleAmount: big.NewInt(1_000_000),
				DailyCeiling:    big.NewInt(1_000_000_000),
			}
		})
		rec := h.post("0xg10")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (mixed-asset AllowedKinds)", rec.Code)
		}
		if h.sub.calls != 0 {
			t.Fatalf("submitter calls = %d, want 0", h.sub.calls)
		}
	})
}

// TestSettle_ReconcileStaleInFlight covers spec 0007 follow-up #179's
// acceptance: a duplicate request landing on an in_flight row older than
// StaleAfter re-checks the chain (mocked) for the authorization's true
// outcome and transitions the row to the correct terminal state, rather than
// leaving it stuck behind a transient response forever.
func TestSettle_ReconcileStaleInFlight(t *testing.T) {
	t.Run("settled-after-timeout reconciles to settled and replays the real tx hash", func(t *testing.T) {
		// MemoryStore.Claim stamps CreatedAt from the real wall clock (Store's
		// timestamps are not injectable), so the fake clock used for staleness
		// starts from real "now" too, rather than an arbitrary fixed epoch.
		clock := time.Now().UTC()
		sub := &fakeSubmitter{err: chain.ErrNoBaseFee} // transient: leaves the row in_flight
		fc := &fakeReconciler{tx: common.HexToHash(txHashHex)}
		h := newHarness(t, func(c *settle.Config) {
			c.Submitters = map[string]settle.Submitter{"base": sub}
			c.Reconcilers = map[string]settle.Reconciler{"base": fc}
			c.StaleAfter = time.Minute
			c.Now = func() time.Time { return clock }
		})

		first := h.post("0xrc1")
		if first.Code != http.StatusServiceUnavailable {
			t.Fatalf("first status = %d, want 503 (transient submit, row left in_flight)", first.Code)
		}

		clock = clock.Add(2 * time.Minute) // past StaleAfter

		second := h.post("0xrc1")
		if second.Code != http.StatusOK {
			t.Fatalf("second status = %d, want 200 (reconciled to settled)", second.Code)
		}
		resp := decodeSettle(t, second)
		if !resp.Success || resp.Transaction != txHashHex {
			t.Fatalf("resp = %+v, want success with tx %q", resp, txHashHex)
		}
		if sub.calls != 1 {
			t.Fatalf("submitter calls = %d, want 1 (reconciliation must never re-broadcast)", sub.calls)
		}
		if fc.calls != 1 {
			t.Fatalf("reconciler calls = %d, want 1", fc.calls)
		}

		// The row is now terminal: a further duplicate replays it directly,
		// without another chain reconcile.
		third := h.post("0xrc1")
		if resp := decodeSettle(t, third); !resp.Success || resp.Transaction != txHashHex {
			t.Fatalf("third resp = %+v, want the same settled tx replayed", resp)
		}
		if fc.calls != 1 {
			t.Fatalf("reconciler calls after settling = %d, want 1 (no reconcile on an already-terminal row)", fc.calls)
		}
	})

	t.Run("never-landed reconciles to failed", func(t *testing.T) {
		// MemoryStore.Claim stamps CreatedAt from the real wall clock (Store's
		// timestamps are not injectable), so the fake clock used for staleness
		// starts from real "now" too, rather than an arbitrary fixed epoch.
		clock := time.Now().UTC()
		sub := &fakeSubmitter{err: chain.ErrNoBaseFee}
		fc := &fakeReconciler{err: chain.ErrAuthorizationNotSettled}
		h := newHarness(t, func(c *settle.Config) {
			c.Submitters = map[string]settle.Submitter{"base": sub}
			c.Reconcilers = map[string]settle.Reconciler{"base": fc}
			c.StaleAfter = time.Minute
			c.Now = func() time.Time { return clock }
		})

		first := h.post("0xrc2")
		if first.Code != http.StatusServiceUnavailable {
			t.Fatalf("first status = %d, want 503", first.Code)
		}

		clock = clock.Add(2 * time.Minute)

		second := h.post("0xrc2")
		if second.Code != http.StatusOK {
			t.Fatalf("second status = %d, want 200 (collection outcome)", second.Code)
		}
		if resp := decodeSettle(t, second); resp.Success {
			t.Fatalf("resp = %+v, want success=false (authorization can no longer settle)", resp)
		}
		if sub.calls != 1 {
			t.Fatalf("submitter calls = %d, want 1", sub.calls)
		}

		// Terminal now: a further duplicate replays failed directly.
		third := h.post("0xrc2")
		if resp := decodeSettle(t, third); resp.Success {
			t.Fatalf("third resp = %+v, want success=false replayed", resp)
		}
		if fc.calls != 1 {
			t.Fatalf("reconciler calls after failing = %d, want 1 (no reconcile on an already-terminal row)", fc.calls)
		}
	})

	// Reproduces the exact gap #179 was filed for: Submit succeeds and the
	// original caller is told success, but the store write that should record
	// it transiently fails, leaving the row in_flight even though the money
	// already moved. A later duplicate must still recover the real tx hash
	// rather than single-flight to a permanent 503.
	t.Run("post-MarkSettled-failure recovers the real tx hash on a later duplicate", func(t *testing.T) {
		// MemoryStore.Claim stamps CreatedAt from the real wall clock (Store's
		// timestamps are not injectable), so the fake clock used for staleness
		// starts from real "now" too, rather than an arbitrary fixed epoch.
		clock := time.Now().UTC()
		wantHash := common.HexToHash(txHashHex)
		sub := &fakeSubmitter{tx: wantHash}
		fc := &fakeReconciler{tx: wantHash}
		store := &markSettledFailOnceStore{MemoryStore: settlement.NewMemoryStore()}
		h := newHarness(t, func(c *settle.Config) {
			c.Submitters = map[string]settle.Submitter{"base": sub}
			c.Reconcilers = map[string]settle.Reconciler{"base": fc}
			c.Store = store
			c.StaleAfter = time.Minute
			c.Now = func() time.Time { return clock }
		})

		// The original request: the chain submit succeeds and the caller is
		// told so, even though the record write behind it fails (the store
		// write failure must never change what a successful submit reports).
		first := h.post("0xrc3")
		if first.Code != http.StatusOK {
			t.Fatalf("first status = %d, want 200", first.Code)
		}
		if resp := decodeSettle(t, first); !resp.Success || resp.Transaction != txHashHex {
			t.Fatalf("first resp = %+v, want success with tx %q despite the record-write failure", resp, txHashHex)
		}

		clock = clock.Add(2 * time.Minute) // past StaleAfter

		second := h.post("0xrc3")
		if second.Code != http.StatusOK {
			t.Fatalf("second status = %d, want 200 (reconciled to settled)", second.Code)
		}
		if resp := decodeSettle(t, second); !resp.Success || resp.Transaction != txHashHex {
			t.Fatalf("second resp = %+v, want the same tx hash recovered via reconciliation", resp)
		}
		if sub.calls != 1 {
			t.Fatalf("submitter calls = %d, want 1 (no re-broadcast)", sub.calls)
		}
		if fc.calls != 1 {
			t.Fatalf("reconciler calls = %d, want 1", fc.calls)
		}
	})

	t.Run("a fresh in_flight row is not reconciled before StaleAfter elapses", func(t *testing.T) {
		sub := &fakeSubmitter{err: chain.ErrNoBaseFee}
		fc := &fakeReconciler{tx: common.HexToHash(txHashHex)}
		h := newHarness(t, func(c *settle.Config) {
			c.Submitters = map[string]settle.Submitter{"base": sub}
			c.Reconcilers = map[string]settle.Reconciler{"base": fc}
			c.StaleAfter = time.Hour
		})

		h.post("0xrc4")
		second := h.post("0xrc4")
		if second.Code != http.StatusServiceUnavailable {
			t.Fatalf("second status = %d, want 503 (row not yet stale)", second.Code)
		}
		if fc.calls != 0 {
			t.Fatalf("reconciler calls = %d, want 0 (must not reconcile a fresh in_flight row)", fc.calls)
		}
	})

	// A nonce collision alone is not proof this duplicate is the same
	// authorization as the claimed row — same guard as the settled-replay
	// path (TestSettle_MismatchedNonceReplayIsNotEchoedAsSuccess). A mismatched
	// duplicate must not trigger (or learn the outcome of) a chain reconcile
	// keyed on someone else's authorization.
	t.Run("a mismatched authorization on a stale row is not reconciled", func(t *testing.T) {
		// MemoryStore.Claim stamps CreatedAt from the real wall clock (Store's
		// timestamps are not injectable), so the fake clock used for staleness
		// starts from real "now" too, rather than an arbitrary fixed epoch.
		clock := time.Now().UTC()
		sub := &fakeSubmitter{err: chain.ErrNoBaseFee}
		fc := &fakeReconciler{tx: common.HexToHash(txHashHex)}
		h := newHarness(t, func(c *settle.Config) {
			c.Submitters = map[string]settle.Submitter{"base": sub}
			c.Reconcilers = map[string]settle.Reconciler{"base": fc}
			c.StaleAfter = time.Minute
			c.Now = func() time.Time { return clock }
		})

		h.post("0xrc5")
		clock = clock.Add(2 * time.Minute)

		attacker := "0x9999999999999999999999999999999999999999"
		rec := h.postBody(settleBodyFrom("0xrc5", attacker))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (mismatched authorization must not be reconciled)", rec.Code)
		}
		if fc.calls != 0 {
			t.Fatalf("reconciler calls = %d, want 0", fc.calls)
		}
	})
}
