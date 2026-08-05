package sweep_test

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/zerkerlabs/gateway/facilitator/internal/chain"
	"github.com/zerkerlabs/gateway/facilitator/internal/settle"
	"github.com/zerkerlabs/gateway/facilitator/internal/settlement"
	"github.com/zerkerlabs/gateway/facilitator/internal/sweep"
	"github.com/zerkerlabs/gateway/x402types"
)

const txHashHex = "0x00000000000000000000000000000000000000000000000000000000000000ab"

// hugeLimit gives claimStaleRow ample daily-ceiling headroom — these tests
// exercise the sweep, not the guardrail ceiling.
var hugeLimit = new(big.Int).Lsh(big.NewInt(1), 128)

// fakeReconciler is a chain-mocked settle.Reconciler: no RPC, just the
// outcome a test wants Reconcile to report.
type fakeReconciler struct {
	tx    common.Hash
	err   error
	calls int32
}

func (f *fakeReconciler) Reconcile(context.Context, x402types.Authorization, time.Time) (common.Hash, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.tx, f.err
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// claimStaleRow inserts an in_flight row on network "base" via Claim — the
// row content a sweep test needs.
func claimStaleRow(t *testing.T, store settlement.Store, nonce string) *settlement.Settlement {
	t.Helper()
	row, claimed, err := store.Claim(context.Background(), settlement.ClaimKey{
		AccountID:   "acct1",
		Nonce:       nonce,
		Payer:       "0x1111111111111111111111111111111111111111",
		PayTo:       "0x2222222222222222222222222222222222222222",
		Network:     "base",
		Asset:       "0x3333333333333333333333333333333333333333",
		Value:       "10000",
		ValidAfter:  "0",
		ValidBefore: "9999999999",
	}, settlement.DailyCeiling{Since: time.Now().Add(-time.Hour), Amount: big.NewInt(10000), Limit: hugeLimit})
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	return row
}

// runOnce drives exactly one sweep pass.
func runOnce(s *sweep.Sweeper) {
	s.SweepOnce(context.Background())
}

func TestSweeper_SweepsStaleRowToSettled(t *testing.T) {
	store := settlement.NewMemoryStore()
	claimStaleRow(t, store, "0xsw1")
	fc := &fakeReconciler{tx: common.HexToHash(txHashHex)}

	// The row's CreatedAt is real "now"; make the clock the sweep uses run
	// far enough ahead that StaleAfter has already elapsed.
	future := time.Now().Add(time.Hour)
	s := sweep.New(sweep.Config{
		Store:       store,
		Reconcilers: map[string]settle.Reconciler{"base": fc},
		StaleAfter:  time.Minute,
		Logger:      discardLogger(),
		Now:         func() time.Time { return future },
	})

	runOnce(s)

	got, err := store.Get(context.Background(), "acct1", "0xsw1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != settlement.StatusSettled || got.TxHash != txHashHex {
		t.Fatalf("row = %+v, want settled with tx %q", got, txHashHex)
	}
	if fc.calls != 1 {
		t.Fatalf("reconciler calls = %d, want 1", fc.calls)
	}
}

func TestSweeper_SweepsStaleRowToFailed(t *testing.T) {
	store := settlement.NewMemoryStore()
	claimStaleRow(t, store, "0xsw2")
	fc := &fakeReconciler{err: chain.ErrAuthorizationNotSettled}

	future := time.Now().Add(time.Hour)
	s := sweep.New(sweep.Config{
		Store:       store,
		Reconcilers: map[string]settle.Reconciler{"base": fc},
		StaleAfter:  time.Minute,
		Logger:      discardLogger(),
		Now:         func() time.Time { return future },
	})

	runOnce(s)

	got, err := store.Get(context.Background(), "acct1", "0xsw2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != settlement.StatusFailed || got.ErrorReason == "" {
		t.Fatalf("row = %+v, want failed with a coarse reason", got)
	}
}

func TestSweeper_NotYetStaleRowLeftUntouched(t *testing.T) {
	store := settlement.NewMemoryStore()
	claimStaleRow(t, store, "0xsw3")
	fc := &fakeReconciler{tx: common.HexToHash(txHashHex)}

	s := sweep.New(sweep.Config{
		Store:       store,
		Reconcilers: map[string]settle.Reconciler{"base": fc},
		StaleAfter:  time.Hour, // the row (CreatedAt ~= now) is nowhere near this stale
		Logger:      discardLogger(),
		Now:         time.Now,
	})

	runOnce(s)

	got, err := store.Get(context.Background(), "acct1", "0xsw3")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != settlement.StatusInFlight {
		t.Fatalf("status = %q, want in_flight (row is not yet stale)", got.Status)
	}
	if fc.calls != 0 {
		t.Fatalf("reconciler calls = %d, want 0", fc.calls)
	}
}

func TestSweeper_IndeterminateRowLeftInFlight(t *testing.T) {
	store := settlement.NewMemoryStore()
	claimStaleRow(t, store, "0xsw4")
	fc := &fakeReconciler{err: chain.ErrReconcileIndeterminate}

	future := time.Now().Add(time.Hour)
	s := sweep.New(sweep.Config{
		Store:       store,
		Reconcilers: map[string]settle.Reconciler{"base": fc},
		StaleAfter:  time.Minute,
		Logger:      discardLogger(),
		Now:         func() time.Time { return future },
	})

	runOnce(s)

	got, err := store.Get(context.Background(), "acct1", "0xsw4")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != settlement.StatusInFlight {
		t.Fatalf("status = %q, want in_flight (chain outcome indeterminate)", got.Status)
	}
}

func TestSweeper_UnconfiguredNetworkLeftUntouched(t *testing.T) {
	store := settlement.NewMemoryStore()
	claimStaleRow(t, store, "0xsw5")

	future := time.Now().Add(time.Hour)
	s := sweep.New(sweep.Config{
		Store:       store,
		Reconcilers: map[string]settle.Reconciler{}, // no reconciler for "base"
		StaleAfter:  time.Minute,
		Logger:      discardLogger(),
		Now:         func() time.Time { return future },
	})

	runOnce(s)

	got, err := store.Get(context.Background(), "acct1", "0xsw5")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != settlement.StatusInFlight {
		t.Fatalf("status = %q, want in_flight (no reconciler configured)", got.Status)
	}
}

// TestSweeper_RunStopsOnContextCancel is the goroutine-leak guard: Run must
// return once its context is canceled, even while blocked on the ticker.
func TestSweeper_RunStopsOnContextCancel(t *testing.T) {
	store := settlement.NewMemoryStore()
	s := sweep.New(sweep.Config{
		Store:       store,
		Reconcilers: map[string]settle.Reconciler{},
		Interval:    time.Hour, // long enough that only cancellation ends Run
		Logger:      discardLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of context cancellation (goroutine leak)")
	}
}

// TestSweeper_ConcurrentSweepAndOnReadDoNotCorruptRow drives the same stale
// row through settle.ReconcileRow concurrently from many callers — modeling a
// sweep pass racing the on-read path, or multiple unreplicated sweeps racing
// each other (spec 0007 follow-up #197 acceptance): the store's in_flight-only
// transition guard (#176) must let exactly one caller's transition win and
// leave the row in a single consistent terminal state, never torn or
// double-applied.
func TestSweeper_ConcurrentSweepAndOnReadDoNotCorruptRow(t *testing.T) {
	store := settlement.NewMemoryStore()
	row := claimStaleRow(t, store, "0xsw6")
	fc := &fakeReconciler{tx: common.HexToHash(txHashHex)}

	var wg sync.WaitGroup
	const n = 8
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			settle.ReconcileRow(context.Background(), store, fc, row, time.Now().Add(time.Hour), discardLogger())
		}()
	}
	wg.Wait()

	got, err := store.Get(context.Background(), "acct1", "0xsw6")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != settlement.StatusSettled || got.TxHash != txHashHex {
		t.Fatalf("row = %+v, want a single consistent settled/%s outcome", got, txHashHex)
	}
}
