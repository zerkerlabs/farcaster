// Package settle is the facilitator's /settle orchestration (spec 0007 T5): it
// wires the pieces the earlier tickets built into the money endpoint —
//
//	mTLS account (T1) → independent re-verify (T2) → guardrails (T6)
//	→ nonce dedupe (T4) → submit + await first-block inclusion (T3)
//	→ record (T4) → SettleResponse
//
// and implements the failure taxonomy the surface-6 client depends on:
//
//   - *Collection outcomes* (re-verify rejection, insufficient balance, expired
//     authorization, consumed nonce, on-chain revert, inclusion timeout) are
//     reported in-body as 200 { success:false, errorReason } with a single
//     coarse reason — the client turns this into settlement_failed and never
//     forwards. No internal detail or key material ever leaks (AGENTS.md
//     invariant #3).
//   - *Guardrail rejections* (spec 0007 T6, Decision 8: disallowed network/
//     asset, over the per-transaction maximum, or over the daily ceiling) are
//     429 — distinct from both a payment-validity outcome and a transient
//     protocol failure, so the caller can tell "this account is over a
//     configured limit" apart from either.
//   - *Protocol failures* (unknown account → 403, malformed body → 400, RPC
//     unreachable / store down / in-flight duplicate → 503) are HTTP errors, so
//     the client's bounded retry can tell "retry may help" from "this payment is
//     dead."
//
// The gas key stays entirely inside the chain client's Signer; this package
// never sees key material.
package settle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/zerkerlabs/gateway/facilitator/internal/account"
	"github.com/zerkerlabs/gateway/facilitator/internal/chain"
	"github.com/zerkerlabs/gateway/facilitator/internal/guardrail"
	"github.com/zerkerlabs/gateway/facilitator/internal/settlement"
	"github.com/zerkerlabs/gateway/facilitator/internal/verify"
	"github.com/zerkerlabs/gateway/x402types"
)

// coarseCollectionReason is the single body reason for every collection-outcome
// failure. It is deliberately uniform so a caller (or a hostile gateway) cannot
// probe which specific check failed, and so no internal detail leaks.
const coarseCollectionReason = "payment could not be collected"

// maxSettleRequestBytes caps the request body the facilitator will buffer,
// matching the gateway's inbound-body convention.
const maxSettleRequestBytes = 1 << 20 // 1 MiB

// Submitter submits a re-verified authorization on-chain and waits for
// first-block inclusion, returning the transaction hash. *chain.Client
// satisfies it; tests supply a fake. It is keyed per network by the Handler.
type Submitter interface {
	Submit(ctx context.Context, authz x402types.Authorization, signature string) (common.Hash, error)
}

// Reconciler re-checks the chain for a stuck in_flight settlement row's true
// outcome (spec 0007 follow-up #179: a post-submit record-write failure, or a
// transient submit/inclusion-timeout error T5 leaves in_flight with no store
// "release"). It never re-broadcasts, only reads. *chain.Client satisfies it
// via chain.Client.Reconcile; tests supply a fake. It is keyed per network,
// same as Submitter.
type Reconciler interface {
	Reconcile(ctx context.Context, authz x402types.Authorization, now time.Time) (common.Hash, error)
}

// Config is the Handler's injected dependencies.
type Config struct {
	// Policy is the re-verification policy (the settle-able (network, asset)
	// set, with the EIP-712 domain data the signature check needs). It backs
	// the default verifier; ignored if Verify is set explicitly.
	Policy verify.Policy

	// Verify performs the independent re-verification (Decision 4). It defaults
	// to verify.Reverify against Policy — the production path, a hard invariant.
	// It is injectable only so orchestration tests need not craft real EIP-3009
	// signatures; production never overrides it.
	Verify func(req x402types.SettleRequest, now time.Time) error

	// Submitters maps a network name to its chain submitter (one gas wallet /
	// RPC / USDC contract per network). A request whose network has no
	// submitter is a misconfiguration, not a caller error.
	Submitters map[string]Submitter

	// Store is the settlement record + nonce-idempotency store (T4); it also
	// backs the guardrail daily-ceiling lookup (T6, SumSettled).
	Store settlement.Store

	// Reconcilers maps a network name to its chain reconciler (spec 0007
	// follow-up #179), mirroring Submitters. A duplicate request that lands on
	// a stale in_flight row (older than StaleAfter) for a network with no
	// configured Reconciler gets the ordinary transient "settlement in
	// progress" response — reconciliation is best-effort, not required for
	// /settle to function.
	Reconcilers map[string]Reconciler

	// StaleAfter is how old an in_flight row (by CreatedAt) must be before a
	// duplicate request's replay attempts reconciliation instead of reporting
	// the ordinary transient "settlement in progress" — long enough that a
	// row still genuinely mid-broadcast is left alone. Defaults to 2 minutes
	// (comfortably past the chain client's default inclusion timeout).
	StaleAfter time.Duration

	// GuardrailDefaults are the conservative per-account guardrail limits
	// (spec 0007 T6, Decision 8) applied to any account without its own
	// account.Account.Guardrails override. A working handler requires this to
	// be populated with a concrete MaxSettleAmount and DailyCeiling — there is
	// no implicit "no limit" default (see guardrail.Limits).
	GuardrailDefaults guardrail.Limits

	Logger *slog.Logger

	// Now defaults to time.Now; injectable for deterministic tests.
	Now func() time.Time
}

// Handler serves POST /settle. Construct with NewHandler.
type Handler struct {
	verify            func(req x402types.SettleRequest, now time.Time) error
	submitters        map[string]Submitter
	store             settlement.Store
	guardrailDefaults guardrail.Limits
	logger            *slog.Logger
	now               func() time.Time
	reconcilers       map[string]Reconciler
	staleAfter        time.Duration
}

// DefaultStaleAfter is Config.StaleAfter's default (see its doc). Also the
// default the periodic sweep (package sweep, spec 0007 follow-up #197) uses
// when its own StaleAfter is left zero, so the two paths agree on what
// "stale" means unless an operator explicitly configures otherwise.
const DefaultStaleAfter = 2 * time.Minute

// NewHandler builds a Handler from cfg, filling defaults.
func NewHandler(cfg Config) *Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = DefaultStaleAfter
	}
	verifyFn := cfg.Verify
	if verifyFn == nil {
		policy := cfg.Policy
		verifyFn = func(req x402types.SettleRequest, now time.Time) error {
			return verify.Reverify(req, policy, now)
		}
	}
	return &Handler{
		verify:            verifyFn,
		submitters:        cfg.Submitters,
		store:             cfg.Store,
		guardrailDefaults: cfg.GuardrailDefaults,
		logger:            cfg.Logger,
		now:               cfg.Now,
		reconcilers:       cfg.Reconcilers,
		staleAfter:        cfg.StaleAfter,
	}
}

// ServeHTTP runs the settle orchestration. The caller has already been
// authenticated and mapped to an active facilitator account by
// account.Middleware; this handler must be mounted behind it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	acct, ok := account.FromContext(r.Context())
	if !ok {
		// Defensive: the middleware guarantees an active account before this
		// handler runs. Reaching here means a misconfigured mount — fail closed.
		h.writeProtocol(w, http.StatusForbidden, "unknown facilitator account")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSettleRequestBytes)
	var req x402types.SettleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeProtocol(w, http.StatusBadRequest, "invalid settle request")
		return
	}

	// Independent re-verification (T2, Decision 4 — a hard invariant). Any
	// rejection is a collection outcome: 200 success:false, no submission.
	if err := h.verify(req, h.now()); err != nil {
		h.writeOutcome(w)
		return
	}

	authz := req.PaymentPayload.Payload.Authorization
	reqmts := req.PaymentRequirements

	// Per-account guardrails (T6, Decision 8) — bound the blast radius of a
	// compromised client cert. The static checks (allowed network/asset,
	// per-tx max) run here, after re-verify (so they never pay for a signature
	// recovery on an already-invalid request) and before nonce claim (so an
	// over-limit request never creates a settlement row). The daily ceiling is
	// deliberately NOT checked here: it is enforced atomically with the claim
	// below (see settlement.Store.Claim's DailyCeiling and
	// guardrail.CheckKindAndAmount's doc) so a burst of concurrent requests
	// can't each pass a stale snapshot of the ceiling independently.
	limits, amount, err := h.resolveGuardrails(acct, reqmts.Network, reqmts.Asset, authz.Value)
	if err != nil {
		if errors.Is(err, errGuardrailMisconfigured) {
			h.logger.Error("settle: guardrail limits not configured for account", "account", acct.ID)
			h.writeProtocol(w, http.StatusServiceUnavailable, "settlement temporarily unavailable")
			return
		}
		if isGuardrailRejection(err) {
			// Distinct from both a collection outcome (payment validity) and a
			// protocol failure (retry may help): the account itself is over a
			// configured limit. No limit value or account detail leaks
			// (AGENTS.md invariant #3).
			h.writeProtocol(w, http.StatusTooManyRequests, "settlement guardrail exceeded")
			return
		}
		h.logger.Error("settle: guardrail check failed", "err", err)
		h.writeProtocol(w, http.StatusServiceUnavailable, "settlement temporarily unavailable")
		return
	}

	// Nonce dedupe BEFORE broadcast (T4), with the daily ceiling enforced
	// atomically as part of the same claim (T6) so concurrent distinct-nonce
	// requests for this account can't collectively exceed it.
	row, claimed, err := h.store.Claim(r.Context(), settlement.ClaimKey{
		AccountID:   acct.ID,
		Nonce:       authz.Nonce,
		Payer:       authz.From,
		PayTo:       reqmts.PayTo,
		Network:     reqmts.Network,
		Asset:       reqmts.Asset,
		Value:       authz.Value,
		ValidAfter:  authz.ValidAfter,
		ValidBefore: authz.ValidBefore,
	}, settlement.DailyCeiling{
		Since:  guardrail.StartOfUTCDay(h.now()),
		Amount: amount,
		Limit:  limits.DailyCeiling,
	})
	if err != nil {
		if isGuardrailRejection(err) {
			h.writeProtocol(w, http.StatusTooManyRequests, "settlement guardrail exceeded")
			return
		}
		h.logger.Error("settle: claim failed", "err", err)
		h.writeProtocol(w, http.StatusServiceUnavailable, "settlement temporarily unavailable")
		return
	}
	if !claimed {
		h.replay(r.Context(), w, row, reqmts, authz)
		return
	}

	// We own the broadcast for this nonce.
	submitter, ok := h.submitters[reqmts.Network]
	if !ok {
		// Reverify's policy admitted this network but no submitter is wired for
		// it — a server misconfiguration, not a caller error. Leave the row
		// in-flight (reconciliation is out of T5 scope) and report transient.
		h.logger.Error("settle: no submitter configured for network", "network", reqmts.Network)
		h.writeProtocol(w, http.StatusServiceUnavailable, "settlement temporarily unavailable")
		return
	}

	txHash, err := submitter.Submit(r.Context(), authz, req.PaymentPayload.Payload.Signature)
	switch {
	case err == nil:
		h.mark(r.Context(), func() (*settlement.Settlement, error) {
			return h.store.MarkSettled(r.Context(), row.ID, txHash.Hex())
		})
		h.writeSuccess(w, txHash.Hex(), reqmts.Network, authz.From)

	case isCollectionOutcome(err):
		// Revert / would-revert / inclusion timeout: the transfer did not land.
		// Record a terminal failure so a retry replays the same coarse outcome
		// without re-broadcasting, and report the outcome in-body. A reverted
		// or timed-out settlement can still have broadcast — chain.Client.Submit
		// returns the real tx hash alongside those outcome errors — so it is
		// recorded too (empty when nothing was ever broadcast, e.g.
		// ErrGasEstimation) rather than lost from the audit trail.
		h.mark(r.Context(), func() (*settlement.Settlement, error) {
			return h.store.MarkFailed(r.Context(), row.ID, coarseCollectionReason, txHashOrEmpty(txHash))
		})
		h.writeOutcome(w)

	default:
		// Transient/internal (RPC unreachable, no base fee, etc.). Leave the
		// row in-flight and report transient. A retry of the same nonce
		// single-flights to 503 until the row goes stale (StaleAfter), at
		// which point replay's reconcileInFlight re-checks the chain directly
		// (#179) instead of leaving it stuck forever.
		h.logger.Error("settle: submit failed", "err", err)
		h.writeProtocol(w, http.StatusServiceUnavailable, "settlement temporarily unavailable")
	}
}

// replay maps an already-existing settlement row (a duplicate nonce) onto a
// response without re-broadcasting. Store.Claim dedupes only on (AccountID,
// Nonce) — Payer/PayTo/Network/Asset/Value are stored for audit, not compared
// — so a nonce collision alone does not prove this request is the same
// authorization as the claimed row (nonces are attacker-choosable and
// observable on-chain). Only a request that matches the row on every one of
// those fields is a legitimate idempotent replay.
func (h *Handler) replay(ctx context.Context, w http.ResponseWriter, row *settlement.Settlement, reqmts x402types.PaymentRequirements, authz x402types.Authorization) {
	switch row.Status {
	case settlement.StatusSettled:
		if !claimMatchesRow(row, reqmts, authz) {
			// A distinct, independently-signed authorization that happens to
			// reuse a nonce already settled for a different payer/payTo/value.
			// Never echo the unrelated tx hash — report a coarse failure like
			// any other collection outcome.
			h.writeOutcome(w)
			return
		}
		// Idempotent success: same tx hash, no re-broadcast. Echo the row's own
		// recorded network/payer (the recorded truth), not request-supplied
		// values.
		h.writeSuccess(w, row.TxHash, row.Network, row.Payer)
	case settlement.StatusFailed:
		h.writeOutcome(w)
	default:
		// In-flight: another submission owns this nonce (single-flight) —
		// unless the row is stale enough to attempt reconciliation (#179):
		// re-check the chain for the authorization's true outcome rather than
		// leave it stuck forever behind a transient response.
		if updated := h.reconcileInFlight(ctx, row, reqmts, authz); updated != nil {
			h.replay(ctx, w, updated, reqmts, authz)
			return
		}
		// Report transient so the caller's bounded retry can pick up the
		// settled result.
		h.writeProtocol(w, http.StatusServiceUnavailable, "settlement in progress")
	}
}

// reconcileInFlight re-checks the chain for row's true outcome and transitions
// it to a terminal state, returning the updated row — or nil if reconciliation
// does not apply (the row isn't stale yet, the request doesn't match the row,
// no reconciler is configured for the network) or ReconcileRow itself declines
// to transition it (see its doc).
func (h *Handler) reconcileInFlight(ctx context.Context, row *settlement.Settlement, reqmts x402types.PaymentRequirements, authz x402types.Authorization) *settlement.Settlement {
	if h.now().Sub(row.CreatedAt) < h.staleAfter {
		return nil
	}
	// Only reconcile using an authorization the request proves it actually
	// knows (same guard as echoing a settled row's tx hash in replay) — a
	// distinct authorization that happens to collide on nonce must not trigger
	// (or learn the result of) a chain check keyed on someone else's row.
	if !claimMatchesRow(row, reqmts, authz) {
		return nil
	}
	reconciler, ok := h.reconcilers[row.Network]
	if !ok {
		return nil
	}
	return ReconcileRow(ctx, h.store, reconciler, row, h.now(), h.logger)
}

// ReconcileRow re-checks the chain for row's true outcome via reconciler and
// transitions it in store to the terminal state Reconcile determines, or
// leaves it untouched (chain.ErrReconcileIndeterminate, a transient chain-read
// failure, or — via the store's in_flight-only transition guard,
// MarkSettled/MarkFailed, #176 — a row already resolved by someone else
// between the caller's staleness check and this call, which is simply left
// alone rather than clobbered). It never re-broadcasts.
//
// This is the #196 reconcile primitive both callers share: the on-read
// duplicate-request path (Handler.reconcileInFlight, keyed off a request the
// caller has already matched against row) and the periodic sweep (package
// sweep, spec 0007 follow-up #197, keyed directly off the store's own row —
// there is no request to match against, only row's own recorded fields).
func ReconcileRow(ctx context.Context, store settlement.Store, reconciler Reconciler, row *settlement.Settlement, now time.Time, logger *slog.Logger) *settlement.Settlement {
	// A row claimed before migration 002 shipped has no recorded validity
	// window: ValidBefore defaults to "0", which chain.Client.Reconcile reads
	// as "window already elapsed" — it would reconcile a row that might still
	// be genuinely pending to StatusFailed, a terminal state per the store's
	// in_flight-only transition guard. Leave it in_flight for manual/support
	// handling rather than guess at an outcome from data the row never had.
	if row.ValidBefore == "0" {
		logger.Warn("settle: reconcile skipped, row has no recorded validity window (pre-dates migration 002)",
			"id", row.ID, "account_id", row.AccountID, "nonce", row.Nonce)
		return nil
	}

	authz := x402types.Authorization{
		From:        row.Payer,
		To:          row.PayTo,
		Nonce:       row.Nonce,
		Value:       row.Value,
		ValidAfter:  row.ValidAfter,
		ValidBefore: row.ValidBefore,
	}

	txHash, err := reconciler.Reconcile(ctx, authz, now)
	switch {
	case err == nil:
		updated, mErr := store.MarkSettled(ctx, row.ID, txHash.Hex())
		if mErr != nil {
			logger.Error("settle: reconcile mark-settled failed", "err", mErr)
			return nil
		}
		return updated
	case errors.Is(err, chain.ErrAuthorizationNotSettled):
		updated, mErr := store.MarkFailed(ctx, row.ID, coarseCollectionReason, "")
		if mErr != nil {
			logger.Error("settle: reconcile mark-failed failed", "err", mErr)
			return nil
		}
		return updated
	case errors.Is(err, chain.ErrReconcileIndeterminate):
		return nil
	default:
		logger.Error("settle: reconcile chain check failed", "err", err)
		return nil
	}
}

// claimMatchesRow reports whether the incoming request's authorization is the
// same one the claimed row records, rather than a distinct authorization that
// happens to reuse the same nonce. Address fields are compared
// case-insensitively, matching verify.Reverify's checksummed-vs-lowercase
// tolerance, so a legitimate retry isn't misreported as a nonce collision.
func claimMatchesRow(row *settlement.Settlement, reqmts x402types.PaymentRequirements, authz x402types.Authorization) bool {
	return strings.EqualFold(row.Payer, authz.From) &&
		strings.EqualFold(row.PayTo, reqmts.PayTo) &&
		row.Network == reqmts.Network &&
		strings.EqualFold(row.Asset, reqmts.Asset) &&
		row.Value == authz.Value
}

// errGuardrailMisconfigured marks a resolved guardrail.Limits missing a
// required MaxSettleAmount or DailyCeiling — a server misconfiguration (there
// is no implicit "no limit" default), not a caller error, so it is reported
// like any other protocol failure (503) rather than a 429 limit rejection.
var errGuardrailMisconfigured = errors.New("settle: guardrail limits not configured for account")

// resolveGuardrails resolves the effective per-account guardrail limits (T6,
// Decision 8) — acct.Guardrails if set, else the handler's service-wide
// default — and enforces the two static checks that need no I/O: the
// (network, asset) pair is allowed, and value is within the per-transaction
// maximum. It returns the resolved limits and parsed amount so the caller can
// pass DailyCeiling.Limit and DailyCeiling.Amount to Store.Claim, which
// enforces the daily ceiling atomically with the nonce claim (see
// guardrail.CheckKindAndAmount's doc for why the ceiling isn't checked here).
func (h *Handler) resolveGuardrails(acct *account.Account, network, asset, value string) (guardrail.Limits, *big.Int, error) {
	limits := h.guardrailDefaults
	if acct.Guardrails != nil {
		limits = *acct.Guardrails
	}
	if limits.MaxSettleAmount == nil || limits.DailyCeiling == nil {
		return guardrail.Limits{}, nil, errGuardrailMisconfigured
	}
	if err := limits.Validate(); err != nil {
		// Mixed-asset AllowedKinds would let the daily ceiling sum incompatible
		// units (see guardrail.ErrMixedAssetKinds) — treat like any other
		// malformed Limits and reject closed rather than settle under it.
		return guardrail.Limits{}, nil, errGuardrailMisconfigured
	}

	amount, ok := new(big.Int).SetString(value, 10)
	if !ok {
		// Unreachable in production: re-verify above already rejects a
		// non-integer authorization value. Defensive fail-closed in case a
		// test injects a permissive Verify with a malformed value.
		return guardrail.Limits{}, nil, fmt.Errorf("settle: non-integer authorization value %q", value)
	}

	if err := guardrail.CheckKindAndAmount(limits, network, asset, amount); err != nil {
		return guardrail.Limits{}, nil, err
	}
	return limits, amount, nil
}

// isGuardrailRejection reports whether err is one of guardrail.Check's typed
// rejections (a limit exceeded), as opposed to a misconfiguration or a
// store/parse failure.
func isGuardrailRejection(err error) bool {
	return errors.Is(err, guardrail.ErrKindNotAllowed) ||
		errors.Is(err, guardrail.ErrAmountExceedsMax) ||
		errors.Is(err, guardrail.ErrDailyCeilingExceeded)
}

// txHashOrEmpty returns h's hex string, or "" for the zero hash — Submit
// returns a zero hash when the transaction was never broadcast (e.g.
// ErrGasEstimation), so there is no tx to record.
func txHashOrEmpty(h common.Hash) string {
	if h == (common.Hash{}) {
		return ""
	}
	return h.Hex()
}

// isCollectionOutcome reports whether a submit error is a collection outcome
// (reported in-body as success:false) rather than a transient/protocol failure.
func isCollectionOutcome(err error) bool {
	return errors.Is(err, chain.ErrReverted) ||
		errors.Is(err, chain.ErrGasEstimation) ||
		errors.Is(err, chain.ErrInclusionTimeout)
}

// mark runs a best-effort store transition. A store write that fails after the
// on-chain outcome is known must not change the client-visible result — the
// chain is authoritative — so the error is logged, not surfaced. If this write
// fails after a successful submit, the row stays StatusInFlight even though
// funds already moved; a retry of the same nonce single-flights to a
// transient 503 until the row goes stale, at which point replay's
// reconcileInFlight (#179) re-checks the chain and recovers the real tx hash
// rather than leaving it stuck forever.
func (h *Handler) mark(_ context.Context, fn func() (*settlement.Settlement, error)) {
	if _, err := fn(); err != nil {
		h.logger.Error("settle: record settlement outcome failed", "err", err)
	}
}

func (h *Handler) writeSuccess(w http.ResponseWriter, txHash, network, payer string) {
	writeJSON(w, http.StatusOK, x402types.SettleResponse{
		Success:     true,
		Transaction: txHash,
		Network:     network,
		Payer:       payer,
		// v1 absorbs gas and takes no cut (Decision 1).
		FacilitatorFee: "0",
	})
}

func (h *Handler) writeOutcome(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, x402types.SettleResponse{
		Success:     false,
		ErrorReason: coarseCollectionReason,
	})
}

func (h *Handler) writeProtocol(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
