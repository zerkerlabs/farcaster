package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/policy"
	"github.com/zerkerlabs/gateway/gateway/internal/ssrf"
)

// defaultClassifierTimeout bounds a single classifier webhook round-trip so a
// slow or unresponsive webhook can never hang the request path (spec 0009
// Decision 4: "best-effort ... no hang on the request path; bounded
// timeout"). enforcePolicy additionally wraps the call in a context with this
// same bound, so the guarantee holds even if a caller injects a
// policy.ClassifierClient whose own *http.Client carries no Timeout.
const defaultClassifierTimeout = 2 * time.Second

// maxClassifierResponseBytes caps how much of a classifier webhook's response
// body is read, mirroring maxSettleResponseBytes — a misbehaving or malicious
// webhook cannot exhaust memory.
const maxClassifierResponseBytes = 1 << 16 // 64 KiB

// httpClassifierClient is the production policy.ClassifierClient: it POSTs
// the minimal request context to an operator-configured webhook and parses
// its verdict. It makes no assumption about the webhook's own implementation
// (Llama Guard, Lakera, an LLM-judge) — only the wire contract in
// policy.ClassifierVerdict.
type httpClassifierClient struct {
	client *http.Client
}

// NewHTTPClassifierClient returns a policy.ClassifierClient that calls
// webhooks over HTTP(S) through the SSRF guard (internal/ssrf), exactly as
// NewFacilitatorSettler does for the facilitator settle call (T6 acceptance:
// "same SSRF guard as upstreams"). If client is nil, a client with the
// SSRF-safe dialer and defaultClassifierTimeout is constructed; tests may
// inject a stubbed override (e.g. an httptest.Server's own client) the same
// way NewFacilitatorSettler's tests do.
func NewHTTPClassifierClient(client *http.Client) policy.ClassifierClient {
	if client == nil {
		t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // http.DefaultTransport is always *http.Transport
		t.DialContext = ssrf.SafeDialContext
		client = &http.Client{Transport: t, Timeout: defaultClassifierTimeout}
	}
	return &httpClassifierClient{client: client}
}

// Classify implements policy.ClassifierClient.
func (c *httpClassifierClient) Classify(ctx context.Context, hookURL string, req policy.ClassifierRequest) (policy.ClassifierVerdict, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return policy.ClassifierVerdict{}, fmt.Errorf("classifier: build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, hookURL, bytes.NewReader(body))
	if err != nil {
		return policy.ClassifierVerdict{}, fmt.Errorf("classifier: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq) //nolint:gosec // hookURL validated at authoring boundary (validateClassifierURL); DNS-rebinding re-checked at dial time via ssrf.SafeDialContext
	if err != nil {
		return policy.ClassifierVerdict{}, fmt.Errorf("classifier: unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxClassifierResponseBytes))
	if err != nil {
		return policy.ClassifierVerdict{}, fmt.Errorf("classifier: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return policy.ClassifierVerdict{}, fmt.Errorf("classifier: non-2xx status %d", resp.StatusCode)
	}

	var verdict policy.ClassifierVerdict
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return policy.ClassifierVerdict{}, fmt.Errorf("classifier: malformed response: %w", err)
	}
	if !isValidAction(verdict.Action) {
		return policy.ClassifierVerdict{}, fmt.Errorf("classifier: invalid verdict action %q", verdict.Action)
	}

	return verdict, nil
}
