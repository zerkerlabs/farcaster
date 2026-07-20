package invocation

import (
	"testing"
	"time"
)

func i64(v int64) *int64           { return &v }
func ecp(v ErrorClass) *ErrorClass { return &v }

func mkRow(agentID string, created time.Time, status Status, ec *ErrorClass, latency, ttft *int64) *Invocation {
	return &Invocation{
		AgentID:    agentID,
		CreatedAt:  created,
		Status:     status,
		ErrorClass: ec,
		LatencyMS:  latency,
		TTFTMS:     ttft,
	}
}

func TestNearestRank(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		sorted []int64
		p      int
		want   int64
	}{
		{"single element p50", []int64{42}, 50, 42},
		{"single element p99", []int64{42}, 99, 42},
		{"five elements p50", []int64{10, 20, 30, 40, 50}, 50, 30},
		{"five elements p95", []int64{10, 20, 30, 40, 50}, 95, 50},
		{"five elements p99", []int64{10, 20, 30, 40, 50}, 99, 50},
		{"three elements p50", []int64{100, 200, 300}, 50, 200},
		{"three elements p95", []int64{100, 200, 300}, 95, 300},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := nearestRank(tc.sorted, tc.p); got != tc.want {
				t.Errorf("nearestRank(%v, %d) = %d, want %d", tc.sorted, tc.p, got, tc.want)
			}
		})
	}
}

func TestPercentiles_Empty(t *testing.T) {
	t.Parallel()
	p := percentiles(nil)
	if p.P50 != nil || p.P95 != nil || p.P99 != nil {
		t.Errorf("percentiles(nil) = %+v, want all nil", p)
	}
}

func TestPercentiles_SortsInput(t *testing.T) {
	t.Parallel()
	// Unsorted input must still yield correct percentiles.
	p := percentiles([]int64{300, 100, 200})
	if p.P50 == nil || *p.P50 != 200 {
		t.Errorf("p50 = %v, want 200", p.P50)
	}
	if p.P95 == nil || *p.P95 != 300 {
		t.Errorf("p95 = %v, want 300", p.P95)
	}
}

func TestTruncateToBucket(t *testing.T) {
	t.Parallel()
	// 10:30:45 UTC.
	ts := time.Date(2026, 6, 30, 10, 30, 45, 0, time.UTC)
	if got := truncateToBucket(ts, BucketHour); !got.Equal(time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("hour bucket = %v, want 10:00:00Z", got)
	}
	if got := truncateToBucket(ts, BucketDay); !got.Equal(time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day bucket = %v, want 00:00:00Z", got)
	}
	// A non-UTC zone is normalised to UTC before truncation.
	est := time.FixedZone("EST", -5*3600)
	tsEST := time.Date(2026, 6, 30, 23, 30, 0, 0, est) // 2026-07-01 04:30 UTC
	if got := truncateToBucket(tsEST, BucketDay); !got.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day bucket of non-UTC = %v, want 2026-07-01 00:00:00Z", got)
	}
}

func TestAggregateRows(t *testing.T) {
	t.Parallel()

	h10 := time.Date(2026, 6, 30, 10, 30, 0, 0, time.UTC) // hour bucket 10:00
	h11 := time.Date(2026, 6, 30, 11, 5, 0, 0, time.UTC)  // hour bucket 11:00

	rows := []*Invocation{
		// agt_a, 10:00 bucket — 2 succeeded, 1 failed-4xx, 1 in-flight.
		mkRow("agt_a", h10, StatusSucceeded, nil, i64(100), i64(10)),
		mkRow("agt_a", h10, StatusSucceeded, nil, i64(200), i64(30)),
		mkRow("agt_a", h10, StatusFailed, ecp(ErrorClassUpstream4xx), i64(300), nil),
		mkRow("agt_a", h10, StatusRunning, nil, nil, nil), // in-flight: counted, excluded from percentiles
		// agt_a, 11:00 bucket — 1 failed timeout (no latency).
		mkRow("agt_a", h11, StatusFailed, ecp(ErrorClassTimeout), nil, nil),
		// agt_b, 10:00 bucket — 1 succeeded.
		mkRow("agt_b", h10, StatusSucceeded, nil, i64(50), i64(5)),
	}

	got := aggregateRows(rows, BucketHour)

	if len(got) != 3 {
		t.Fatalf("group count = %d, want 3; groups = %+v", len(got), got)
	}

	// Sorted by agent_id then bucket-start: a@10, a@11, b@10.
	a10, a11, b10 := got[0], got[1], got[2]

	if a10.AgentID != "agt_a" || !a10.BucketStart.Equal(time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("group[0] = %s @ %v, want agt_a @ 10:00", a10.AgentID, a10.BucketStart)
	}
	if a10.Count != 4 {
		t.Errorf("a@10 count = %d, want 4", a10.Count)
	}
	if a10.ErrorCount != 1 {
		t.Errorf("a@10 error count = %d, want 1", a10.ErrorCount)
	}
	if a10.ByErrorClass[ErrorClassUpstream4xx] != 1 {
		t.Errorf("a@10 by_error_class[upstream_4xx] = %d, want 1", a10.ByErrorClass[ErrorClassUpstream4xx])
	}
	// Latency percentiles over [100,200,300] (in-flight row's nil latency excluded).
	if a10.LatencyMS.P50 == nil || *a10.LatencyMS.P50 != 200 {
		t.Errorf("a@10 latency p50 = %v, want 200", a10.LatencyMS.P50)
	}
	if a10.LatencyMS.P95 == nil || *a10.LatencyMS.P95 != 300 {
		t.Errorf("a@10 latency p95 = %v, want 300", a10.LatencyMS.P95)
	}
	// TTFT percentiles over [10,30] (only the 2 succeeded streamed-ish rows).
	if a10.TTFTMS.P50 == nil || *a10.TTFTMS.P50 != 10 {
		t.Errorf("a@10 ttft p50 = %v, want 10", a10.TTFTMS.P50)
	}

	// a@11 is a single failed-timeout row: counted, but no latency/ttft samples.
	if a11.Count != 1 || a11.ErrorCount != 1 {
		t.Errorf("a@11 count/error = %d/%d, want 1/1", a11.Count, a11.ErrorCount)
	}
	if a11.ByErrorClass[ErrorClassTimeout] != 1 {
		t.Errorf("a@11 by_error_class[timeout] = %d, want 1", a11.ByErrorClass[ErrorClassTimeout])
	}
	if a11.LatencyMS.P50 != nil {
		t.Errorf("a@11 latency p50 = %v, want nil (no terminal-with-latency rows)", *a11.LatencyMS.P50)
	}

	if b10.AgentID != "agt_b" || b10.Count != 1 || b10.ErrorCount != 0 {
		t.Errorf("b@10 = %s count=%d err=%d, want agt_b 1 0", b10.AgentID, b10.Count, b10.ErrorCount)
	}
	if b10.LatencyMS.P50 == nil || *b10.LatencyMS.P50 != 50 {
		t.Errorf("b@10 latency p50 = %v, want 50", b10.LatencyMS.P50)
	}
}

func TestAggregateRows_DayBucketMergesHours(t *testing.T) {
	t.Parallel()
	rows := []*Invocation{
		mkRow("agt_a", time.Date(2026, 6, 30, 1, 0, 0, 0, time.UTC), StatusSucceeded, nil, i64(10), nil),
		mkRow("agt_a", time.Date(2026, 6, 30, 23, 0, 0, 0, time.UTC), StatusSucceeded, nil, i64(20), nil),
	}
	got := aggregateRows(rows, BucketDay)
	if len(got) != 1 {
		t.Fatalf("day-bucket group count = %d, want 1 (both hours collapse to one day)", len(got))
	}
	if got[0].Count != 2 {
		t.Errorf("count = %d, want 2", got[0].Count)
	}
}

func TestAggregateRows_Empty(t *testing.T) {
	t.Parallel()
	if got := aggregateRows(nil, BucketHour); len(got) != 0 {
		t.Errorf("aggregateRows(nil) = %+v, want empty", got)
	}
}
