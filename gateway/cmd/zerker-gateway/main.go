// Command zerker-gateway is the entrypoint for the Zerker gateway.
//
// Zerker is Zerker's gateway to manage, analyze, and productize agents and
// agentic workflows. This binary serves operational endpoints and the Agent
// Catalog surface (spec 0001); further product surfaces land as they are
// specced under docs/specs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/gateway/gateway/db"
	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/gateway/internal/auth"
	"github.com/zerkerlabs/gateway/gateway/internal/credential"
	"github.com/zerkerlabs/gateway/gateway/internal/httpapi"
	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
	"github.com/zerkerlabs/gateway/gateway/internal/kms"
	"github.com/zerkerlabs/gateway/gateway/internal/policy"
	"github.com/zerkerlabs/gateway/gateway/internal/proxy"
	"github.com/zerkerlabs/gateway/gateway/internal/ratelimit"
	"github.com/zerkerlabs/gateway/gateway/internal/server"
	"github.com/zerkerlabs/gateway/gateway/internal/settlement"
	"github.com/zerkerlabs/gateway/gateway/internal/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	addr := os.Getenv("ZERKER_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	if err := run(logger, addr); err != nil {
		logger.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, addr string) error {
	// Store selection: use Postgres when ZERKER_DATABASE_URL or DATABASE_URL is
	// set; fall back to the in-memory store for local dev only (non-durable).
	store, credSvc, invStore, settlementStore, policyStore, decisionStore, closeStore, err := openStore(logger)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer closeStore()

	// Auth middleware is required — the server does not start without OIDC config.
	// Set ZERKER_OIDC_ISSUER and ZERKER_OIDC_AUDIENCE to configure.
	authCfg := auth.ConfigFromEnv()
	mw, err := auth.NewMiddleware(context.Background(), authCfg, logger)
	if err != nil {
		return fmt.Errorf("init auth middleware: %w", err)
	}

	// Per-caller rate limiting (invariant #8). Runs inside auth so it keys on
	// the authenticated identity.
	// Zero-value Config uses documented defaults (rate, burst, eviction TTL).
	limiter := ratelimit.New(ratelimit.Config{})

	// Per-agent invocation rate limiter (spec 0002 Q11/Q12). Applied at the
	// proxy handler layer when an agent has InvocationRateLimit set.
	agentLimiter := ratelimit.NewAgentLimiter(0) // 0 → DefaultTTL
	defer agentLimiter.Close()

	// Observed per-caller rate for the policy enforcement point's rate_per_min
	// matching (spec 0009 fast-follow, #212).
	rateObserver := ratelimit.NewObservedRateTracker(0) // 0 → DefaultTTL
	defer rateObserver.Close()

	// Tighter per-caller limiter for GET /v1/analytics: percentile aggregation
	// is heavier than a row fetch, so it gets a stricter bound than the global
	// per-caller limiter (spec 0003 decision 4).
	analyticsLimiter := ratelimit.New(ratelimit.Config{Rate: 1, Burst: 5})
	defer analyticsLimiter.Close()

	fwd := proxy.New(store, credSvc, proxy.Config{}, logger)

	// Build the API handler explicitly so we can hold a reference for draining
	// in-flight transactional goroutines on graceful shutdown (issue #53).
	apiHandler := httpapi.NewHandler(store, logger).
		WithCredentials(credSvc).
		WithProxy(fwd, invStore).
		WithAgentLimiter(agentLimiter).
		WithRateObserver(rateObserver).
		WithAnalyticsLimiter(analyticsLimiter).
		WithSettlement(settlementStore).
		WithSettler(httpapi.NewFacilitatorSettler(nil), credSvc).
		WithPolicy(policyStore).
		WithPolicyDecisions(decisionStore)

	srv := &http.Server{
		Addr: addr,
		Handler: server.New(server.Config{
			APIHandler:     apiHandler,
			AuthMiddleware: mw,
			RateLimit:      limiter.Handler(),
			Logger:         logger,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Shut down cleanly on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("zerker gateway listening",
			"addr", addr, "version", version.Version, "commit", version.Commit)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
		// Allow 30 s for active HTTP connections (including streaming) to drain.
		httpCtx, httpCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer httpCancel()
		if err := srv.Shutdown(httpCtx); err != nil {
			logger.Warn("http server shutdown", "err", err)
		}
		// Drain in-flight transactional goroutines: Shutdown cancels their
		// upstream calls via the handler's internal context; goroutines then
		// record a terminal invocation status and exit. Allow 30 s for drain.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer drainCancel()
		return apiHandler.Shutdown(drainCtx)
	}
}

// openStore selects store and service implementations from environment variables.
// Returns the agent store, credential service, invocation store, settlement
// config store, policy store, policy decision store, and a cleanup function.
// The caller must call the cleanup function when done.
func openStore(logger *slog.Logger) (agent.AgentStore, *credential.Service, invocation.Store, settlement.Store, policy.PolicyStore, policy.DecisionStore, func(), error) {
	dbURL := os.Getenv("ZERKER_DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}

	if dbURL != "" {
		pool, err := pgxpool.New(context.Background(), dbURL)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, func() {}, fmt.Errorf("open database pool: %w", err)
		}
		if err := db.Migrate(context.Background(), pool); err != nil {
			pool.Close()
			return nil, nil, nil, nil, nil, nil, func() {}, fmt.Errorf("run migrations: %w", err)
		}
		kmsProvider, err := kms.NewLocalProvider()
		if err != nil {
			pool.Close()
			return nil, nil, nil, nil, nil, nil, func() {}, fmt.Errorf("init kms provider: %w", err)
		}
		credSvc := credential.NewService(
			credential.NewPostgresStore(pool),
			credential.NewPostgresKEKStore(pool),
			kmsProvider,
			credential.StubVaultResolver{},
		)
		return agent.NewPostgresStore(pool), credSvc, invocation.NewPostgresStore(pool), settlement.NewPostgresStore(pool), policy.NewPostgresStore(pool), policy.NewPostgresDecisionStore(pool), pool.Close, nil
	}

	logger.Warn("WARNING: DATABASE_URL is not set — using in-memory store; " +
		"all registered agents will be lost on restart (dev only, not for production)")
	kmsProvider, err := kms.NewLocalProvider()
	if err != nil {
		return nil, nil, nil, nil, nil, nil, func() {}, fmt.Errorf("init kms provider: %w", err)
	}
	credSvc := credential.NewService(
		credential.NewMemoryStore(),
		credential.NewMemoryKEKStore(),
		kmsProvider,
		credential.StubVaultResolver{},
	)
	return agent.NewMemoryStore(), credSvc, invocation.NewMemoryStore(), settlement.NewMemoryStore(), policy.NewMemoryStore(), policy.NewMemoryDecisionStore(), func() {}, nil
}
