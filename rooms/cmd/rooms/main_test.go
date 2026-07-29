package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/farcaster/rooms/internal/auth/authtest"
)

func TestOperationalRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		method   string
		path     string
		wantCode int
		wantKeys []string
	}{
		{"healthz reports ok", http.MethodGet, "/healthz", http.StatusOK, []string{"status"}},
		{"version reports build metadata", http.MethodGet, "/version", http.StatusOK, []string{"version", "commit"}},
		{"unknown route is 404", http.MethodGet, "/nope", http.StatusNotFound, nil},
		{"healthz rejects POST", http.MethodPost, "/healthz", http.StatusMethodNotAllowed, nil},
	}

	// The operational route tests never exercise message delivery, so a nil
	// gateway client (never called) is fine here.
	mux := newMux(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if len(tt.wantKeys) == 0 {
				return
			}

			var body map[string]string
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			for _, k := range tt.wantKeys {
				if body[k] == "" {
					t.Errorf("body[%q] is empty, want a value", k)
				}
			}
		})
	}
}

// The room routes are only protected if the middleware is actually wired into
// what the server serves. Handler and middleware tests both pass with a
// perfectly good auth package that main.go never calls, so this exercises the
// composed handler: an unauthenticated room request must be refused, while the
// operational routes stay reachable.
func TestHandlerRequiresBearerTokenForRoomRoutes(t *testing.T) {
	oidc := authtest.New()
	t.Cleanup(oidc.Close)

	const (
		audience    = "rooms-test-audience"
		tenantClaim = "org_id"
	)

	t.Setenv("ROOMS_OIDC_ISSUER", oidc.URL)
	t.Setenv("ROOMS_OIDC_AUDIENCE", audience)
	t.Setenv("ROOMS_OIDC_TENANT_CLAIM", tenantClaim)

	// Nil gateway client: no test here posts an addressed message, the only
	// path that calls it.
	handler, err := newHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}

	token, err := oidc.Mint(oidc.Claims(audience, tenantClaim, "tenant-alpha", "sub", "user-xyz"))
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	tests := []struct {
		name     string
		method   string
		path     string
		token    string
		wantCode int
	}{
		{"room route without a token is refused", http.MethodPost, "/v1/rooms", "", http.StatusUnauthorized},
		{"room route with a garbage token is refused", http.MethodPost, "/v1/rooms", "notavalidjwt", http.StatusUnauthorized},
		{"room route with a valid token reaches the handler", http.MethodPost, "/v1/rooms", token, http.StatusBadRequest},
		{"healthz stays open", http.MethodGet, "/healthz", "", http.StatusOK},
		{"version stays open", http.MethodGet, "/version", "", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			// The valid-token case sends no body, so 400 from the handler is
			// the tell that authentication passed and the request got through.
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

// Rooms must not start without authentication configured: an operator who
// forgets the OIDC variables gets a failed start, never an open server.
func TestNewHandlerFailsWithoutOIDCConfig(t *testing.T) {
	t.Setenv("ROOMS_OIDC_ISSUER", "")
	t.Setenv("ROOMS_OIDC_AUDIENCE", "")

	if _, err := newHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), nil); err == nil {
		t.Fatal("newHandler with no OIDC config: want error, got nil")
	}
}

// The operational routes must not leak configuration. /version reports build
// metadata and nothing else, so a listen address or env value can never appear
// there by accident as the service grows (AGENTS.md invariant #9).
func TestVersionExposesOnlyBuildMetadata(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	newMux(slog.New(slog.NewTextHandler(io.Discard, nil)), nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version", nil))

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for k := range body {
		if k != "version" && k != "commit" {
			t.Errorf("unexpected key %q in /version response", k)
		}
	}
}
