// Package gateway provides the client Rooms uses to call one member's agent
// on behalf of another: every agent-to-agent call in Rooms is issued through
// the Farcaster gateway's proxy endpoint (POST /v1/proxy/{agt_id}), never
// directly. Routing the call this way means it inherits the gateway's policy
// enforcement, x402 payment gate, and invocation capture for free — Rooms
// does not reimplement any of that, and this package must not grow a direct
// agent-to-agent path.
//
// The proxy endpoint is ASYNCHRONOUS: it returns 202 with an invocation_id as
// soon as the call is accepted, then performs the upstream call server-side.
// A 202 therefore means "accepted", NOT "delivered" — the real outcome is only
// observable by polling GET /v1/proxy/{agt_id}/invocations/{inv_id} until the
// invocation reaches a terminal state. This client polls, because treating the
// 202 as success would report the dominant real failure mode (the recipient
// agent erroring, being unreachable, or timing out) as a delivered message.
//
// This package only issues the call and classifies the outcome. Retries,
// streaming, payment handling, and policy logic all live in the gateway.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Invocation states the gateway reports. Only succeeded and failed are
// terminal; pending and running mean the call is still in flight.
const (
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
)

// maxResponseBytes caps how much of a gateway response is parsed. These bodies
// are small JSON envelopes; the cap stops a malformed or hostile response from
// exhausting memory.
const maxResponseBytes = 1 << 20

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
	// could not be reached at all (network error, timeout), or the invocation
	// itself finished in a failed state.
	ErrorClassUpstreamFailure ErrorClass = "upstream_failure"
	// ErrorClassUnconfirmed means the gateway accepted the call but its
	// invocation did not reach a terminal state within the confirmation
	// budget, so whether the recipient received it is genuinely unknown.
	//
	// This is deliberately NOT folded into upstream_failure. The call may well
	// have succeeded, and recording it as a plain failure would be as wrong as
	// recording it as a delivery. Unknown is its own outcome, and the one
	// thing it must never be reported as is success.
	ErrorClassUnconfirmed ErrorClass = "unconfirmed"
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

// DefaultTimeout bounds a single HTTP request to the gateway when
// Config.Timeout is unset. A slow or hanging gateway must not be able to block
// a room indefinitely.
const DefaultTimeout = 30 * time.Second

// DefaultConfirmTimeout bounds the total time spent confirming that an
// accepted invocation reached a terminal state, across all poll attempts.
const DefaultConfirmTimeout = 60 * time.Second

// DefaultPollInterval is the delay between poll attempts while an invocation
// is still pending or running.
const DefaultPollInterval = 250 * time.Millisecond

// Config configures a Client. BaseURL and Credential must come from
// configuration — never hardcoded — and Credential must never be logged.
type Config struct {
	// BaseURL is the gateway's base URL, e.g. "https://gateway.example.com".
	// Required.
	BaseURL string
	// Credential is the bearer credential Rooms authenticates to the gateway
	// with. Required. Never logged and never returned in an error.
	Credential string
	// Timeout bounds a single HTTP request. Zero or negative uses
	// DefaultTimeout.
	Timeout time.Duration
	// ConfirmTimeout bounds the total time spent polling for an accepted
	// invocation's terminal state. Zero or negative uses
	// DefaultConfirmTimeout.
	ConfirmTimeout time.Duration
	// PollInterval is the delay between poll attempts. Zero or negative uses
	// DefaultPollInterval.
	PollInterval time.Duration
}

// Result describes a confirmed-delivered proxied call. InvocationID is the
// gateway's own identifier for the call, carried back so a room event can be
// reconciled against the gateway's invocation record.
type Result struct {
	InvocationID   string
	UpstreamStatus int // the recipient's HTTP status; 0 when the gateway did not report one
}

// Client issues agent-to-agent calls through the gateway's proxy endpoint. It
// holds no state about rooms, members, or messages — callers supply whatever
// they need delivered.
type Client struct {
	baseURL        string
	credential     string
	httpClient     *http.Client
	confirmTimeout time.Duration
	pollInterval   time.Duration
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
	confirmTimeout := cfg.ConfirmTimeout
	if confirmTimeout <= 0 {
		confirmTimeout = DefaultConfirmTimeout
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	return &Client{
		baseURL:        strings.TrimRight(cfg.BaseURL, "/"),
		credential:     cfg.Credential,
		httpClient:     &http.Client{Timeout: timeout},
		confirmTimeout: confirmTimeout,
		pollInterval:   pollInterval,
	}, nil
}

// Call delivers body to agentID through the gateway's proxy and returns only
// once delivery is CONFIRMED.
//
// The proxy is asynchronous: the POST returns 202 with an invocation_id, and
// the upstream call runs server-side afterwards. Call therefore does two
// things — it issues the POST, then polls
// GET /v1/proxy/{agt_id}/invocations/{inv_id} until the invocation is
// succeeded or failed. Returning at the 202 would report every upstream
// error, timeout, and unreachable agent as a successful delivery.
//
// Failures are classified, never forwarded: a 4xx (on the POST, or as the
// recipient's own status) is ErrorClassCallerError; a 5xx, network failure, or
// failed invocation is ErrorClassUpstreamFailure; an accepted call whose
// outcome is still unknown when the confirmation budget runs out is
// ErrorClassUnconfirmed. No raw response body reaches the caller (AGENTS.md
// invariant #3), and the credential never appears in a returned error
// (invariant #4).
func (c *Client) Call(ctx context.Context, agentID string, body []byte) (*Result, error) {
	invocationID, err := c.accept(ctx, agentID, body)
	if err != nil {
		return nil, err
	}
	return c.confirm(ctx, agentID, invocationID)
}

// accept issues the POST and returns the invocation_id the gateway assigned.
func (c *Client) accept(ctx context.Context, agentID string, body []byte) (string, error) {
	reqURL := c.baseURL + "/v1/proxy/" + url.PathEscape(agentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("gateway: build proxied call request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.credential)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Do's error can embed the request URL, but never the Authorization
		// header, so the credential is not at risk of leaking here.
		return "", &CallError{Class: ErrorClassUpstreamFailure}
	}
	defer closeBody(resp)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return "", &CallError{Class: ErrorClassCallerError, StatusCode: resp.StatusCode}
	default:
		return "", &CallError{Class: ErrorClassUpstreamFailure, StatusCode: resp.StatusCode}
	}

	var accepted struct {
		InvocationID string `json:"invocation_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&accepted); err != nil || accepted.InvocationID == "" {
		// Accepted, but with no handle to confirm against. The call may well
		// run to completion server-side, so this is unconfirmed rather than
		// failed — and it must not be reported as delivered.
		return "", &CallError{Class: ErrorClassUnconfirmed, StatusCode: resp.StatusCode}
	}
	return accepted.InvocationID, nil
}

// confirm polls the invocation until it reaches a terminal state, the
// confirmation budget expires, or ctx is cancelled.
func (c *Client) confirm(ctx context.Context, agentID, invocationID string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.confirmTimeout)
	defer cancel()

	unconfirmed := &CallError{Class: ErrorClassUnconfirmed}
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		status, upstream, err := c.poll(ctx, agentID, invocationID)
		if err == nil {
			switch status {
			case statusSucceeded:
				return &Result{InvocationID: invocationID, UpstreamStatus: upstream}, nil
			case statusFailed:
				// The gateway reached the recipient and it failed, or the
				// gateway could not reach it. A recipient 4xx is the caller's
				// problem; anything else is an upstream failure.
				class := ErrorClassUpstreamFailure
				if upstream >= 400 && upstream < 500 {
					class = ErrorClassCallerError
				}
				return nil, &CallError{Class: class, StatusCode: upstream}
			}
			// pending or running — keep waiting.
		}
		// A transient poll failure is not itself a delivery failure: the
		// invocation is still running server-side. Retry until the budget runs
		// out, then report the outcome as unknown.

		select {
		case <-ctx.Done():
			return nil, unconfirmed
		case <-ticker.C:
		}
	}
}

// poll fetches the invocation's current status and the recipient's HTTP status
// (zero when the gateway has not recorded one yet).
func (c *Client) poll(ctx context.Context, agentID, invocationID string) (string, int, error) {
	reqURL := c.baseURL + "/v1/proxy/" + url.PathEscape(agentID) +
		"/invocations/" + url.PathEscape(invocationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.credential)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer closeBody(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("gateway: poll returned status %d", resp.StatusCode)
	}

	var payload struct {
		Status         string `json:"status"`
		UpstreamStatus *int   `json:"upstream_status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return "", 0, err
	}
	upstream := 0
	if payload.UpstreamStatus != nil {
		upstream = *payload.UpstreamStatus
	}
	return payload.Status, upstream, nil
}

func closeBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
