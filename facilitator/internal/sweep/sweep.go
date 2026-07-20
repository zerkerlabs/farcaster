// Package sweep runs the facilitator's periodic reconciliation loop (spec 0007
// fast-follow #197, following #179/PR #196). #196 added chain-read
// reconciliation of stuck in_flight settlement rows, but it only fires
// on-read — when a duplicate /settle request happens to land on a stale row.
// A row from the post-MarkSettled-failure scenario (the on-chain transfer
// landed, but the store write recording it failed) is only self-healing if a
// later duplicate arrives, which is not guaranteed. Sweeper closes that gap:
// on a fixed interval it finds every in_flight row older than StaleAfter and
// drives each through settle.ReconcileRow — the same #196 primitive the
// on-read path uses — to its true terminal state, so recovery never depends
// on a duplicate request.
//
// Multiple replicas may run this loop with no coordination. The settlement
// store's in_flight-only transition guard (#176, settlement.Store's
// MarkSettled/MarkFailed) makes a redundant concurrent sweep safe: at worst
// two replicas both issue the same read-only chain check on the same row, and
// only one of the resulting store transitions wins — the other returns
// settlement.ErrInvalidTransition, which settle.ReconcileRow logs and
// discards, never surfaced. Leader election / single-flight across replicas
// is explicitly out of scope for #197 (wasteful, not unsafe); revisit only if
// the redundant chain-read load becomes a real cost.
package sweep

import (
	"context"
	"log/slog"
	"time"

	"github.com/zerkerlabs/farcaster/facilitator/internal/settle"
	"github.com/zerkerlabs/farcaster/facilitator/internal/settlement"
)

// DefaultInterval is Config.Interval's default: comfortably below
// settle.DefaultStaleAfter (2 minutes) so a row that just went stale is
// picked up well within one more interval, without polling the store and
// chain harder than the money path needs.
const DefaultInterval = 30 * time.Second

// Config is the Sweeper's injected dependencies.
type Config struct {
	// Store is the settlement store swept for stale in_flight rows and
	// transitioned via settle.ReconcileRow.
	Store settlement.Store

	// Reconcilers maps a network name to its chain reconciler, mirroring
	// settle.Config.Reconcilers — in production the same map, since it's the
	// same chain.Client per network satisfying both settle.Submitter and
	// settle.Reconciler. A stale row on a network with no configured
	// reconciler is left in_flight; a later sweep pass or the on-read path
	// picks it up if a reconciler is ever configured.
	Reconcilers map[string]settle.Reconciler

	// StaleAfter is how old an in_flight row (by CreatedAt) must be before
	// the sweep attempts to reconcile it. Should match the Handler's own
	// StaleAfter so both paths agree on what "stale" means; defaults to
	// settle.DefaultStaleAfter.
	StaleAfter time.Duration

	// Interval is how often the sweep runs. Defaults to DefaultInterval.
	Interval time.Duration

	Logger *slog.Logger

	// Now defaults to time.Now; injectable for deterministic tests.
	Now func() time.Time
}

// Sweeper periodically reconciles stale in_flight settlement rows. Construct
// with New and run with Run.
type Sweeper struct {
	store       settlement.Store
	reconcilers map[string]settle.Reconciler
	staleAfter  time.Duration
	interval    time.Duration
	logger      *slog.Logger
	now         func() time.Time
}

// New builds a Sweeper from cfg, filling defaults.
func New(cfg Config) *Sweeper {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = settle.DefaultStaleAfter
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	return &Sweeper{
		store:       cfg.Store,
		reconcilers: cfg.Reconcilers,
		staleAfter:  cfg.StaleAfter,
		interval:    cfg.Interval,
		logger:      cfg.Logger,
		now:         cfg.Now,
	}
}

// Run sweeps immediately, then every Interval, until ctx is canceled — the
// caller's graceful-shutdown signal (spec 0007 follow-up #197: the loop must
// start and stop cleanly with the facilitator process). It always returns
// once ctx is done, so the caller can wait on it (e.g. sync.WaitGroup) to
// drain the loop without leaking the goroutine it runs in.
func (s *Sweeper) Run(ctx context.Context) {
	s.SweepOnce(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SweepOnce(ctx)
		}
	}
}

// SweepOnce finds every in_flight row older than StaleAfter and drives each
// through the #196 reconcile primitive (settle.ReconcileRow). A row whose
// network has no configured reconciler, or whose outcome the chain read
// can't yet determine, is left in_flight for a later pass. Run calls this on
// its own schedule; it is exported so a test (or an operator's ad hoc trigger)
// can run a single pass synchronously without waiting on the ticker.
func (s *Sweeper) SweepOnce(ctx context.Context) {
	rows, err := s.store.StaleInFlight(ctx, s.now().Add(-s.staleAfter))
	if err != nil {
		s.logger.Error("sweep: list stale in_flight rows failed", "err", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		reconciler, ok := s.reconcilers[row.Network]
		if !ok {
			continue
		}
		settle.ReconcileRow(ctx, s.store, reconciler, row, s.now(), s.logger)
	}
}
