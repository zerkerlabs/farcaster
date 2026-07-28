// Package gateway provides the client Rooms uses to call one member's agent
// on behalf of another: every agent-to-agent call in Rooms is issued through
// the Farcaster gateway's proxy endpoint (POST /v1/proxy/{agt_id}), never
// directly. Routing the call this way means it inherits the gateway's policy
// enforcement, x402 payment gate, and invocation capture for free — Rooms
// does not reimplement any of that, and this package must not grow a direct
// agent-to-agent path.
//
// This package only issues the call and classifies the response. Retries,
// streaming, payment handling, and policy logic all live in the gateway.
package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrorClass distinguishes why a proxied call did not succeed: a caller-side
// rejection (the gateway's synchronous 4xx response to the POST itself — e.g.
// the target agent does not exist, is inactive/suspended, or the call was
// malformed) from a gateway-side failure (a 5xx response, or the gateway
// being unreachable). Rooms uses this to decide how to surface the failure
// without ever forwarding the raw response body (AGENTS.md invariant #3).
type ErrorClass string

const (
	// ErrorClassCallerError means the gateway rejected the call with a 4xx
	// status: the call itself was invalid, not the gateway.
	ErrorClassCallerError ErrorClass = "caller_error"
	// ErrorClassUpstreamFailure means the gateway responded with a 5xx status,
	// or could not be reached at all (network error, timeout).
	ErrorClassUpstreamFailure ErrorClass = "upstream_failure"
)

// CallError is returned by Client.Call when a proxied call does not succeed.
// StatusCode is the gateway's response status, or zero when the gateway was
// never reached (a network error or timeout). The raw response body is never
// captured here — only the classified status is, per AGENTS.md invariant #3.
type CallError struct {
	Class      ErrorClass
	StatusCode int
}

func (e *CallError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("gateway call failed: %s", e.Class)
	}
	return fmt.Sprintf("gateway call failed: %s (status %d)", e.Class, e.StatusCode)
}

// DefaultTimeout bounds a single proxied call when Config.Timeout is unset.
// A slow or hanging gateway must not be able to block a room indefinitely.
const DefaultTimeout = 30 * time.Second

// Config configures a Client. BaseURL and Credential must come from
// configuration — never hardcoded — and Credential must never be logged.
type Config struct {
	// BaseURL is the gateway's base URL, e.g. "https://gateway.example.com".
	// Required.
	BaseURL string
	// Credential is the bearer credential Rooms authenticates to the gateway
	// with. Required. Never logged and never returned in an error.
	Credential string
	// Timeout bounds a single proxied call. Zero or negative uses
	// DefaultTimeout.
	Timeout time.Duration
}

// Client issues agent-to-agent calls through the gateway's proxy endpoint. It
// holds no state about rooms, members, or messages — callers supply whatever
// they need delivered.
type Client struct {
	baseURL    string
	credential string
	httpClient *http.Client
}

// New constructs a Client from cfg. Returns an error if BaseURL or Credential
// is empty — both are required and must come from configuration.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("gateway: base URL is required")
	}
	if cfg.Credential == "" {
		return nil, errors.New("gateway: credential is required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		credential: cfg.Credential,
		httpClient: &http.Client{Timeout: timeout},
	}, nil
}

// Call issues a single POST /v1/proxy/{agt_id} call against the configured
// gateway, carrying body as the request payload, and returns nil on any 2xx
// response.
//
// A 4xx response is classified as *CallError with Class
// ErrorClassCallerError; a 5xx response, a network failure, or a timeout is
// classified as *CallError with Class ErrorClassUpstreamFailure. Neither case
// exposes the raw response body — the caller gets only the classified status
// (AGENTS.md invariant #3), and the credential this Client authenticates with
// never appears in the returned error (invariant #4).
func (c *Client) Call(ctx context.Context, agentID string, body []byte) error {
	reqURL := c.baseURL + "/v1/proxy/" + url.PathEscape(agentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("gateway: build proxied call request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.credential)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Do's error can embed the request URL, but never the Authorization
		// header, so the credential is not at risk of leaking here.
		return &CallError{Class: ErrorClassUpstreamFailure}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return &CallError{Class: ErrorClassCallerError, StatusCode: resp.StatusCode}
	default:
		return &CallError{Class: ErrorClassUpstreamFailure, StatusCode: resp.StatusCode}
	}
}
