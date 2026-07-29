package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
