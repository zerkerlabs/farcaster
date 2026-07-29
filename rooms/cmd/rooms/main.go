// Command rooms is the Rooms service: a substrate where agents with persistent
// memory join a shared, membership-scoped space and cowork a task over time.
//
// Rooms is a separate deployable, not part of the gateway. It owns membership,
// shared task state, and orchestration; it deliberately owns no policy engine
// and no payment logic. Agent-to-agent calls are issued through the gateway's
// public proxy API, so they inherit policy enforcement, payment metering, and
// invocation capture rather than reimplementing any of it.
//
// This entrypoint serves the operational routes plus the v1 room API: create
// a room, read it back, seat a member, and post a message.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/gateway"
	"github.com/zerkerlabs/farcaster/rooms/internal/httpapi"
	"github.com/zerkerlabs/farcaster/rooms/internal/memory"
	"github.com/zerkerlabs/farcaster/rooms/internal/room"
	"github.com/zerkerlabs/farcaster/rooms/internal/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		log.Fatal(err)
	}
}

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests to drain once a signal arrives, before giving up.
const shutdownTimeout = 10 * time.Second

// defaultAddr is the listen address when ROOMS_ADDR is unset. Rooms sits
// behind a TLS terminator in production (AGENTS.md invariant #5).
const defaultAddr = ":8090"

func run(logger *slog.Logger) error {
	// SIGINT/SIGTERM cancels ctx — the single shutdown signal.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("ROOMS_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	gwClient, err := gateway.New(gatewayConfigFromEnv(logger))
	if err != nil {
		return fmt.Errorf("init gateway client: %w", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(logger, gwClient),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("rooms: listening", "addr", addr, "version", version.Version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("rooms: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// newMux builds the router. Only the operational routes are unauthenticated
// (AGENTS.md invariant #1). The four room routes are tenant-scoped through
// the tenant context seam (rooms/internal/tenant); bearer-token
// authentication that populates it lands separately.
func newMux(logger *slog.Logger, gwClient httpapi.GatewayCaller) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthz())
	mux.Handle("GET /version", versionHandler())

	// memory.NewFake is a stand-in for the real memory backend, which does not
	// exist yet (rooms/internal/memory); a real client wires in here later
	// without changing the httpapi.Handler seam.
	httpapi.NewHandler(room.NewStore(), memory.NewFake(), gwClient, logger).RegisterRoutes(mux)
	return mux
}

// gatewayConfigFromEnv builds the gateway client's config from environment
// variables. ROOMS_GATEWAY_BASE_URL, ROOMS_GATEWAY_CREDENTIAL, and
// ROOMS_GATEWAY_TENANT are required — gateway.New rejects a config missing any
// of them, since the base URL and credential must come from configuration and
// never be hardcoded (AGENTS.md invariant #4 covers the credential).
//
// ROOMS_GATEWAY_TENANT names the gateway tenant the credential authenticates
// as. The gateway takes the acting tenant from the credential's claims, so one
// credential acts for one tenant, and this deployment can only deliver
// addressed messages for rooms belonging to that tenant — a room from any
// other tenant is refused rather than sent out misattributed. Serving several
// tenants means running a Rooms per tenant.
//
// Two optional durations tune the call, and they bound different things.
// ROOMS_GATEWAY_TIMEOUT bounds a single HTTP request. The proxy is
// asynchronous, though — it returns 202 and runs the call server-side — so
// ROOMS_GATEWAY_CONFIRM_TIMEOUT bounds how long Rooms will poll the resulting
// invocation for a terminal state before reporting the outcome as unknown.
// That is the one to raise for slow recipient agents; raising the request
// timeout would not help. Unset or invalid values fall back to the package
// defaults.
//
// Their sum is what a caller feels: posting an addressed message blocks until
// delivery is confirmed, so it can take ROOMS_GATEWAY_TIMEOUT +
// ROOMS_GATEWAY_CONFIRM_TIMEOUT (90s by default) in the worst case. Anything
// fronting Rooms needs a request timeout above that.
func gatewayConfigFromEnv(logger *slog.Logger) gateway.Config {
	return gateway.Config{
		BaseURL:        os.Getenv("ROOMS_GATEWAY_BASE_URL"),
		Credential:     os.Getenv("ROOMS_GATEWAY_CREDENTIAL"),
		Tenant:         os.Getenv("ROOMS_GATEWAY_TENANT"),
		Timeout:        durationFromEnv(logger, "ROOMS_GATEWAY_TIMEOUT"),
		ConfirmTimeout: durationFromEnv(logger, "ROOMS_GATEWAY_CONFIRM_TIMEOUT"),
	}
}

// durationFromEnv reads an optional duration, returning zero (which the
// gateway package reads as "use the default") when unset or unparseable.
func durationFromEnv(logger *slog.Logger, key string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		logger.Warn("rooms: invalid duration, using default", "var", key, "value", v, "err", err)
		return 0
	}
	return d
}

// healthz reports liveness only: the process answered. Readiness gains meaning
// once Rooms has dependencies to check, and gets added then — reporting "ready"
// while checking nothing would be worse than not reporting it (invariant #9).
func healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

// versionHandler returns deliberate build metadata only — never configuration
// values or internal addresses (invariant #9).
func versionHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"version": version.Version,
			"commit":  version.Commit,
		})
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
