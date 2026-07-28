package gateway_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/gateway"
)

const testCredential = "s3cr3t-credential-value" //nolint:gosec // test fixture, not a real credential

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

func TestClient_Call_Success(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod, gotAuth, gotContentType string
	var gotBody []byte
	var requestCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c, err := gateway.New(gateway.Config{BaseURL: srv.URL, Credential: testCredential})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.Call(context.Background(), "agt_recipient", []byte(`{"body":"hi"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if got := atomic.LoadInt32(&requestCount); got != 1 {
		t.Fatalf("request count = %d, want exactly 1", got)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/proxy/agt_recipient" {
		t.Errorf("path = %q, want %q", gotPath, "/v1/proxy/agt_recipient")
	}
	if gotAuth != "Bearer "+testCredential {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer "+testCredential)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if string(gotBody) != `{"body":"hi"}` {
		t.Errorf("body = %q, want %q", gotBody, `{"body":"hi"}`)
	}
}

func TestClient_Call_ClassifiesCallerError(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("internal detail that must not leak"))
			}))
			defer srv.Close()

			c, err := gateway.New(gateway.Config{BaseURL: srv.URL, Credential: testCredential})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			err = c.Call(context.Background(), "agt_x", nil)
			var callErr *gateway.CallError
			if !errors.As(err, &callErr) {
				t.Fatalf("err = %v, want *gateway.CallError", err)
			}
			if callErr.Class != gateway.ErrorClassCallerError {
				t.Errorf("Class = %q, want %q", callErr.Class, gateway.ErrorClassCallerError)
			}
			if callErr.StatusCode != status {
				t.Errorf("StatusCode = %d, want %d", callErr.StatusCode, status)
			}
			if strings.Contains(err.Error(), "internal detail") {
				t.Errorf("err.Error() = %q, must not contain the raw upstream body", err.Error())
			}
		})
	}
}

func TestClient_Call_ClassifiesUpstreamFailure(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			c, err := gateway.New(gateway.Config{BaseURL: srv.URL, Credential: testCredential})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			err = c.Call(context.Background(), "agt_x", nil)
			var callErr *gateway.CallError
			if !errors.As(err, &callErr) {
				t.Fatalf("err = %v, want *gateway.CallError", err)
			}
			if callErr.Class != gateway.ErrorClassUpstreamFailure {
				t.Errorf("Class = %q, want %q", callErr.Class, gateway.ErrorClassUpstreamFailure)
			}
			if callErr.StatusCode != status {
				t.Errorf("StatusCode = %d, want %d", callErr.StatusCode, status)
			}
		})
	}
}

// TestClient_Call_TimeoutClassifiesAsUpstreamFailure verifies that a gateway
// which never responds cannot block a call indefinitely: Client enforces its
// own request timeout independent of the caller's context.
func TestClient_Call_TimeoutClassifiesAsUpstreamFailure(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock // released below, after Client.Call has already timed out
	}))
	// srv.Close waits for the in-flight handler to return, so unblock must be
	// closed first: defers run LIFO, so declaring this second means it runs
	// before srv.Close (declared first).
	defer srv.Close()
	defer close(unblock)

	c, err := gateway.New(gateway.Config{
		BaseURL:    srv.URL,
		Credential: testCredential,
		Timeout:    50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	err = c.Call(context.Background(), "agt_slow", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("err = nil, want a timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Call took %s, want it bounded by the configured timeout", elapsed)
	}
	var callErr *gateway.CallError
	if !errors.As(err, &callErr) {
		t.Fatalf("err = %v, want *gateway.CallError", err)
	}
	if callErr.Class != gateway.ErrorClassUpstreamFailure {
		t.Errorf("Class = %q, want %q", callErr.Class, gateway.ErrorClassUpstreamFailure)
	}
}

// TestClient_Call_CredentialNeverLeaks verifies that the configured
// credential never appears in a returned error, regardless of outcome
// (AGENTS.md invariant #4).
func TestClient_Call_CredentialNeverLeaks(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, err := gateway.New(gateway.Config{BaseURL: srv.URL, Credential: testCredential})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = c.Call(context.Background(), "agt_x", nil)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if strings.Contains(err.Error(), testCredential) {
		t.Errorf("err.Error() = %q, must never contain the credential", err.Error())
	}
}

func TestClient_Call_UnreachableGatewayClassifiesAsUpstreamFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close() // closed before use: connections to it fail

	c, err := gateway.New(gateway.Config{BaseURL: srv.URL, Credential: testCredential})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = c.Call(context.Background(), "agt_x", nil)
	var callErr *gateway.CallError
	if !errors.As(err, &callErr) {
		t.Fatalf("err = %v, want *gateway.CallError", err)
	}
	if callErr.Class != gateway.ErrorClassUpstreamFailure {
		t.Errorf("Class = %q, want %q", callErr.Class, gateway.ErrorClassUpstreamFailure)
	}
}
