package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/auth"
)

// testOIDCServer is a minimal in-process OIDC provider: it exposes an
// OpenID Configuration discovery endpoint and a JWKS endpoint backed by a
// generated RSA key pair. It mirrors the gateway's own test OIDC server
// (gateway/internal/auth/auth_test.go), since Rooms validates tokens the same
// way the gateway does but cannot import its test helpers across a module
// boundary.
type testOIDCServer struct {
	*httptest.Server
	key   *rsa.PrivateKey
	keyID string
}

func newTestOIDCServer(t *testing.T) *testOIDCServer {
	t.Helper()

	const keyID = "test-key-1"

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	var serverURL string

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		doc := map[string]string{
			"issuer":   serverURL,
			"jwks_uri": serverURL + "/jwks.json",
		}
		if encErr := json.NewEncoder(w).Encode(doc); encErr != nil {
			http.Error(w, "encode error", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"keys": []any{rsaJWK(keyID, &key.PublicKey)},
		}
		if encErr := json.NewEncoder(w).Encode(body); encErr != nil {
			http.Error(w, "encode error", http.StatusInternalServerError)
		}
	})

	srv := httptest.NewServer(mux)
	serverURL = srv.URL
	t.Cleanup(srv.Close)

	return &testOIDCServer{Server: srv, key: key, keyID: keyID}
}

func (s *testOIDCServer) mint(claims map[string]any) (string, error) {
	return signedJWT(s.key, s.keyID, claims)
}

func signedJWT(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{
		"alg": "RS256",
		"kid": kid,
		"typ": "JWT",
	})
	if err != nil {
		return "", err
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	headerEnc := base64.RawURLEncoding.EncodeToString(header)
	payloadEnc := base64.RawURLEncoding.EncodeToString(payload)
	sigInput := headerEnc + "." + payloadEnc

	h := sha256.Sum256([]byte(sigInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}

	return sigInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func rsaJWK(kid string, pub *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func cloneClaims(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func TestNewMiddleware(t *testing.T) {
	t.Parallel()

	srv := newTestOIDCServer(t)

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

	now := time.Now()

	validClaims := map[string]any{
		"iss":       srv.URL,
		"aud":       []string{audience},
		tenantClaim: testTenant,
		userClaim:   testUser,
		"exp":       now.Add(time.Hour).Unix(),
		"iat":       now.Unix(),
	}

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

	validTok, err := srv.mint(validClaims)
	if err != nil {
		t.Fatalf("mint valid token: %v", err)
	}

	expiredClaims := cloneClaims(validClaims)
	expiredClaims["exp"] = now.Add(-time.Hour).Unix()
	expiredClaims["iat"] = now.Add(-2 * time.Hour).Unix()
	expiredTok, err := srv.mint(expiredClaims)
	if err != nil {
		t.Fatalf("mint expired token: %v", err)
	}

	wrongAudClaims := cloneClaims(validClaims)
	wrongAudClaims["aud"] = []string{"wrong-audience"}
	wrongAudTok, err := srv.mint(wrongAudClaims)
	if err != nil {
		t.Fatalf("mint wrong-audience token: %v", err)
	}

	// Token is signed with badKey but claims srv.keyID as the kid. go-oidc
	// fetches the key for that kid (the valid public key) and the signature
	// check fails because the signing key does not match.
	badSigTok, err := signedJWT(badKey, srv.keyID, validClaims)
	if err != nil {
		t.Fatalf("mint bad-signature token: %v", err)
	}

	noTenantClaims := cloneClaims(validClaims)
	delete(noTenantClaims, tenantClaim)
	noTenantTok, err := srv.mint(noTenantClaims)
	if err != nil {
		t.Fatalf("mint no-tenant token: %v", err)
	}

	noUserClaims := cloneClaims(validClaims)
	delete(noUserClaims, userClaim)
	noUserTok, err := srv.mint(noUserClaims)
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
