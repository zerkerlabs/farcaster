package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
	"github.com/zerkerlabs/farcaster/gateway/internal/auth/authtest"
	"github.com/zerkerlabs/farcaster/gateway/internal/credential"
	"github.com/zerkerlabs/farcaster/gateway/internal/httpapi"
	"github.com/zerkerlabs/farcaster/gateway/internal/kms"
)

// newCredService returns a real credential.Service backed by in-memory stores.
func newCredService(t *testing.T) *credential.Service {
	t.Helper()
	store := credential.NewMemoryStore()
	kekStore := credential.NewMemoryKEKStore()
	provider, err := kms.NewLocalProvider()
	if err != nil {
		t.Fatalf("new kms provider: %v", err)
	}
	return credential.NewService(store, kekStore, provider, credential.StubVaultResolver{})
}

// newCredHandler returns a Handler wired with a fresh credential service.
// The returned Service can be used to pre-seed credentials in setup functions.
func newCredHandler(t *testing.T) (*httpapi.Handler, *credential.Service) {
	t.Helper()
	svc := newCredService(t)
	h := httpapi.NewHandler(agent.NewMemoryStore(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.WithCredentials(svc)
	return h, svc
}

// authedCredReq builds a request to a credential endpoint with auth context.
func authedCredReq(t *testing.T, method, path string, body any, tenant, user string) *http.Request {
	t.Helper()
	var b []byte
	if raw, ok := body.([]byte); ok {
		b = raw
	} else if body != nil {
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	if len(b) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	return req.WithContext(authtest.WithIdentity(req.Context(), tenant, user))
}

// noAuthCredReq builds a request with no auth context.
func noAuthCredReq(t *testing.T, method, path string) *http.Request {
	t.Helper()
	return httptest.NewRequest(method, path, nil)
}

// serve sends req through the handler's mux and returns the recorder.
func serve(t *testing.T, h *httpapi.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// assertBody checks that the response body does NOT contain any of the
// forbidden strings. Used to enforce write-only invariant #4.
func assertBodyNotContains(t *testing.T, body []byte, forbidden ...string) {
	t.Helper()
	s := string(body)
	for _, f := range forbidden {
		if strings.Contains(s, f) {
			t.Errorf("response body must not contain %q (invariant #4), got: %s", f, s)
		}
	}
}

// seedCredential creates a managed credential in svc under testTenant and
// returns the created record.
func seedCredential(t *testing.T, svc *credential.Service, name, plaintext string) *credential.Credential {
	t.Helper()
	c, err := svc.Create(context.Background(), testTenant, credential.CreateParams{
		Name:      name,
		AuthType:  credential.AuthTypeBearer,
		Source:    credential.SourceManaged,
		Plaintext: []byte(plaintext),
	})
	if err != nil {
		t.Fatalf("seed credential %q: %v", name, err)
	}
	return c
}

// referencedSvc wraps a Service to return ErrReferenced for specific IDs on
// Delete, simulating the Postgres FK constraint that the MemoryStore does not
// enforce (spec 0002 §Q2: DELETE of a referenced credential → 409).
type referencedSvc struct {
	*credential.Service
	refID string
}

func (r *referencedSvc) Delete(ctx context.Context, tenantID, id string) error {
	if id == r.refID {
		return credential.ErrReferenced
	}
	return r.Service.Delete(ctx, tenantID, id)
}

func TestHandleCreateCredential(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       any
		noAuth     bool
		reqTenant  string
		reqUser    string
		wantStatus int
		checkBody  func(t *testing.T, body []byte)
	}{
		{
			name: "201 managed credential — secret never in response",
			body: map[string]any{
				"name":      "my-api-key",
				"auth_type": "bearer",
				"source":    "managed",
				"plaintext": "sk-supersecret",
			},
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["id"] == "" {
					t.Error("id is empty")
				}
				if resp["name"] != "my-api-key" {
					t.Errorf("name = %q, want %q", resp["name"], "my-api-key")
				}
				if resp["source"] != "managed" {
					t.Errorf("source = %q, want managed", resp["source"])
				}
				// masked_hint must be present for managed credentials.
				if resp["masked_hint"] == "" {
					t.Error("masked_hint is absent or empty for managed credential")
				}
				// Write-only invariant #4: plaintext must never appear in the response.
				assertBodyNotContains(t, b, "sk-supersecret")
				// Ciphertext internals must not appear.
				if _, ok := resp["encrypted_secret"]; ok {
					t.Error("encrypted_secret must not appear in response")
				}
				if _, ok := resp["encrypted_dek"]; ok {
					t.Error("encrypted_dek must not appear in response")
				}
				if _, ok := resp["kek_version"]; ok {
					t.Error("kek_version must not appear in response")
				}
			},
		},
		{
			name: "201 vault credential — vault_ref in response, no masked_hint",
			body: map[string]any{
				"name":      "vault-cred",
				"auth_type": "api_key",
				"source":    "vault",
				"vault_ref": "secret/data/myapp/key",
			},
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["source"] != "vault" {
					t.Errorf("source = %q, want vault", resp["source"])
				}
				if resp["vault_ref"] != "secret/data/myapp/key" {
					t.Errorf("vault_ref = %q, want secret/data/myapp/key", resp["vault_ref"])
				}
				if _, ok := resp["masked_hint"]; ok {
					t.Error("masked_hint must not appear for vault credentials")
				}
			},
		},
		{
			name:       "400 — name missing",
			body:       map[string]any{"auth_type": "bearer", "source": "managed", "plaintext": "x"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 — auth_type invalid",
			body:       map[string]any{"name": "c", "auth_type": "oauth", "source": "managed", "plaintext": "x"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 — source invalid",
			body:       map[string]any{"name": "c", "auth_type": "bearer", "source": "hsm", "plaintext": "x"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 — managed without plaintext",
			body:       map[string]any{"name": "c", "auth_type": "bearer", "source": "managed"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 — vault without vault_ref",
			body:       map[string]any{"name": "c", "auth_type": "bearer", "source": "vault"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 — malformed JSON",
			body:       []byte(`{not json`),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "401 — no auth context",
			body:       map[string]any{"name": "c", "auth_type": "bearer", "source": "managed", "plaintext": "x"},
			noAuth:     true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "409 — duplicate name within tenant",
			body: map[string]any{"name": "dup-key", "auth_type": "bearer", "source": "managed", "plaintext": "x"},
			// Pre-seeding happens in the test body below via svc.
			wantStatus: http.StatusConflict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, svc := newCredHandler(t)

			if tc.name == "409 — duplicate name within tenant" {
				seedCredential(t, svc, "dup-key", "existing-secret")
			}

			var req *http.Request
			if tc.noAuth {
				b, _ := json.Marshal(tc.body)
				req = httptest.NewRequest(http.MethodPost, "/v1/credentials", bytes.NewReader(b))
			} else {
				tenant, user := testTenant, testUser
				if tc.reqTenant != "" {
					tenant = tc.reqTenant
				}
				if tc.reqUser != "" {
					user = tc.reqUser
				}
				req = authedCredReq(t, http.MethodPost, "/v1/credentials", tc.body, tenant, user)
			}

			rec := serve(t, h, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.checkBody != nil {
				tc.checkBody(t, rec.Body.Bytes())
			}
		})
	}
}

func TestHandleGetCredential(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		id         string // overrides the seeded id when set
		noAuth     bool
		reqTenant  string
		reqUser    string
		wantStatus int
		checkBody  func(t *testing.T, body []byte)
	}{
		{
			name:       "200 — returns metadata and masked_hint, no plaintext",
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if resp["id"] == "" {
					t.Error("id is empty")
				}
				if resp["masked_hint"] == "" {
					t.Error("masked_hint is absent or empty for managed credential")
				}
				// Write-only: plaintext must never appear.
				assertBodyNotContains(t, b, "topsecret1234")
			},
		},
		{
			name:       "401 — no auth context",
			noAuth:     true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "404 — unknown id",
			id:         "cred_doesnotexist",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "404 — credential owned by different tenant",
			reqTenant:  "tenant-2",
			reqUser:    "user-2",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, svc := newCredHandler(t)
			seeded := seedCredential(t, svc, "get-test-cred", "topsecret1234")

			id := tc.id
			if id == "" {
				id = seeded.ID
			}

			var req *http.Request
			if tc.noAuth {
				req = noAuthCredReq(t, http.MethodGet, "/v1/credentials/"+id)
			} else {
				tenant, user := testTenant, testUser
				if tc.reqTenant != "" {
					tenant, user = tc.reqTenant, tc.reqUser
				}
				req = authedCredReq(t, http.MethodGet, "/v1/credentials/"+id, nil, tenant, user)
			}

			rec := serve(t, h, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.checkBody != nil {
				tc.checkBody(t, rec.Body.Bytes())
			}
		})
	}
}

func TestHandleListCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		noAuth     bool
		reqTenant  string
		reqUser    string
		wantStatus int
		checkBody  func(t *testing.T, body []byte)
	}{
		{
			name:       "200 — returns list with masked_hint, no plaintext",
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				items, _ := resp["credentials"].([]any)
				if len(items) != 2 {
					t.Fatalf("credentials count = %d, want 2", len(items))
				}
				// Write-only: no plaintext values in the list response.
				assertBodyNotContains(t, b, "secret-alpha", "secret-beta")
			},
		},
		{
			name:       "200 — tenant isolation: other tenant sees empty list",
			reqTenant:  "tenant-2",
			reqUser:    "user-2",
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				items, _ := resp["credentials"].([]any)
				if len(items) != 0 {
					t.Errorf("tenant-2 should see 0 credentials, got %d", len(items))
				}
			},
		},
		{
			name:       "401 — no auth context",
			noAuth:     true,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, svc := newCredHandler(t)
			seedCredential(t, svc, "alpha-key", "secret-alpha")
			seedCredential(t, svc, "beta-key", "secret-beta")

			var req *http.Request
			if tc.noAuth {
				req = noAuthCredReq(t, http.MethodGet, "/v1/credentials")
			} else {
				tenant, user := testTenant, testUser
				if tc.reqTenant != "" {
					tenant, user = tc.reqTenant, tc.reqUser
				}
				req = authedCredReq(t, http.MethodGet, "/v1/credentials", nil, tenant, user)
			}

			rec := serve(t, h, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.checkBody != nil {
				tc.checkBody(t, rec.Body.Bytes())
			}
		})
	}
}

func TestHandlePutCredential(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		id         string
		body       any
		noAuth     bool
		reqTenant  string
		reqUser    string
		wantStatus int
		checkBody  func(t *testing.T, body []byte)
	}{
		{
			name:       "200 — rotate plaintext; new masked_hint; old secret not in response",
			body:       map[string]any{"plaintext": "new-secret-value"},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if resp["masked_hint"] == "" {
					t.Error("masked_hint absent after rotation")
				}
				assertBodyNotContains(t, b, "new-secret-value", "original-secret")
			},
		},
		{
			name:       "200 — rename only (no plaintext rotation)",
			body:       map[string]any{"name": "renamed-key"},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, b []byte) {
				t.Helper()
				var resp map[string]any
				if err := json.Unmarshal(b, &resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if resp["name"] != "renamed-key" {
					t.Errorf("name = %q, want renamed-key", resp["name"])
				}
			},
		},
		{
			name:       "400 — empty name",
			body:       map[string]any{"name": ""},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 — invalid auth_type",
			body:       map[string]any{"auth_type": "invalid"},
			wantStatus: http.StatusBadRequest,
		},
		{
			// An explicit empty-string plaintext must be rejected, not treated as
			// "rotate to empty" (which would zero out the stored secret).
			name:       "400 — empty plaintext",
			body:       map[string]any{"plaintext": ""},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 — malformed JSON",
			body:       []byte(`{bad`),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "401 — no auth context",
			noAuth:     true,
			body:       map[string]any{"plaintext": "x"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "404 — unknown id",
			id:         "cred_doesnotexist",
			body:       map[string]any{"plaintext": "x"},
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "404 — credential owned by different tenant",
			reqTenant:  "tenant-2",
			reqUser:    "user-2",
			body:       map[string]any{"plaintext": "x"},
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, svc := newCredHandler(t)
			seeded := seedCredential(t, svc, "put-test-cred", "original-secret")

			id := tc.id
			if id == "" {
				id = seeded.ID
			}

			var req *http.Request
			if tc.noAuth {
				b, _ := json.Marshal(tc.body)
				req = httptest.NewRequest(http.MethodPut, "/v1/credentials/"+id, bytes.NewReader(b))
			} else {
				tenant, user := testTenant, testUser
				if tc.reqTenant != "" {
					tenant, user = tc.reqTenant, tc.reqUser
				}
				req = authedCredReq(t, http.MethodPut, "/v1/credentials/"+id, tc.body, tenant, user)
			}

			rec := serve(t, h, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.checkBody != nil {
				tc.checkBody(t, rec.Body.Bytes())
			}
		})
	}
}

func TestHandleDeleteCredential(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		id         string
		noAuth     bool
		reqTenant  string
		reqUser    string
		useRefSvc  bool // inject ErrReferenced on Delete for the seeded credential
		wantStatus int
	}{
		{
			name:       "204 — deletes credential",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "401 — no auth context",
			noAuth:     true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "404 — unknown id",
			id:         "cred_doesnotexist",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "404 — credential owned by different tenant",
			reqTenant:  "tenant-2",
			reqUser:    "user-2",
			wantStatus: http.StatusNotFound,
		},
		{
			// spec 0002 §Q2: DELETE of a credential still referenced by an agent → 409
			name:       "409 — credential is still referenced by an agent",
			useRefSvc:  true,
			wantStatus: http.StatusConflict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svcReal := newCredService(t)
			seeded := seedCredential(t, svcReal, "del-test-cred", "del-secret")

			// Build the handler, optionally wrapping the service to inject 409.
			h := httpapi.NewHandler(agent.NewMemoryStore(), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if tc.useRefSvc {
				h.WithCredentials(&referencedSvc{Service: svcReal, refID: seeded.ID})
			} else {
				h.WithCredentials(svcReal)
			}

			id := tc.id
			if id == "" {
				id = seeded.ID
			}

			var req *http.Request
			if tc.noAuth {
				req = noAuthCredReq(t, http.MethodDelete, "/v1/credentials/"+id)
			} else {
				tenant, user := testTenant, testUser
				if tc.reqTenant != "" {
					tenant, user = tc.reqTenant, tc.reqUser
				}
				req = authedCredReq(t, http.MethodDelete, "/v1/credentials/"+id, nil, tenant, user)
			}

			rec := serve(t, h, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}
