package httpapi

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/auth"
	"github.com/zerkerlabs/farcaster/gateway/internal/invocation"
)

// maxAnalyticsRange caps the [since, until] window a single /v1/analytics query
// may span. Percentile aggregation scans every row in the range, so an unbounded
// window is an availability risk; 31 days is the v1 cap (spec 0003 decision 4).
const maxAnalyticsRange = 31 * 24 * time.Hour

// analyticsResponse is the body returned by GET /v1/analytics.
type analyticsResponse struct {
	Range  analyticsRange   `json:"range"`
	Bucket string           `json:"bucket"`
	Groups []analyticsGroup `json:"groups"`
}

type analyticsRange struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// analyticsGroup is one (agent_id, bucket_start) cell. ErrorRate is
// ErrorCount/Count in [0,1]. Latency/TTFT percentiles are null when no row in
// the group carried the metric (e.g. all rows in-flight).
type analyticsGroup struct {
	AgentID      string                 `json:"agent_id"`
	BucketStart  time.Time              `json:"bucket_start"`
	Count        int                    `json:"count"`
	ErrorRate    float64                `json:"error_rate"`
	ByErrorClass map[string]int         `json:"by_error_class"`
	LatencyMS    invocation.Percentiles `json:"latency_ms"`
	TTFTMS       invocation.Percentiles `json:"ttft_ms"`
}

// handleAnalytics handles GET /v1/analytics. It returns aggregate metrics over
// the caller's tenant, grouped by agent_id and a time bucket (spec 0003). The
// endpoint is OAuth-gated, tenant-scoped, carries its own tighter per-caller
// rate limit, and requires a `since` bound with a capped range.
func (h *Handler) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	if tenant == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Tighter per-caller rate limit than the global /v1 limiter: percentile
	// aggregation is heavier than a row fetch (spec 0003 decision 4).
	if h.analyticsLimiter != nil {
		if delay := h.analyticsLimiter.Allow(tenant + "\x00" + user); delay > 0 {
			retryAfter := int(math.Ceil(delay.Seconds()))
			if retryAfter > proxyRetryAfterCap {
				retryAfter = proxyRetryAfterCap
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
	}

	q := r.URL.Query()

	// group_by: agent_id is the only supported dimension in v1 (spec 0003
	// decision 2). An explicit other value is rejected rather than ignored.
	if gb := q.Get("group_by"); gb != "" && gb != "agent_id" {
		writeError(w, http.StatusBadRequest, "invalid group_by: only agent_id is supported")
		return
	}

	// bucket: hour (default) or day.
	var bucket invocation.Bucket
	switch q.Get("bucket") {
	case "", "hour":
		bucket = invocation.BucketHour
	case "day":
		bucket = invocation.BucketDay
	default:
		writeError(w, http.StatusBadRequest, "invalid bucket: must be one of hour, day")
		return
	}

	// since is required (spec 0003 decision 3).
	sinceStr := q.Get("since")
	if sinceStr == "" {
		writeError(w, http.StatusBadRequest, "since is required (RFC 3339 timestamp)")
		return
	}
	since, err := time.Parse(time.RFC3339, sinceStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid since: must be RFC 3339 timestamp")
		return
	}

	// until defaults to now when absent.
	until := time.Now().UTC()
	if uv := q.Get("until"); uv != "" {
		until, err = time.Parse(time.RFC3339, uv)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid until: must be RFC 3339 timestamp")
			return
		}
	}

	if since.After(until) {
		writeError(w, http.StatusBadRequest, "since must not be after until")
		return
	}
	if until.Sub(since) > maxAnalyticsRange {
		writeError(w, http.StatusBadRequest, "range exceeds the maximum of 31 days")
		return
	}

	groups, err := h.invocations.Aggregate(r.Context(), tenant, invocation.AggregateQuery{
		Bucket: bucket,
		Since:  since,
		Until:  until,
	})
	if err != nil {
		h.logger.Error("analytics: aggregate", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	resp := analyticsResponse{
		Range:  analyticsRange{Since: since, Until: until},
		Bucket: string(bucket),
		Groups: make([]analyticsGroup, 0, len(groups)),
	}
	for _, g := range groups {
		byErrorClass := make(map[string]int, len(g.ByErrorClass))
		for ec, n := range g.ByErrorClass {
			byErrorClass[string(ec)] = n
		}
		var errorRate float64
		if g.Count > 0 {
			errorRate = float64(g.ErrorCount) / float64(g.Count)
		}
		resp.Groups = append(resp.Groups, analyticsGroup{
			AgentID:      g.AgentID,
			BucketStart:  g.BucketStart,
			Count:        g.Count,
			ErrorRate:    errorRate,
			ByErrorClass: byErrorClass,
			LatencyMS:    g.LatencyMS,
			TTFTMS:       g.TTFTMS,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}
