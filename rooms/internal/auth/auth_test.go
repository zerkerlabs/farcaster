package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/auth"
	"github.com/zerkerlabs/farcaster/rooms/internal/auth/authtest"
)

func TestNewMiddleware(t *testing.T) {
	t.Parallel()

	srv := authtest.New()
	t.Cleanup(srv.Close)

	badKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate bad RSA key: %v", err)
	}

	const (
		audience    = "rooms-test-audience"
		tenantClaim = "org_id"
		userClaim   = "sub"
		testTenant  = "tenant-abc"
		testUser    = "user-xyz"
	)

	validClaims := srv.Claims(audience, tenantClaim, testTenant, userClaim, testUser)

	cfg := auth.Config{
		IssuerURL:   srv.URL,
		Audience:    audience,
		TenantClaim: tenantClaim,
		UserClaim:   userClaim,
		HTTPClient:  srv.Client(),
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mw, err := auth.NewMiddleware(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("NewMiddleware: %v", err)
	}

	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tenant", auth.TenantFromContext(r.Context()))
		w.Header().Set("X-User", auth.UserFromContext(r.Context()))
		w.WriteHeader(http.StatusOK)
	})

	handler := mw(downstream)

	validTok, err := srv.Mint(validClaims)
	if err != nil {
		t.Fatalf("mint valid token: %v", err)
	}

	now := time.Now()

	expiredClaims := authtest.CloneClaims(validClaims)
	expiredClaims["exp"] = now.Add(-time.Hour).Unix()
	expiredClaims["iat"] = now.Add(-2 * time.Hour).Unix()
	expiredTok, err := srv.Mint(expiredClaims)
	if err != nil {
		t.Fatalf("mint expired token: %v", err)
	}

	wrongAudClaims := authtest.CloneClaims(validClaims)
	wrongAudClaims["aud"] = []string{"wrong-audience"}
	wrongAudTok, err := srv.Mint(wrongAudClaims)
	if err != nil {
		t.Fatalf("mint wrong-audience token: %v", err)
	}

	// Token is signed with badKey but claims authtest.KeyID as the kid. go-oidc
	// fetches the key for that kid (the valid public key) and the signature
	// check fails because the signing key does not match.
	badSigTok, err := authtest.SignJWT(badKey, authtest.KeyID, validClaims)
	if err != nil {
		t.Fatalf("mint bad-signature token: %v", err)
	}

	noTenantClaims := authtest.CloneClaims(validClaims)
	delete(noTenantClaims, tenantClaim)
	noTenantTok, err := srv.Mint(noTenantClaims)
	if err != nil {
		t.Fatalf("mint no-tenant token: %v", err)
	}

	noUserClaims := authtest.CloneClaims(validClaims)
	delete(noUserClaims, userClaim)
	noUserTok, err := srv.Mint(noUserClaims)
	if err != nil {
		t.Fatalf("mint no-user token: %v", err)
	}

	tests := []struct {
		name       string
		path       string
		authHeader string
		wantStatus int
		wantTenant string
		wantUser   string
	}{
		{
			name:       "valid token sets context claims",
			path:       "/v1/rooms",
			authHeader: "Bearer " + validTok,
			wantStatus: http.StatusOK,
			wantTenant: testTenant,
			wantUser:   testUser,
		},
		{
			name:       "no auth header",
			path:       "/v1/rooms",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong bearer prefix",
			path:       "/v1/rooms",
			authHeader: "Token " + validTok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "bearer with empty token",
			path:       "/v1/rooms",
			authHeader: "Bearer ",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "garbage token value",
			path:       "/v1/rooms",
			authHeader: "Bearer notavalidjwt",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "expired token",
			path:       "/v1/rooms",
			authHeader: "Bearer " + expiredTok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong audience",
			path:       "/v1/rooms",
			authHeader: "Bearer " + wrongAudTok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "bad signature",
			path:       "/v1/rooms",
			authHeader: "Bearer " + badSigTok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing tenant claim",
			path:       "/v1/rooms",
			authHeader: "Bearer " + noTenantTok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing user claim",
			path:       "/v1/rooms",
			authHeader: "Bearer " + noUserTok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "healthz bypasses auth",
			path:       "/healthz",
			wantStatus: http.StatusOK,
		},
		{
			name:       "version bypasses auth",
			path:       "/version",
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantTenant != "" && rec.Header().Get("X-Tenant") != tc.wantTenant {
				t.Errorf("X-Tenant = %q, want %q", rec.Header().Get("X-Tenant"), tc.wantTenant)
			}
			if tc.wantUser != "" && rec.Header().Get("X-User") != tc.wantUser {
				t.Errorf("X-User = %q, want %q", rec.Header().Get("X-User"), tc.wantUser)
			}
		})
	}
}

// TestMiddleware_ClassifiesVerificationFailures drives real tokens through
// every verification failure mode classifyVerifyError recognizes and asserts
// the log carries the expected stable category. For audience and issuer
// mismatches, go-oidc's error text embeds the token's own claim value to
// describe the mismatch; those cases also assert the captured log never
// contains that value, since a rejected token's claims are as sensitive as
// the token itself (AGENTS.md invariant #4).
//
// This test is pinned to the go-oidc version in go.mod: it exercises the
// library's actual error text rather than synthetic strings, so a dependency
// bump that rewords a message is caught here (the case falls through to
// "unknown") instead of silently degrading log categories in production.
func TestMiddleware_ClassifiesVerificationFailures(t *testing.T) {
	t.Parallel()

	srv := authtest.New()
	t.Cleanup(srv.Close)

	const (
		audience    = "rooms-test-audience"
		tenantClaim = "org_id"
		userClaim   = "sub"
		leakTag     = "leaked-claim-4f9c2e1b"
	)
	now := time.Now()

	baseClaims := func() map[string]any {
		return srv.Claims(audience, tenantClaim, "tenant-abc", userClaim, "user-xyz")
	}

	cfg := auth.Config{
		IssuerURL:   srv.URL,
		Audience:    audience,
		TenantClaim: tenantClaim,
		UserClaim:   userClaim,
		HTTPClient:  srv.Client(),
	}

	tests := []struct {
		name         string
		mint         func(t *testing.T) string
		wantCategory string
		leakedValue  string // if non-empty, asserted absent from the log
	}{
		{
			name: "expired token",
			mint: func(t *testing.T) string {
				claims := authtest.CloneClaims(baseClaims())
				claims["exp"] = now.Add(-time.Hour).Unix()
				claims["iat"] = now.Add(-2 * time.Hour).Unix()
				tok, err := srv.Mint(claims)
				if err != nil {
					t.Fatalf("mint: %v", err)
				}
				return tok
			},
			wantCategory: "expired",
		},
		{
			name: "audience mismatch",
			mint: func(t *testing.T) string {
				claims := authtest.CloneClaims(baseClaims())
				claims["aud"] = []string{leakTag}
				tok, err := srv.Mint(claims)
				if err != nil {
					t.Fatalf("mint: %v", err)
				}
				return tok
			},
			wantCategory: "audience_mismatch",
			leakedValue:  leakTag,
		},
		{
			name: "issuer mismatch",
			mint: func(t *testing.T) string {
				claims := authtest.CloneClaims(baseClaims())
				claims["iss"] = leakTag
				tok, err := srv.Mint(claims)
				if err != nil {
					t.Fatalf("mint: %v", err)
				}
				return tok
			},
			wantCategory: "issuer_mismatch",
			leakedValue:  leakTag,
		},
		{
			name: "token not yet valid",
			mint: func(t *testing.T) string {
				claims := authtest.CloneClaims(baseClaims())
				claims["nbf"] = now.Add(time.Hour).Unix()
				tok, err := srv.Mint(claims)
				if err != nil {
					t.Fatalf("mint: %v", err)
				}
				return tok
			},
			wantCategory: "not_yet_valid",
		},
		{
			name: "bad signature",
			mint: func(t *testing.T) string {
				badKey, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatalf("generate bad key: %v", err)
				}
				tok, err := authtest.SignJWT(badKey, authtest.KeyID, baseClaims())
				if err != nil {
					t.Fatalf("mint: %v", err)
				}
				return tok
			},
			wantCategory: "signature_invalid",
		},
		{
			name: "unresolvable distributed claim source",
			mint: func(t *testing.T) string {
				claims := authtest.CloneClaims(baseClaims())
				claims["_claim_names"] = map[string]string{"extra": ""}
				tok, err := srv.Mint(claims)
				if err != nil {
					t.Fatalf("mint: %v", err)
				}
				return tok
			},
			wantCategory: "claims_invalid",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var logBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logBuf, nil))
			mw, err := auth.NewMiddleware(context.Background(), cfg, logger)
			if err != nil {
				t.Fatalf("NewMiddleware: %v", err)
			}
			handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/v1/rooms", nil)
			req.Header.Set("Authorization", "Bearer "+tc.mint(t))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if body := rec.Body.String(); body != "" {
				t.Errorf("response body = %q, want empty", body)
			}

			logged := logBuf.String()
			if !strings.Contains(logged, tc.wantCategory) {
				t.Errorf("log does not classify the failure as %s; log:\n%s", tc.wantCategory, logged)
			}
			if tc.leakedValue != "" && strings.Contains(logged, tc.leakedValue) {
				t.Errorf("log contains the token's claim value %q; log:\n%s", tc.leakedValue, logged)
			}
		})
	}
}

func TestContextAccessors_EmptyContext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	if v := auth.TenantFromContext(ctx); v != "" {
		t.Errorf("TenantFromContext on empty context = %q, want empty string", v)
	}
	if v := auth.UserFromContext(ctx); v != "" {
		t.Errorf("UserFromContext on empty context = %q, want empty string", v)
	}
}

func TestNewMiddleware_MissingConfig(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := auth.NewMiddleware(context.Background(), auth.Config{Audience: "aud"}, logger); err == nil {
		t.Error("NewMiddleware with no IssuerURL: want error, got nil")
	}
	if _, err := auth.NewMiddleware(context.Background(), auth.Config{IssuerURL: "http://example.invalid"}, logger); err == nil {
		t.Error("NewMiddleware with no Audience: want error, got nil")
	}
}
