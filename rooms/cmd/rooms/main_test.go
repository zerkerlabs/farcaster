package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
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
