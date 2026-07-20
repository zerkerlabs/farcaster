package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/policy"
)

func TestHTTPClassifierClient_Classify_Success(t *testing.T) {
	t.Parallel()

	var gotReq policy.ClassifierRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(policy.ClassifierVerdict{Action: policy.ActionDeny, Reason: "flagged: unsafe content"})
	}))
	defer srv.Close()

	client := NewHTTPClassifierClient(srv.Client())
	tool := "risky_tool"
	verdict, err := client.Classify(context.Background(), srv.URL, policy.ClassifierRequest{
		TenantID: "tnt_1", AgentID: "agt_1", Protocol: policy.ProtocolMCP, MCPTool: &tool,
	})
	if err != nil {
		t.Fatalf("Classify: unexpected error: %v", err)
	}
	if verdict.Action != policy.ActionDeny {
		t.Errorf("Action = %q, want deny", verdict.Action)
	}
	if verdict.Reason != "flagged: unsafe content" {
		t.Errorf("Reason = %q, want %q", verdict.Reason, "flagged: unsafe content")
	}
	if gotReq.TenantID != "tnt_1" || gotReq.AgentID != "agt_1" {
		t.Errorf("request context not forwarded correctly: %+v", gotReq)
	}
	if gotReq.MCPTool == nil || *gotReq.MCPTool != tool {
		t.Errorf("request MCPTool = %v, want %q", gotReq.MCPTool, tool)
	}
}

func TestHTTPClassifierClient_Classify_Allow(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(policy.ClassifierVerdict{Action: policy.ActionAllow})
	}))
	defer srv.Close()

	client := NewHTTPClassifierClient(srv.Client())
	verdict, err := client.Classify(context.Background(), srv.URL, policy.ClassifierRequest{TenantID: "tnt_1"})
	if err != nil {
		t.Fatalf("Classify: unexpected error: %v", err)
	}
	if verdict.Action != policy.ActionAllow {
		t.Errorf("Action = %q, want allow", verdict.Action)
	}
}

func TestHTTPClassifierClient_Classify_BadStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewHTTPClassifierClient(srv.Client())
	_, err := client.Classify(context.Background(), srv.URL, policy.ClassifierRequest{TenantID: "tnt_1"})
	if err == nil {
		t.Fatal("Classify: expected error, got nil")
	}
}

func TestHTTPClassifierClient_Classify_MalformedBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	client := NewHTTPClassifierClient(srv.Client())
	_, err := client.Classify(context.Background(), srv.URL, policy.ClassifierRequest{TenantID: "tnt_1"})
	if err == nil {
		t.Fatal("Classify: expected error, got nil")
	}
}

func TestHTTPClassifierClient_Classify_InvalidVerdictAction(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(policy.ClassifierVerdict{Action: policy.Action("block")})
	}))
	defer srv.Close()

	client := NewHTTPClassifierClient(srv.Client())
	_, err := client.Classify(context.Background(), srv.URL, policy.ClassifierRequest{TenantID: "tnt_1"})
	if err == nil {
		t.Fatal("Classify: expected error for invalid verdict action, got nil")
	}
}

func TestHTTPClassifierClient_Classify_Unreachable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // closed immediately: connection refused

	client := NewHTTPClassifierClient(srv.Client())
	_, err := client.Classify(context.Background(), url, policy.ClassifierRequest{TenantID: "tnt_1"})
	if err == nil {
		t.Fatal("Classify: expected error, got nil")
	}
}

func TestHTTPClassifierClient_Classify_Timeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(policy.ClassifierVerdict{Action: policy.ActionDeny})
	}))
	defer srv.Close()

	fastClient := *srv.Client()
	fastClient.Timeout = 10 * time.Millisecond
	client := NewHTTPClassifierClient(&fastClient)

	_, err := client.Classify(context.Background(), srv.URL, policy.ClassifierRequest{TenantID: "tnt_1"})
	if err == nil {
		t.Fatal("Classify: expected a timeout error, got nil")
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && !netErr.Timeout() {
		t.Errorf("Classify: error is not a timeout: %v", err)
	}
}

// TestHTTPClassifierClient_Classify_SSRFGuard exercises the production wiring
// (NewHTTPClassifierClient(nil)): a classifier URL that resolves to loopback
// must be blocked, exactly as the facilitator settle client is (T6
// acceptance: "same SSRF guard as upstreams"). Tests that want to hit a local
// httptest.Server instead inject its client (see the tests above), which
// bypasses this guard the same way NewFacilitatorSettler's tests do.
func TestHTTPClassifierClient_Classify_SSRFGuard(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(policy.ClassifierVerdict{Action: policy.ActionAllow})
	}))
	defer srv.Close()

	client := NewHTTPClassifierClient(nil)
	_, err := client.Classify(context.Background(), srv.URL, policy.ClassifierRequest{TenantID: "tnt_1"})
	if err == nil {
		t.Fatal("Classify: expected the SSRF guard to block a loopback classifier URL, got nil error")
	}
}
