package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/gateway"
)

const testCredential = "s3cr3t-credential-value" //nolint:gosec // test fixture, not a real credential

// fakeGateway models the gateway's ACTUAL proxy contract: POST accepts the
// call and returns 202 with an invocation_id, and the real outcome is only
// available by polling the invocation.
//
// The tests this replaced used a server that answered 202 to everything and
// served no invocation at all — which is why a client that treated 202 as
// delivery passed them. A fake that cannot express "accepted then failed"
// cannot catch the bug that matters here.
type fakeGateway struct {
	// statuses is the sequence of invocation states returned by successive
	// polls; the last entry repeats once exhausted.
	statuses []string
	// upstreamStatus is reported alongside a terminal state; 0 omits it.
	upstreamStatus int
	// acceptStatus overrides the POST response status (default 202).
	acceptStatus int
	// omitInvocationID accepts the call without returning a handle to poll.
	omitInvocationID bool

	mu        sync.Mutex
	posts     int32
	polls     int32
	lastPath  string
	lastAuth  string
	lastBody  []byte
	pollPaths []string
}

func (f *fakeGateway) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		if r.Method == http.MethodPost {
			atomic.AddInt32(&f.posts, 1)
			f.lastPath = r.URL.Path
			f.lastAuth = r.Header.Get("Authorization")
			f.lastBody, _ = io.ReadAll(r.Body)

			status := f.acceptStatus
			if status == 0 {
				status = http.StatusAccepted
			}
			if status < 200 || status >= 300 {
				w.WriteHeader(status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if f.omitInvocationID {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"invocation_id": "inv_abc123"})
			return
		}

		n := atomic.AddInt32(&f.polls, 1)
		f.pollPaths = append(f.pollPaths, r.URL.Path)

		idx := int(n) - 1
		if idx >= len(f.statuses) {
			idx = len(f.statuses) - 1
		}
		body := map[string]any{"status": f.statuses[idx]}
		if f.upstreamStatus != 0 {
			body["upstream_status"] = f.upstreamStatus
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fastClient(t *testing.T, baseURL string) *gateway.Client {
	t.Helper()
	c, err := gateway.New(gateway.Config{
		BaseURL:        baseURL,
		Credential:     testCredential,
		ConfirmTimeout: 2 * time.Second,
		PollInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func classOf(t *testing.T, err error) gateway.ErrorClass {
	t.Helper()
	var ce *gateway.CallError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *gateway.CallError", err, err)
	}
	return ce.Class
}

func TestNew_RequiresBaseURLAndCredential(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  gateway.Config
	}{
		{"missing base URL", gateway.Config{Credential: testCredential}},
		{"missing credential", gateway.Config{BaseURL: "https://gateway.example.com"}},
		{"missing both", gateway.Config{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c, err := gateway.New(tt.cfg)
			if err == nil {
				t.Fatal("err = nil, want an error for incomplete config")
			}
			if c != nil {
				t.Errorf("client = %v, want nil", c)
			}
		})
	}
}

// A call is delivered only once its invocation reports succeeded — and the
// client must poll to find that out.
func TestClient_Call_ConfirmsViaPolling(t *testing.T) {
	t.Parallel()

	f := &fakeGateway{statuses: []string{"pending", "running", "succeeded"}, upstreamStatus: 200}
	srv := f.server(t)

	result, err := fastClient(t, srv.URL).Call(context.Background(), "agt_recipient", []byte(`{"body":"hi"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if got := atomic.LoadInt32(&f.posts); got != 1 {
		t.Errorf("POST count = %d, want exactly 1", got)
	}
	if got := atomic.LoadInt32(&f.polls); got < 3 {
		t.Errorf("poll count = %d, want at least 3 (pending, running, succeeded)", got)
	}
	if f.lastPath != "/v1/proxy/agt_recipient" {
		t.Errorf("POST path = %q, want %q", f.lastPath, "/v1/proxy/agt_recipient")
	}
	if f.lastAuth != "Bearer "+testCredential {
		t.Errorf("Authorization = %q, want the configured credential", f.lastAuth)
	}
	if string(f.lastBody) != `{"body":"hi"}` {
		t.Errorf("body = %q, want %q", f.lastBody, `{"body":"hi"}`)
	}
	if want := "/v1/proxy/agt_recipient/invocations/inv_abc123"; f.pollPaths[0] != want {
		t.Errorf("poll path = %q, want %q", f.pollPaths[0], want)
	}
	if result.InvocationID != "inv_abc123" {
		t.Errorf("InvocationID = %q, want %q", result.InvocationID, "inv_abc123")
	}
	if result.UpstreamStatus != 200 {
		t.Errorf("UpstreamStatus = %d, want 200", result.UpstreamStatus)
	}
}

// The regression this whole change exists for: the gateway accepts the call
// with a 202 and the invocation then FAILS. Treating the 202 as success
// reports the dominant real-world failure — the recipient erroring, being
// unreachable, or timing out — as a delivered message.
func TestClient_Call_AcceptedThenFailedIsNotDelivered(t *testing.T) {
	t.Parallel()

	f := &fakeGateway{statuses: []string{"running", "failed"}, upstreamStatus: 502}
	srv := f.server(t)

	result, err := fastClient(t, srv.URL).Call(context.Background(), "agt_recipient", []byte(`{}`))
	if err == nil {
		t.Fatal("err = nil for a failed invocation — an accepted call was reported as delivered")
	}
	if result != nil {
		t.Errorf("result = %+v, want nil on failure", result)
	}
	if got := classOf(t, err); got != gateway.ErrorClassUpstreamFailure {
		t.Errorf("class = %q, want %q", got, gateway.ErrorClassUpstreamFailure)
	}
}

// The recipient's own 4xx is the caller's problem, not the gateway's.
func TestClient_Call_FailedWithRecipient4xxIsCallerError(t *testing.T) {
	t.Parallel()

	f := &fakeGateway{statuses: []string{"failed"}, upstreamStatus: 422}
	srv := f.server(t)

	_, err := fastClient(t, srv.URL).Call(context.Background(), "agt_recipient", []byte(`{}`))
	if got := classOf(t, err); got != gateway.ErrorClassCallerError {
		t.Errorf("class = %q, want %q", got, gateway.ErrorClassCallerError)
	}
}

// A synchronous rejection of the POST never reaches the polling stage.
func TestClient_Call_ClassifiesSynchronousRejection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status int
		want   gateway.ErrorClass
	}{
		{http.StatusBadRequest, gateway.ErrorClassCallerError},
		{http.StatusNotFound, gateway.ErrorClassCallerError},
		{http.StatusUnprocessableEntity, gateway.ErrorClassCallerError},
		{http.StatusTooManyRequests, gateway.ErrorClassCallerError},
		{http.StatusInternalServerError, gateway.ErrorClassUpstreamFailure},
		{http.StatusBadGateway, gateway.ErrorClassUpstreamFailure},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			t.Parallel()

			f := &fakeGateway{acceptStatus: tt.status, statuses: []string{"succeeded"}}
			srv := f.server(t)

			_, err := fastClient(t, srv.URL).Call(context.Background(), "agt_recipient", []byte(`{}`))
			if got := classOf(t, err); got != tt.want {
				t.Errorf("class = %q, want %q", got, tt.want)
			}
			if got := atomic.LoadInt32(&f.polls); got != 0 {
				t.Errorf("poll count = %d, want 0 — a rejected call has no invocation to poll", got)
			}
		})
	}
}

// An invocation that never reaches a terminal state is UNKNOWN, not delivered
// and not failed. It may still have reached the recipient, so it gets its own
// class rather than being folded into either certainty.
func TestClient_Call_NeverTerminalIsUnconfirmed(t *testing.T) {
	t.Parallel()

	f := &fakeGateway{statuses: []string{"running"}}
	srv := f.server(t)

	c, err := gateway.New(gateway.Config{
		BaseURL:        srv.URL,
		Credential:     testCredential,
		ConfirmTimeout: 60 * time.Millisecond,
		PollInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, callErr := c.Call(context.Background(), "agt_recipient", []byte(`{}`))
	if callErr == nil {
		t.Fatal("err = nil — an unconfirmed call was reported as delivered")
	}
	if result != nil {
		t.Errorf("result = %+v, want nil", result)
	}
	if got := classOf(t, callErr); got != gateway.ErrorClassUnconfirmed {
		t.Errorf("class = %q, want %q", got, gateway.ErrorClassUnconfirmed)
	}
}

// Accepted with no invocation_id leaves nothing to confirm against. The call
// may well run to completion, so it is unconfirmed rather than failed — and
// must not be reported as delivered.
func TestClient_Call_AcceptedWithoutInvocationIDIsUnconfirmed(t *testing.T) {
	t.Parallel()

	f := &fakeGateway{omitInvocationID: true, statuses: []string{"succeeded"}}
	srv := f.server(t)

	_, err := fastClient(t, srv.URL).Call(context.Background(), "agt_recipient", []byte(`{}`))
	if got := classOf(t, err); got != gateway.ErrorClassUnconfirmed {
		t.Errorf("class = %q, want %q", got, gateway.ErrorClassUnconfirmed)
	}
}

// A poll that transiently fails is not a delivery failure — the invocation is
// still running server-side — so the client keeps trying until it resolves.
func TestClient_Call_TransientPollFailureIsRetried(t *testing.T) {
	t.Parallel()

	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"invocation_id":"inv_abc123"}`))
			return
		}
		if atomic.AddInt32(&polls, 1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"status":"succeeded","upstream_status":200}`))
	}))
	defer srv.Close()

	if _, err := fastClient(t, srv.URL).Call(context.Background(), "agt_recipient", []byte(`{}`)); err != nil {
		t.Fatalf("Call: %v — transient poll failures should be retried, not treated as delivery failures", err)
	}
}

func TestClient_Call_UnreachableGatewayIsUpstreamFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	c, err := gateway.New(gateway.Config{BaseURL: url, Credential: testCredential})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, callErr := c.Call(context.Background(), "agt_recipient", []byte(`{}`))
	if got := classOf(t, callErr); got != gateway.ErrorClassUpstreamFailure {
		t.Errorf("class = %q, want %q", got, gateway.ErrorClassUpstreamFailure)
	}
}

// The credential must never surface in an error a caller could log or return
// (AGENTS.md invariant #4).
func TestClient_Call_CredentialNeverLeaks(t *testing.T) {
	t.Parallel()

	cases := map[string]*fakeGateway{
		"synchronous rejection": {acceptStatus: http.StatusForbidden},
		"failed invocation":     {statuses: []string{"failed"}, upstreamStatus: 500},
		"unconfirmed":           {omitInvocationID: true},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := f.server(t)
			_, err := fastClient(t, srv.URL).Call(context.Background(), "agt_recipient", []byte(`{}`))
			if err == nil {
				t.Fatal("err = nil, want an error")
			}
			if strings.Contains(err.Error(), testCredential) {
				t.Errorf("error %q contains the credential", err.Error())
			}
		})
	}
}
