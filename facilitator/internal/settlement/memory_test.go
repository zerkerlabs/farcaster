package settlement_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerkerlabs/gateway/facilitator/internal/guardrail"
	"github.com/zerkerlabs/gateway/facilitator/internal/settlement"
)

func sampleKey(accountID, nonce string) settlement.ClaimKey {
	return sampleKeyValue(accountID, nonce, "10000")
}

func sampleKeyValue(accountID, nonce, value string) settlement.ClaimKey {
	return settlement.ClaimKey{
		AccountID: accountID,
		Nonce:     nonce,
		Payer:     "0xPayer",
		PayTo:     "0xOperator",
		Network:   "base",
		Asset:     "0xUSDC",
		Value:     value,
	}
}

// farPastSince and hugeLimit give claimNoCeiling ample headroom, so tests
// unrelated to the daily ceiling don't need to think about it. Ceiling-
// specific tests build their own settlement.DailyCeiling to exercise
// at-limit/over-limit/concurrent behavior.
var (
	farPastSince = time.Now().Add(-365 * 24 * time.Hour)
	hugeLimit    = new(big.Int).Lsh(big.NewInt(1), 128)
)

// claimNoCeiling calls Claim with a DailyCeiling that never rejects, for
// tests exercising idempotency/replay/mark behavior unrelated to guardrails.
func claimNoCeiling(ctx context.Context, s settlement.Store, key settlement.ClaimKey) (*settlement.Settlement, bool, error) {
	amount, ok := new(big.Int).SetString(key.Value, 10)
	if !ok {
		amount = big.NewInt(0)
	}
	return s.Claim(ctx, key, settlement.DailyCeiling{Since: farPastSince, Amount: amount, Limit: hugeLimit})
}

// storeFactory lets the same behavioral suite run against MemoryStore here and
// the Postgres store in the integration test.
func testStoreBehavior(t *testing.T, newStore func(t *testing.T) settlement.Store) {
	ctx := context.Background()

	t.Run("claim new is claimed and in-flight", func(t *testing.T) {
		s := newStore(t)
		row, claimed, err := claimNoCeiling(ctx, s, sampleKey("acct1", "0xaa"))
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if !claimed {
			t.Fatal("first claim should be claimed=true")
		}
		if row.Status != settlement.StatusInFlight {
			t.Fatalf("status = %q, want in_flight", row.Status)
		}
		if row.ID == "" {
			t.Fatal("expected an id")
		}
	})

	t.Run("duplicate claim returns existing row, not claimed", func(t *testing.T) {
		s := newStore(t)
		first, _, err := claimNoCeiling(ctx, s, sampleKey("acct1", "0xbb"))
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		second, claimed, err := claimNoCeiling(ctx, s, sampleKey("acct1", "0xbb"))
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if claimed {
			t.Fatal("second claim on same key must be claimed=false")
		}
		if second.ID != first.ID {
			t.Fatalf("duplicate claim returned id %q, want %q", second.ID, first.ID)
		}
	})

	t.Run("settled nonce replays the same tx hash", func(t *testing.T) {
		s := newStore(t)
		row, _, err := claimNoCeiling(ctx, s, sampleKey("acct1", "0xcc"))
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if _, err := s.MarkSettled(ctx, row.ID, "0xdeadbeef"); err != nil {
			t.Fatalf("MarkSettled: %v", err)
		}
		dup, claimed, err := claimNoCeiling(ctx, s, sampleKey("acct1", "0xcc"))
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if claimed {
			t.Fatal("claim on a settled nonce must be claimed=false")
		}
		if dup.Status != settlement.StatusSettled || dup.TxHash != "0xdeadbeef" {
			t.Fatalf("replay = {%s %s}, want {settled 0xdeadbeef}", dup.Status, dup.TxHash)
		}
	})

	t.Run("mark failed sets coarse reason", func(t *testing.T) {
		s := newStore(t)
		row, _, _ := claimNoCeiling(ctx, s, sampleKey("acct1", "0xdd"))
		got, err := s.MarkFailed(ctx, row.ID, "payment could not be collected", "")
		if err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		if got.Status != settlement.StatusFailed || got.ErrorReason == "" {
			t.Fatalf("got {%s %q}, want failed with a reason", got.Status, got.ErrorReason)
		}
	})

	t.Run("mark unknown id is not found", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.MarkSettled(ctx, "facset_missing", "0x1"); !errors.Is(err, settlement.ErrNotFound) {
			t.Fatalf("MarkSettled err = %v, want ErrNotFound", err)
		}
		if _, err := s.MarkFailed(ctx, "facset_missing", "reason", ""); !errors.Is(err, settlement.ErrNotFound) {
			t.Fatalf("MarkFailed err = %v, want ErrNotFound", err)
		}
	})

	t.Run("re-marking an already-settled row with the same tx hash is idempotent", func(t *testing.T) {
		s := newStore(t)
		row, _, _ := claimNoCeiling(ctx, s, sampleKey("acct1", "0xidem-settled"))
		first, err := s.MarkSettled(ctx, row.ID, "0xdeadbeef")
		if err != nil {
			t.Fatalf("MarkSettled: %v", err)
		}
		again, err := s.MarkSettled(ctx, row.ID, "0xdeadbeef")
		if err != nil {
			t.Fatalf("re-mark with same tx hash: %v", err)
		}
		if again.Status != settlement.StatusSettled || again.TxHash != first.TxHash {
			t.Fatalf("re-mark = %+v, want unchanged %+v", again, first)
		}
	})

	t.Run("re-marking an already-settled row with a different tx hash is an error and mutates nothing", func(t *testing.T) {
		s := newStore(t)
		row, _, _ := claimNoCeiling(ctx, s, sampleKey("acct1", "0xidem-settled-diff"))
		if _, err := s.MarkSettled(ctx, row.ID, "0xdeadbeef"); err != nil {
			t.Fatalf("MarkSettled: %v", err)
		}
		if _, err := s.MarkSettled(ctx, row.ID, "0xdifferent"); !errors.Is(err, settlement.ErrInvalidTransition) {
			t.Fatalf("err = %v, want ErrInvalidTransition", err)
		}
		got, err := s.Get(ctx, "acct1", "0xidem-settled-diff")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.TxHash != "0xdeadbeef" {
			t.Fatalf("tx hash = %q, want unchanged 0xdeadbeef (rejected mark must not clobber it)", got.TxHash)
		}
	})

	t.Run("MarkFailed on an already-settled row is an illegal transition and mutates nothing", func(t *testing.T) {
		s := newStore(t)
		row, _, _ := claimNoCeiling(ctx, s, sampleKey("acct1", "0xillegal-fail-on-settled"))
		if _, err := s.MarkSettled(ctx, row.ID, "0xdeadbeef"); err != nil {
			t.Fatalf("MarkSettled: %v", err)
		}
		if _, err := s.MarkFailed(ctx, row.ID, "payment could not be collected", ""); !errors.Is(err, settlement.ErrInvalidTransition) {
			t.Fatalf("err = %v, want ErrInvalidTransition", err)
		}
		got, err := s.Get(ctx, "acct1", "0xillegal-fail-on-settled")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != settlement.StatusSettled || got.TxHash != "0xdeadbeef" {
			t.Fatalf("row = %+v, want unchanged settled/0xdeadbeef", got)
		}
	})

	t.Run("MarkSettled on an already-failed row is an illegal transition and mutates nothing", func(t *testing.T) {
		s := newStore(t)
		row, _, _ := claimNoCeiling(ctx, s, sampleKey("acct1", "0xillegal-settle-on-failed"))
		if _, err := s.MarkFailed(ctx, row.ID, "payment could not be collected", ""); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		if _, err := s.MarkSettled(ctx, row.ID, "0xshouldnotstick"); !errors.Is(err, settlement.ErrInvalidTransition) {
			t.Fatalf("err = %v, want ErrInvalidTransition", err)
		}
		got, err := s.Get(ctx, "acct1", "0xillegal-settle-on-failed")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != settlement.StatusFailed || got.TxHash != "" {
			t.Fatalf("row = %+v, want unchanged failed with no tx hash", got)
		}
	})

	t.Run("re-marking an already-failed row with the same reason and tx hash is idempotent", func(t *testing.T) {
		s := newStore(t)
		row, _, _ := claimNoCeiling(ctx, s, sampleKey("acct1", "0xidem-failed"))
		if _, err := s.MarkFailed(ctx, row.ID, "payment could not be collected", "0xreverted"); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		again, err := s.MarkFailed(ctx, row.ID, "payment could not be collected", "0xreverted")
		if err != nil {
			t.Fatalf("re-mark with same reason/tx hash: %v", err)
		}
		if again.Status != settlement.StatusFailed || again.TxHash != "0xreverted" {
			t.Fatalf("re-mark = %+v, want unchanged failed/0xreverted", again)
		}
	})

	t.Run("re-marking an already-failed row with a different tx hash is an error and mutates nothing", func(t *testing.T) {
		s := newStore(t)
		row, _, _ := claimNoCeiling(ctx, s, sampleKey("acct1", "0xidem-failed-diff"))
		if _, err := s.MarkFailed(ctx, row.ID, "payment could not be collected", "0xreverted"); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		if _, err := s.MarkFailed(ctx, row.ID, "payment could not be collected", "0xother"); !errors.Is(err, settlement.ErrInvalidTransition) {
			t.Fatalf("err = %v, want ErrInvalidTransition", err)
		}
		got, err := s.Get(ctx, "acct1", "0xidem-failed-diff")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.TxHash != "0xreverted" {
			t.Fatalf("tx hash = %q, want unchanged 0xreverted (rejected mark must not clobber it)", got.TxHash)
		}
	})

	t.Run("per-account isolation: same nonce, different accounts", func(t *testing.T) {
		s := newStore(t)
		a, claimedA, _ := claimNoCeiling(ctx, s, sampleKey("acctA", "0xshared"))
		b, claimedB, _ := claimNoCeiling(ctx, s, sampleKey("acctB", "0xshared"))
		if !claimedA || !claimedB {
			t.Fatal("same nonce under different accounts must both claim")
		}
		if a.ID == b.ID {
			t.Fatal("distinct accounts must get distinct rows")
		}
		// Each account sees only its own row.
		listA, err := s.List(ctx, "acctA")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(listA) != 1 || listA[0].AccountID != "acctA" {
			t.Fatalf("acctA list = %+v, want exactly its own row", listA)
		}
		if _, err := s.Get(ctx, "acctA", "0xshared"); err != nil {
			t.Fatalf("Get own: %v", err)
		}
	})

	t.Run("get unknown is not found", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Get(ctx, "acct1", "0xnope"); !errors.Is(err, settlement.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("sum settled totals only settled rows for the account since the window", func(t *testing.T) {
		s := newStore(t)
		since := time.Now().Add(-time.Hour)

		row1, _, _ := claimNoCeiling(ctx, s, sampleKeyValue("acctSum", "0x01", "1000"))
		if _, err := s.MarkSettled(ctx, row1.ID, "0xtx1"); err != nil {
			t.Fatalf("MarkSettled: %v", err)
		}
		row2, _, _ := claimNoCeiling(ctx, s, sampleKeyValue("acctSum", "0x02", "2500"))
		if _, err := s.MarkSettled(ctx, row2.ID, "0xtx2"); err != nil {
			t.Fatalf("MarkSettled: %v", err)
		}
		// A failed row must not count.
		row3, _, _ := claimNoCeiling(ctx, s, sampleKeyValue("acctSum", "0x03", "9999"))
		if _, err := s.MarkFailed(ctx, row3.ID, "payment could not be collected", ""); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		// An in-flight row must not count.
		if _, _, err := claimNoCeiling(ctx, s, sampleKeyValue("acctSum", "0x04", "5000")); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		// A different account's settled row must not count.
		otherRow, _, _ := claimNoCeiling(ctx, s, sampleKeyValue("acctOther", "0x05", "7000"))
		if _, err := s.MarkSettled(ctx, otherRow.ID, "0xtx5"); err != nil {
			t.Fatalf("MarkSettled: %v", err)
		}

		sum, err := s.SumSettled(ctx, "acctSum", since)
		if err != nil {
			t.Fatalf("SumSettled: %v", err)
		}
		if sum.String() != "3500" {
			t.Fatalf("sum = %s, want 3500", sum.String())
		}
	})

	t.Run("sum settled excludes rows created before the window", func(t *testing.T) {
		s := newStore(t)
		row, _, _ := claimNoCeiling(ctx, s, sampleKeyValue("acctWindow", "0x10", "1000"))
		if _, err := s.MarkSettled(ctx, row.ID, "0xtx"); err != nil {
			t.Fatalf("MarkSettled: %v", err)
		}

		future := time.Now().Add(time.Hour)
		sum, err := s.SumSettled(ctx, "acctWindow", future)
		if err != nil {
			t.Fatalf("SumSettled: %v", err)
		}
		if sum.Sign() != 0 {
			t.Fatalf("sum = %s, want 0 (row created before the window)", sum.String())
		}
	})

	t.Run("sum settled for an unknown account is zero", func(t *testing.T) {
		s := newStore(t)
		sum, err := s.SumSettled(ctx, "acctNoRows", time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("SumSettled: %v", err)
		}
		if sum.Sign() != 0 {
			t.Fatalf("sum = %s, want 0", sum.String())
		}
	})

	t.Run("concurrent claims single-flight to exactly one winner", func(t *testing.T) {
		s := newStore(t)
		const n = 32
		var wins int64
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				_, claimed, err := claimNoCeiling(ctx, s, sampleKey("acctRace", "0xrace"))
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if claimed {
					atomic.AddInt64(&wins, 1)
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("claimed=true count = %d, want exactly 1 (single-flight)", wins)
		}
	})

	// The race this closes: concurrent settle requests for the same account
	// use distinct, attacker-choosable nonces, so the nonce single-flight test
	// above does not exercise it. If the ceiling check merely read a snapshot
	// of the already-settled total before any claim committed, every one of
	// these could individually pass the check and collectively blow past the
	// ceiling. Claim must serialize the check against every concurrent claim
	// for the same account so exactly the number that fit are admitted.
	t.Run("concurrent claims near the ceiling admit exactly the number that fit", func(t *testing.T) {
		s := newStore(t)
		const perClaim = 1000
		const fits = 5
		const n = 20 // 4x the number that fit, to make the race easy to hit
		limit := big.NewInt(perClaim * fits)
		since := time.Now().Add(-time.Hour)

		var wins int64
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			i := i
			go func() {
				defer wg.Done()
				key := sampleKeyValue("acctCeiling", fmt.Sprintf("0xceil%02d", i), strconv.Itoa(perClaim))
				_, claimed, err := s.Claim(ctx, key, settlement.DailyCeiling{
					Since: since, Amount: big.NewInt(perClaim), Limit: limit,
				})
				if err != nil && !errors.Is(err, guardrail.ErrDailyCeilingExceeded) {
					t.Errorf("Claim: %v", err)
					return
				}
				if claimed {
					atomic.AddInt64(&wins, 1)
				}
			}()
		}
		wg.Wait()
		if wins != fits {
			t.Fatalf("claimed count = %d, want exactly %d (a %d limit at %d per claim)", wins, fits, perClaim*fits, perClaim)
		}
	})

	t.Run("claim over the ceiling is rejected and inserts no row", func(t *testing.T) {
		s := newStore(t)
		since := time.Now().Add(-time.Hour)
		_, claimed, err := s.Claim(ctx, sampleKeyValue("acctOverCeiling", "0xover", "1000"), settlement.DailyCeiling{
			Since: since, Amount: big.NewInt(1000), Limit: big.NewInt(999),
		})
		if !errors.Is(err, guardrail.ErrDailyCeilingExceeded) {
			t.Fatalf("err = %v, want ErrDailyCeilingExceeded", err)
		}
		if claimed {
			t.Fatal("claimed = true, want false when the ceiling is exceeded")
		}
		if _, err := s.Get(ctx, "acctOverCeiling", "0xover"); !errors.Is(err, settlement.ErrNotFound) {
			t.Fatalf("Get after a ceiling rejection = %v, want ErrNotFound (no row inserted)", err)
		}
	})

	t.Run("retrying an already-claimed nonce replays the existing row even at the ceiling", func(t *testing.T) {
		s := newStore(t)
		since := time.Now().Add(-time.Hour)
		ceiling := settlement.DailyCeiling{Since: since, Amount: big.NewInt(1000), Limit: big.NewInt(1000)}

		first, claimed, err := s.Claim(ctx, sampleKeyValue("acctRetry", "0xretry", "1000"), ceiling)
		if err != nil || !claimed {
			t.Fatalf("first claim: claimed=%v err=%v, want true, nil", claimed, err)
		}

		// A retry of the same (account, nonce) request must replay the
		// existing row, not re-run the ceiling check: committed (1000) plus
		// the same request's amount (1000) would exceed the 1000 limit if
		// double-counted.
		retry, claimed, err := s.Claim(ctx, sampleKeyValue("acctRetry", "0xretry", "1000"), ceiling)
		if err != nil {
			t.Fatalf("retry claim: %v", err)
		}
		if claimed {
			t.Fatal("retry claim: claimed = true, want false (replay, not a new claim)")
		}
		if retry.ID != first.ID {
			t.Fatalf("retry claim returned id %q, want %q (the original row)", retry.ID, first.ID)
		}
	})

	t.Run("stale in-flight returns rows older than the cutoff and excludes fresh or settled ones", func(t *testing.T) {
		s := newStore(t)

		stale, _, err := claimNoCeiling(ctx, s, sampleKey("acctStale", "0xstale"))
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}

		// cutoff sits strictly between "stale" (claimed above) and "fresh"/
		// "settled" (claimed below), so StaleInFlight(cutoff) can only match
		// the first row.
		time.Sleep(2 * time.Millisecond)
		cutoff := time.Now()
		time.Sleep(2 * time.Millisecond)

		fresh, _, err := claimNoCeiling(ctx, s, sampleKey("acctStale", "0xfresh"))
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		settled, _, err := claimNoCeiling(ctx, s, sampleKey("acctStale", "0xsettled"))
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if _, err := s.MarkSettled(ctx, settled.ID, "0xdeadbeef"); err != nil {
			t.Fatalf("MarkSettled: %v", err)
		}

		got, err := s.StaleInFlight(ctx, cutoff)
		if err != nil {
			t.Fatalf("StaleInFlight: %v", err)
		}
		if len(got) != 1 || got[0].ID != stale.ID {
			t.Fatalf("StaleInFlight(cutoff) = %+v, want exactly [%s]", got, stale.ID)
		}

		// A cutoff after every claim above must exclude the settled row
		// regardless of age, while still catching the (now also stale) fresh
		// row.
		all, err := s.StaleInFlight(ctx, time.Now())
		if err != nil {
			t.Fatalf("StaleInFlight: %v", err)
		}
		gotIDs := map[string]bool{}
		for _, row := range all {
			gotIDs[row.ID] = true
		}
		if !gotIDs[stale.ID] || !gotIDs[fresh.ID] {
			t.Fatalf("StaleInFlight(now) = %+v, want both %s and %s", all, stale.ID, fresh.ID)
		}
		if gotIDs[settled.ID] {
			t.Fatalf("StaleInFlight(now) = %+v, must not include settled row %s", all, settled.ID)
		}
	})

	t.Run("an in-flight (not yet settled) claim counts toward the ceiling", func(t *testing.T) {
		s := newStore(t)
		since := time.Now().Add(-time.Hour)
		limit := big.NewInt(1500)

		// First claim is in-flight (never marked settled or failed) and alone
		// is within the 1500 ceiling.
		first, claimed, err := s.Claim(ctx, sampleKeyValue("acctInFlight", "0xif1", "1000"), settlement.DailyCeiling{
			Since: since, Amount: big.NewInt(1000), Limit: limit,
		})
		if err != nil || !claimed {
			t.Fatalf("first claim: claimed=%v err=%v, want true, nil", claimed, err)
		}
		if first.Status != settlement.StatusInFlight {
			t.Fatalf("first status = %q, want in_flight", first.Status)
		}

		// A second, distinct-nonce claim of 1000 would bring the committed
		// total to 2000 > 1500 even though the first has not settled yet.
		_, claimed, err = s.Claim(ctx, sampleKeyValue("acctInFlight", "0xif2", "1000"), settlement.DailyCeiling{
			Since: since, Amount: big.NewInt(1000), Limit: limit,
		})
		if !errors.Is(err, guardrail.ErrDailyCeilingExceeded) {
			t.Fatalf("second claim err = %v, want ErrDailyCeilingExceeded (in-flight amount must count)", err)
		}
		if claimed {
			t.Fatal("second claim: claimed = true, want false")
		}
	})
}

func TestMemoryStore(t *testing.T) {
	testStoreBehavior(t, func(*testing.T) settlement.Store {
		return settlement.NewMemoryStore()
	})
}
