package guardrail_test

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/facilitator/internal/guardrail"
)

func conservativeLimits() guardrail.Limits {
	return guardrail.Limits{
		AllowedKinds:    []guardrail.Kind{{Network: "base", Asset: "0xUSDC"}},
		MaxSettleAmount: big.NewInt(10_000),
		DailyCeiling:    big.NewInt(20_000),
	}
}

func TestCheck_KindNotAllowed(t *testing.T) {
	t.Parallel()

	err := guardrail.Check(conservativeLimits(), "base-sepolia", "0xUSDC", big.NewInt(1), big.NewInt(0))
	if !errors.Is(err, guardrail.ErrKindNotAllowed) {
		t.Fatalf("err = %v, want ErrKindNotAllowed", err)
	}
}

func TestCheck_KindAllowedIsCaseInsensitiveOnAsset(t *testing.T) {
	t.Parallel()

	limits := guardrail.Limits{
		AllowedKinds:    []guardrail.Kind{{Network: "base", Asset: "0xAbCd"}},
		MaxSettleAmount: big.NewInt(10),
		DailyCeiling:    big.NewInt(10),
	}
	if err := guardrail.Check(limits, "base", "0xabcd", big.NewInt(1), big.NewInt(0)); err != nil {
		t.Fatalf("Check() = %v, want nil (case-insensitive asset match)", err)
	}
}

func TestCheck_AmountAtMaxSucceeds(t *testing.T) {
	t.Parallel()

	err := guardrail.Check(conservativeLimits(), "base", "0xUSDC", big.NewInt(10_000), big.NewInt(0))
	if err != nil {
		t.Fatalf("Check() = %v, want nil at exactly the per-tx max", err)
	}
}

func TestCheck_AmountOverMaxRejected(t *testing.T) {
	t.Parallel()

	err := guardrail.Check(conservativeLimits(), "base", "0xUSDC", big.NewInt(10_001), big.NewInt(0))
	if !errors.Is(err, guardrail.ErrAmountExceedsMax) {
		t.Fatalf("err = %v, want ErrAmountExceedsMax", err)
	}
}

func TestCheck_DailyCeilingAtLimitSucceeds(t *testing.T) {
	t.Parallel()

	// priorToday (10_000) + amount (10_000) == DailyCeiling (20_000).
	err := guardrail.Check(conservativeLimits(), "base", "0xUSDC", big.NewInt(10_000), big.NewInt(10_000))
	if err != nil {
		t.Fatalf("Check() = %v, want nil at exactly the daily ceiling", err)
	}
}

func TestCheck_DailyCeilingOverLimitRejected(t *testing.T) {
	t.Parallel()

	// priorToday (10_001) + amount (10_000) exceeds DailyCeiling (20_000).
	err := guardrail.Check(conservativeLimits(), "base", "0xUSDC", big.NewInt(10_000), big.NewInt(10_001))
	if !errors.Is(err, guardrail.ErrDailyCeilingExceeded) {
		t.Fatalf("err = %v, want ErrDailyCeilingExceeded", err)
	}
}

// The kind check runs before the amount checks, so an out-of-policy request
// is rejected on that basis even if the amount would also violate a limit —
// callers only need the first rejection, and this pins the check order.
func TestCheck_KindCheckedBeforeAmount(t *testing.T) {
	t.Parallel()

	err := guardrail.Check(conservativeLimits(), "ethereum", "0xUSDC", big.NewInt(999_999), big.NewInt(0))
	if !errors.Is(err, guardrail.ErrKindNotAllowed) {
		t.Fatalf("err = %v, want ErrKindNotAllowed", err)
	}
}

func TestCheck_EmptyAllowedKindsDeniesEverything(t *testing.T) {
	t.Parallel()

	limits := guardrail.Limits{MaxSettleAmount: big.NewInt(1), DailyCeiling: big.NewInt(1)}
	if err := guardrail.Check(limits, "base", "0xUSDC", big.NewInt(0), big.NewInt(0)); !errors.Is(err, guardrail.ErrKindNotAllowed) {
		t.Fatalf("err = %v, want ErrKindNotAllowed for an empty allow-list", err)
	}
}

func TestValidate_SingleAssetAcrossNetworksSucceeds(t *testing.T) {
	t.Parallel()

	limits := guardrail.Limits{
		AllowedKinds: []guardrail.Kind{
			{Network: "base", Asset: "0xUSDC"},
			{Network: "base-sepolia", Asset: "0xusdc"},
		},
	}
	if err := limits.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a single asset across networks (case-insensitive)", err)
	}
}

func TestValidate_EmptyOrSingleKindSucceeds(t *testing.T) {
	t.Parallel()

	if err := (guardrail.Limits{}).Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for an empty AllowedKinds", err)
	}
	if err := conservativeLimits().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a single-kind policy", err)
	}
}

func TestValidate_MixedAssetKindsRejected(t *testing.T) {
	t.Parallel()

	limits := guardrail.Limits{
		AllowedKinds: []guardrail.Kind{
			{Network: "base", Asset: "0xUSDC"},
			{Network: "base", Asset: "0xDAI"},
		},
	}
	if err := limits.Validate(); !errors.Is(err, guardrail.ErrMixedAssetKinds) {
		t.Fatalf("Validate() = %v, want ErrMixedAssetKinds", err)
	}
}

func TestStartOfUTCDay(t *testing.T) {
	t.Parallel()

	in := time.Date(2026, time.July, 10, 23, 59, 59, 0, time.FixedZone("PST", -8*60*60))
	got := guardrail.StartOfUTCDay(in)
	// 2026-07-10 23:59:59-08:00 is 2026-07-11 07:59:59 UTC.
	want := time.Date(2026, time.July, 11, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("StartOfUTCDay(%v) = %v, want %v", in, got, want)
	}
}
