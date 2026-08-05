package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/gateway/gateway/internal/httpapi"
	"github.com/zerkerlabs/gateway/gateway/internal/invocation"
)

// stubCallerLimiter is a CallerRateLimiter test double returning a fixed delay.
type stubCallerLimiter struct{ delay time.Duration }

func (s stubCallerLimiter) Allow(string) time.Duration { return s.delay }

type apiPercentiles struct {
	P50 *int64 `json:"p50"`
	P95 *int64 `json:"p95"`
	P99 *int64 `json:"p99"`
}

type apiAnalyticsGroup struct {
	AgentID      string         `json:"agent_id"`
	BucketStart  time.Time      `json:"bucket_start"`
	Count        int            `json:"count"`
	ErrorRate    float64        `json:"error_rate"`
	ByErrorClass map[string]int `json:"by_error_class"`
	LatencyMS    apiPercentiles `json:"latency_ms"`
	TTFTMS       apiPercentiles `json:"ttft_ms"`
}

type apiAnalyticsResponse struct {
	Range struct {
		Since time.Time `json:"since"`
		Until time.Time `json:"until"`
	} `json:"range"`
	Bucket string              `json:"bucket"`
	Groups []apiAnalyticsGroup `json:"groups"`
}

// newAnalyticsHandler returns a Handler wired with an invocation store and the
// given (optional) analytics limiter, plus the store for seeding.
func newAnalyticsHandler(t *testing.T, lim httpapi.CallerRateLimiter) (*http.ServeMux, *invocation.MemoryStore) {
	t.Helper()
	invStore := invocation.NewMemoryStore()
	h := httpapi.NewHandler(nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithProxy(&mockForwarder{}, invStore)
	if lim != nil {
		h.WithAnalyticsLimiter(lim)
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux, invStore
}

func getAnalytics(t *testing.T, mux *http.ServeMux, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/analytics?"+query, nil)
	req = req.WithContext(authtest.WithIdentity(req.Context(), testTenant, testUser))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAnalytics_MissingSinceIs400(t *testing.T) {
	t.Parallel()
	mux, _ := newAnalyticsHandler(t, nil)
	rec := getAnalytics(t, mux, "bucket=hour")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAnalytics_InvalidParamsAre400(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	since := now.Add(-time.Hour).Format(time.RFC3339)

	tests := []struct {
		name  string
		query string
	}{
		{"invalid since", "since=not-a-time"},
		{"invalid until", "since=" + since + "&until=not-a-time"},
		{"invalid bucket", "since=" + since + "&bucket=week"},
		{"invalid group_by", "since=" + since + "&group_by=model"},
		{"since after until", "since=" + now.Format(time.RFC3339) + "&until=" + now.Add(-time.Hour).Format(time.RFC3339)},
		{"range exceeds cap", "since=" + now.Add(-40*24*time.Hour).Format(time.RFC3339) + "&until=" + now.Format(time.RFC3339)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux, _ := newAnalyticsHandler(t, nil)
			rec := getAnalytics(t, mux, tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAnalytics_Unauthorized(t *testing.T) {
	t.Parallel()
	mux, _ := newAnalyticsHandler(t, nil)
	// No identity in context → 401.
	req := httptest.NewRequest(http.MethodGet, "/v1/analytics?since="+time.Now().Add(-time.Hour).Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAnalytics_RateLimited(t *testing.T) {
	t.Parallel()
	mux, _ := newAnalyticsHandler(t, stubCallerLimiter{delay: 2 * time.Second})
	rec := getAnalytics(t, mux, "since="+time.Now().Add(-time.Hour).Format(time.RFC3339))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After header missing on 429")
	}
}

func TestAnalytics_EmptyRangeIs200(t *testing.T) {
	t.Parallel()
	mux, _ := newAnalyticsHandler(t, nil)
	// A window entirely in the past with no seeded rows.
	since := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	until := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	rec := getAnalytics(t, mux, "since="+since+"&until="+until)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp apiAnalyticsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Groups) != 0 {
		t.Errorf("groups = %d, want 0 for empty range", len(resp.Groups))
	}
}

func TestAnalytics_HappyPath(t *testing.T) {
	t.Parallel()
	mux, store := newAnalyticsHandler(t, nil)

	const agentID = "agt_happy"
	up4xx := invocation.ErrorClassUpstream4xx
	// All rows land in the current hour bucket (Create stamps created_at = now).
	seed := func(status invocation.Status, ec *invocation.ErrorClass, latency, ttft *int64) {
		inv := &invocation.Invocation{
			AgentID:    agentID,
			Mode:       invocation.ModeTransactional,
			Status:     status,
			ErrorClass: ec,
			LatencyMS:  latency,
			TTFTMS:     ttft,
		}
		seedInvocation(t, store, testTenant, inv)
	}
	seed(invocation.StatusSucceeded, nil, i64ptr(100), i64ptr(10))
	seed(invocation.StatusSucceeded, nil, i64ptr(300), i64ptr(30))
	seed(invocation.StatusFailed, &up4xx, i64ptr(200), nil)
	seed(invocation.StatusRunning, nil, nil, nil) // in-flight

	since := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	rec := getAnalytics(t, mux, "since="+since+"&bucket=hour")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp apiAnalyticsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Bucket != "hour" {
		t.Errorf("bucket = %q, want hour", resp.Bucket)
	}
	if len(resp.Groups) != 1 {
		t.Fatalf("groups = %d, want 1; body = %s", len(resp.Groups), rec.Body.String())
	}
	g := resp.Groups[0]
	if g.AgentID != agentID {
		t.Errorf("agent_id = %q, want %q", g.AgentID, agentID)
	}
	if g.Count != 4 {
		t.Errorf("count = %d, want 4 (incl. in-flight)", g.Count)
	}
	if g.ErrorRate != 0.25 {
		t.Errorf("error_rate = %v, want 0.25", g.ErrorRate)
	}
	if g.ByErrorClass["upstream_4xx"] != 1 {
		t.Errorf("by_error_class[upstream_4xx] = %d, want 1", g.ByErrorClass["upstream_4xx"])
	}
	// Latency percentiles over [100,200,300]; in-flight row excluded.
	if g.LatencyMS.P50 == nil || *g.LatencyMS.P50 != 200 {
		t.Errorf("latency p50 = %v, want 200", g.LatencyMS.P50)
	}
	if g.LatencyMS.P95 == nil || *g.LatencyMS.P95 != 300 {
		t.Errorf("latency p95 = %v, want 300", g.LatencyMS.P95)
	}
	// TTFT percentiles over [10,30] only.
	if g.TTFTMS.P50 == nil || *g.TTFTMS.P50 != 10 {
		t.Errorf("ttft p50 = %v, want 10", g.TTFTMS.P50)
	}
}

func TestAnalytics_TenantIsolation(t *testing.T) {
	t.Parallel()
	mux, store := newAnalyticsHandler(t, nil)

	// Seed a row under a DIFFERENT tenant; the testTenant query must not see it.
	other := &invocation.Invocation{
		AgentID: "agt_other", Mode: invocation.ModeTransactional,
		Status: invocation.StatusSucceeded, LatencyMS: i64ptr(100),
	}
	if err := store.Create(context.Background(), "tenant-other", other); err != nil {
		t.Fatalf("seed other tenant: %v", err)
	}

	since := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	rec := getAnalytics(t, mux, "since="+since)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp apiAnalyticsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Groups) != 0 {
		t.Errorf("groups = %d, want 0 (other tenant's rows must not leak)", len(resp.Groups))
	}
}

func i64ptr(v int64) *int64 { return &v }
