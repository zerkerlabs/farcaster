// Command facilitator is the Zerker-hosted x402 facilitator: the `/settle`
// server the gateway POSTs verified payment authorizations to (ADR-0010; the
// first commercial-tier deliverable, scoped by spec 0007). It terminates mutual
// TLS and maps the caller's client certificate to a facilitator account (T1),
// advertises capability and liveness/readiness via /supported and /healthz
// (T7), and runs the full settle orchestration behind the readiness gate (T5):
// independent re-verify (T2) → nonce dedupe (T4) → submit + first-block
// inclusion (T3) → record (T4).
//
// Signing goes through the chain package's Signer interface (Decision 2). This
// build wires the self-hostable local-keystore signer from env; the managed
// AWS-KMS signer (same interface) is injected as managed deployment
// configuration and is a follow-up here.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/gateway/facilitator/db"
	"github.com/zerkerlabs/gateway/facilitator/internal/account"
	"github.com/zerkerlabs/gateway/facilitator/internal/chain"
	"github.com/zerkerlabs/gateway/facilitator/internal/guardrail"
	"github.com/zerkerlabs/gateway/facilitator/internal/health"
	"github.com/zerkerlabs/gateway/facilitator/internal/mtls"
	"github.com/zerkerlabs/gateway/facilitator/internal/settle"
	"github.com/zerkerlabs/gateway/facilitator/internal/settlement"
	"github.com/zerkerlabs/gateway/facilitator/internal/sweep"
	"github.com/zerkerlabs/gateway/facilitator/internal/verify"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		log.Fatal(err)
	}
}

// shutdownTimeout bounds how long graceful shutdown waits for the in-flight
// HTTP server to drain once a shutdown signal arrives, before giving up.
const shutdownTimeout = 10 * time.Second

func run(logger *slog.Logger) error {
	// SIGINT/SIGTERM cancels ctx — the one shutdown signal both the HTTP
	// server drain and the periodic sweep loop (spec 0007 follow-up #197)
	// hang off of, so the process stops cleanly as a unit rather than the
	// sweep goroutine outliving the server or vice versa.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("FACILITATOR_ADDR")
	if addr == "" {
		addr = ":8090"
	}

	tlsCfg, err := serverTLSConfigFromEnv()
	if err != nil {
		return fmt.Errorf("configure mTLS: %w", err)
	}

	// Facilitator accounts are provisioned manually/allowlisted for v1 (spec
	// 0007 Decision 7); no seeding or admin API exists yet.
	accounts := account.NewMemoryStore()

	staleAfter, err := staleAfterFromEnv()
	if err != nil {
		return err
	}
	sweepInterval, err := sweepIntervalFromEnv()
	if err != nil {
		return err
	}

	settleHandler, store, reconcilers, err := buildSettleHandler(ctx, logger, staleAfter)
	if err != nil {
		return fmt.Errorf("build /settle handler: %w", err)
	}

	checker, kinds, err := readinessFromEnv()
	if err != nil {
		return fmt.Errorf("configure readiness: %w", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           routes(accounts, settleHandler, checker, kinds, logger),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The periodic sweep (spec 0007 follow-up #179/#197): reconciles in_flight
	// settlement rows stuck past staleAfter through the same #196 chain-read
	// reconcile primitive the on-read duplicate-request path uses, so recovery
	// from a post-MarkSettled-failure row never depends on a duplicate /settle
	// request landing on it. Multiple replicas may run this loop unsynchronized
	// (see package sweep's doc) — no leader election, by design (spec 0007
	// follow-up #197 scope).
	sweeper := sweep.New(sweep.Config{
		Store:       store,
		Reconcilers: reconcilers,
		StaleAfter:  staleAfter,
		Interval:    sweepInterval,
		Logger:      logger,
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sweeper.Run(ctx)
	}()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("facilitator listening", "addr", addr, "sweep_interval", sweepInterval, "stale_after", staleAfter)
		// Certificates are already set on TLSConfig, so no file paths are
		// passed here (ListenAndServeTLS accepts empty strings in that case).
		serveErr <- srv.ListenAndServeTLS("", "")
	}()

	select {
	case err := <-serveErr:
		stop() // also stop the sweep loop if the server exits on its own
		waitWithTimeout(&wg, shutdownTimeout, logger)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("facilitator: graceful shutdown of HTTP server failed", "err", err)
		}
		waitWithTimeout(&wg, shutdownTimeout, logger) // drain the sweep loop before returning
		<-serveErr
		return nil
	}
}

// waitWithTimeout waits for wg, bounded by timeout. The sweep loop (unlike
// the HTTP server) has no server-level drain deadline of its own — it stops
// only once its in-progress SweepOnce call returns and ctx.Done() is
// observed — so if a chain RPC call inside one ever fails to honor context
// cancellation, this stops the process from hanging on shutdown forever
// instead of leaving the sweep goroutine to finish on its own time.
func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration, logger *slog.Logger) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logger.Error("facilitator: sweep loop did not stop within shutdown timeout, exiting anyway", "timeout", timeout)
	}
}

// staleAfterFromEnv parses FACILITATOR_STALE_AFTER (a Go duration string,
// e.g. "2m") — how old an in_flight settlement row must be before either the
// on-read duplicate-request path (#196) or the periodic sweep (#197) attempts
// chain-read reconciliation. Empty uses settle.DefaultStaleAfter.
func staleAfterFromEnv() (time.Duration, error) {
	s := os.Getenv("FACILITATOR_STALE_AFTER")
	if s == "" {
		return settle.DefaultStaleAfter, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid FACILITATOR_STALE_AFTER %q: must be a positive Go duration (e.g. \"2m\")", s)
	}
	return d, nil
}

// sweepIntervalFromEnv parses FACILITATOR_SWEEP_INTERVAL (a Go duration
// string, e.g. "30s") — how often the periodic sweep (#197) runs. Empty uses
// sweep.DefaultInterval.
func sweepIntervalFromEnv() (time.Duration, error) {
	s := os.Getenv("FACILITATOR_SWEEP_INTERVAL")
	if s == "" {
		return sweep.DefaultInterval, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid FACILITATOR_SWEEP_INTERVAL %q: must be a positive Go duration (e.g. \"30s\")", s)
	}
	return d, nil
}

// reconcileLookbackBlocksFromEnv parses FACILITATOR_RECONCILE_LOOKBACK_BLOCKS
// — how far back chain.Client.Reconcile searches chain logs for the
// AuthorizationUsed event (spec 0007 follow-up #179's nit, closed by #197).
// Empty returns 0, which chain.Dial/chain.Config fills with its own default.
func reconcileLookbackBlocksFromEnv() (uint64, error) {
	s := os.Getenv("FACILITATOR_RECONCILE_LOOKBACK_BLOCKS")
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid FACILITATOR_RECONCILE_LOOKBACK_BLOCKS %q: must be a positive integer", s)
	}
	return n, nil
}

// serverTLSConfigFromEnv builds the facilitator's mTLS server configuration
// from FACILITATOR_TLS_CERT_FILE / FACILITATOR_TLS_KEY_FILE (the facilitator's
// own server certificate + key) and FACILITATOR_CLIENT_CA_FILE (the CA that
// signs caller client certificates). All three are required — the facilitator
// must not start in a mode that would silently accept plaintext or
// unauthenticated connections (AGENTS.md invariants #1 and #5).
func serverTLSConfigFromEnv() (*tls.Config, error) {
	certFile := os.Getenv("FACILITATOR_TLS_CERT_FILE")
	keyFile := os.Getenv("FACILITATOR_TLS_KEY_FILE")
	clientCAFile := os.Getenv("FACILITATOR_CLIENT_CA_FILE")
	if certFile == "" || keyFile == "" || clientCAFile == "" {
		return nil, fmt.Errorf("FACILITATOR_TLS_CERT_FILE, FACILITATOR_TLS_KEY_FILE, and FACILITATOR_CLIENT_CA_FILE are all required")
	}

	serverCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server certificate: %w", err)
	}

	caPEM, err := os.ReadFile(clientCAFile) //nolint:gosec // operator-configured path via FACILITATOR_CLIENT_CA_FILE, not user input
	if err != nil {
		return nil, fmt.Errorf("read client CA file: %w", err)
	}
	clientCAs, err := mtls.ClientCAPool(caPEM)
	if err != nil {
		return nil, fmt.Errorf("parse client CA file: %w", err)
	}

	return mtls.ServerConfig(serverCert, clientCAs), nil
}

func routes(accounts account.Store, settleHandler http.Handler, checker health.Checker, kinds []health.Kind, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	// Authenticate (mTLS → account), then gate on readiness (RPC + gas), then
	// run the settle orchestration.
	mux.Handle("POST /settle", account.Middleware(accounts, logger)(health.GateSettle(checker, logger, settleHandler)))
	mux.Handle("GET /supported", health.Supported(kinds))
	mux.Handle("GET /healthz", health.Healthz(checker))
	return mux
}

// readinessFromEnv builds the /healthz-and-/settle readiness checker and the
// /supported capability list from FACILITATOR_RPC_URL, FACILITATOR_NETWORK,
// FACILITATOR_USDC_ADDRESS, FACILITATOR_GAS_WALLET_ADDRESS, and
// FACILITATOR_GAS_LOW_WATER_MARK_WEI. All five are required — a facilitator
// that cannot check RPC reachability or its own gas balance must not report
// readiness at all (spec 0007 T7), rather than silently reporting "ok".
//
// v1 settles exactly one (scheme, network, asset) kind per deployment
// (scheme "exact", USDC only — spec 0007 "Networks/assets (v1 cut)"), so a
// single configured network/asset pair is advertised. The gas wallet address
// is configured independently of the settle Signer (buildSigner); the two must
// name the same address in a real deployment.
func readinessFromEnv() (*chain.ReadinessChecker, []health.Kind, error) {
	rpcURL := os.Getenv("FACILITATOR_RPC_URL")
	network := os.Getenv("FACILITATOR_NETWORK")
	usdcAddr := os.Getenv("FACILITATOR_USDC_ADDRESS")
	gasWalletAddr := os.Getenv("FACILITATOR_GAS_WALLET_ADDRESS")
	lowWaterMarkStr := os.Getenv("FACILITATOR_GAS_LOW_WATER_MARK_WEI")
	if rpcURL == "" || network == "" || usdcAddr == "" || gasWalletAddr == "" || lowWaterMarkStr == "" {
		return nil, nil, fmt.Errorf("FACILITATOR_RPC_URL, FACILITATOR_NETWORK, FACILITATOR_USDC_ADDRESS, FACILITATOR_GAS_WALLET_ADDRESS, and FACILITATOR_GAS_LOW_WATER_MARK_WEI are all required")
	}
	if !common.IsHexAddress(usdcAddr) {
		return nil, nil, fmt.Errorf("FACILITATOR_USDC_ADDRESS is not a valid address: %q", usdcAddr)
	}
	if !common.IsHexAddress(gasWalletAddr) {
		return nil, nil, fmt.Errorf("FACILITATOR_GAS_WALLET_ADDRESS is not a valid address: %q", gasWalletAddr)
	}
	lowWaterMark, ok := new(big.Int).SetString(lowWaterMarkStr, 10)
	if !ok || lowWaterMark.Sign() < 0 {
		return nil, nil, fmt.Errorf("FACILITATOR_GAS_LOW_WATER_MARK_WEI is not a non-negative base-10 integer: %q", lowWaterMarkStr)
	}

	checker, err := chain.DialReadinessChecker(context.Background(), rpcURL, common.HexToAddress(gasWalletAddr), lowWaterMark)
	if err != nil {
		return nil, nil, err
	}

	kinds := []health.Kind{{
		Scheme:  "exact",
		Network: network,
		Asset:   common.HexToAddress(usdcAddr).Hex(),
	}}
	return checker, kinds, nil
}

// buildSettleHandler constructs the /settle orchestration from env. The
// facilitator is a money service, so every dependency it needs to actually
// settle is required — it fails closed rather than starting unable to settle
// (consistent with the mTLS and readiness requirements above). It also
// returns the settlement store and the network→Reconciler map, so run can
// hang the periodic sweep (spec 0007 follow-up #197) off the same store and
// chain client the /settle orchestration uses, rather than building either
// twice.
func buildSettleHandler(ctx context.Context, logger *slog.Logger, staleAfter time.Duration) (*settle.Handler, settlement.Store, map[string]settle.Reconciler, error) {
	store, err := buildSettlementStore(ctx, logger)
	if err != nil {
		return nil, nil, nil, err
	}

	signer, err := buildSigner()
	if err != nil {
		return nil, nil, nil, err
	}

	rpcURL := os.Getenv("FACILITATOR_RPC_URL")
	network := os.Getenv("FACILITATOR_NETWORK")
	usdc := os.Getenv("FACILITATOR_USDC_ADDRESS")
	chainIDStr := os.Getenv("FACILITATOR_CHAIN_ID")
	assetName := os.Getenv("FACILITATOR_ASSET_NAME")
	assetVersion := os.Getenv("FACILITATOR_ASSET_VERSION")
	if rpcURL == "" || network == "" || usdc == "" || chainIDStr == "" || assetName == "" || assetVersion == "" {
		return nil, nil, nil, errors.New("FACILITATOR_RPC_URL, FACILITATOR_NETWORK, FACILITATOR_USDC_ADDRESS, " +
			"FACILITATOR_CHAIN_ID, FACILITATOR_ASSET_NAME, and FACILITATOR_ASSET_VERSION are all required")
	}
	chainID, err := strconv.ParseInt(chainIDStr, 10, 64)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid FACILITATOR_CHAIN_ID: %w", err)
	}
	reconcileLookback, err := reconcileLookbackBlocksFromEnv()
	if err != nil {
		return nil, nil, nil, err
	}

	client, err := chain.Dial(ctx, rpcURL, signer, big.NewInt(chainID), chain.Config{
		USDC:                    common.HexToAddress(usdc),
		ReconcileLookbackBlocks: reconcileLookback,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect chain client: %w", err)
	}

	// v1 settles a single network/asset (Base + USDC); the policy and the
	// per-network submitter/reconciler maps are all keyed on that one network.
	policy := verify.Policy{Kinds: []verify.Kind{{
		Network:      network,
		Asset:        usdc,
		ChainID:      chainID,
		AssetName:    assetName,
		AssetVersion: assetVersion,
	}}}

	guardrailDefaults, err := guardrailDefaultsFromEnv(network, usdc)
	if err != nil {
		return nil, nil, nil, err
	}

	reconcilers := map[string]settle.Reconciler{network: client}
	handler := settle.NewHandler(settle.Config{
		Policy:            policy,
		Submitters:        map[string]settle.Submitter{network: client},
		Reconcilers:       reconcilers,
		Store:             store,
		GuardrailDefaults: guardrailDefaults,
		StaleAfter:        staleAfter,
		Logger:            logger,
	})
	return handler, store, reconcilers, nil
}

// guardrailDefaultsFromEnv builds the conservative service-wide guardrail
// defaults (spec 0007 T6, Decision 8) every account uses unless it carries
// its own account.Account.Guardrails override: the allowed (network, asset)
// set (this deployment's one configured kind — an account can never settle
// outside what the facilitator itself supports), a per-transaction maximum,
// and a per-account daily ceiling. FACILITATOR_GUARDRAIL_MAX_SETTLE_AMOUNT and
// FACILITATOR_GUARDRAIL_DAILY_CEILING are decimal strings in the asset's
// smallest unit (matching the x402 wire's value/maxAmountRequired) and both
// required — the facilitator must not start with an implicit "no limit"
// default.
func guardrailDefaultsFromEnv(network, usdc string) (guardrail.Limits, error) {
	maxSettleStr := os.Getenv("FACILITATOR_GUARDRAIL_MAX_SETTLE_AMOUNT")
	dailyCeilingStr := os.Getenv("FACILITATOR_GUARDRAIL_DAILY_CEILING")
	if maxSettleStr == "" || dailyCeilingStr == "" {
		return guardrail.Limits{}, errors.New("FACILITATOR_GUARDRAIL_MAX_SETTLE_AMOUNT and FACILITATOR_GUARDRAIL_DAILY_CEILING are both required")
	}
	maxSettle, ok := new(big.Int).SetString(maxSettleStr, 10)
	if !ok || maxSettle.Sign() <= 0 {
		return guardrail.Limits{}, fmt.Errorf("FACILITATOR_GUARDRAIL_MAX_SETTLE_AMOUNT is not a positive base-10 integer: %q", maxSettleStr)
	}
	dailyCeiling, ok := new(big.Int).SetString(dailyCeilingStr, 10)
	if !ok || dailyCeiling.Sign() <= 0 {
		return guardrail.Limits{}, fmt.Errorf("FACILITATOR_GUARDRAIL_DAILY_CEILING is not a positive base-10 integer: %q", dailyCeilingStr)
	}
	if maxSettle.Cmp(dailyCeiling) > 0 {
		return guardrail.Limits{}, errors.New("FACILITATOR_GUARDRAIL_MAX_SETTLE_AMOUNT must not exceed FACILITATOR_GUARDRAIL_DAILY_CEILING")
	}
	return guardrail.Limits{
		AllowedKinds:    []guardrail.Kind{{Network: network, Asset: usdc}},
		MaxSettleAmount: maxSettle,
		DailyCeiling:    dailyCeiling,
	}, nil
}

// buildSigner builds the gas-key signer. It wires the self-hostable
// local-encrypted keystore (a sovereign operator runs the same binary with
// their own key); the managed AWS-KMS signer implements the same chain.Signer
// interface and is injected as managed deployment config (a follow-up here —
// provisioning the KMS key/IAM is ops, spec 0007 OUT of T3).
func buildSigner() (chain.Signer, error) {
	ksFile := os.Getenv("FACILITATOR_SIGNER_KEYSTORE_FILE")
	if ksFile == "" {
		return nil, errors.New("FACILITATOR_SIGNER_KEYSTORE_FILE is required (managed AWS-KMS signer wiring is a follow-up)")
	}
	keyJSON, err := os.ReadFile(ksFile) //nolint:gosec // operator-configured path, not user input
	if err != nil {
		return nil, fmt.Errorf("read signer keystore: %w", err)
	}
	return chain.NewLocalSignerFromKeystore(keyJSON, os.Getenv("FACILITATOR_SIGNER_KEYSTORE_PASSPHRASE"))
}

// buildSettlementStore returns the Postgres-backed settlement store, running
// the facilitator's own migrations. FACILITATOR_DATABASE_URL is required so the
// money-path audit trail is durable; the non-durable in-memory store is only
// reachable via an explicit FACILITATOR_ALLOW_INMEMORY_STORE=true opt-in (local
// dev), never by omission in production.
func buildSettlementStore(ctx context.Context, logger *slog.Logger) (settlement.Store, error) {
	dbURL := os.Getenv("FACILITATOR_DATABASE_URL")
	if dbURL == "" {
		if os.Getenv("FACILITATOR_ALLOW_INMEMORY_STORE") != "true" {
			return nil, errors.New("FACILITATOR_DATABASE_URL is required (set FACILITATOR_ALLOW_INMEMORY_STORE=true to use the non-durable in-memory store for local dev only)")
		}
		logger.Warn("using in-memory settlement store (FACILITATOR_ALLOW_INMEMORY_STORE=true; not durable — dev only)")
		return settlement.NewMemoryStore(), nil
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return nil, fmt.Errorf("open facilitator database: %w", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		return nil, fmt.Errorf("migrate facilitator database: %w", err)
	}
	return settlement.NewPostgresStore(pool), nil
}
