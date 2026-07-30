package auth_test

import (
	"bytes"
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
	"strings"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/gateway/internal/auth"
)

// testOIDCServer is a minimal in-process OIDC provider: it exposes an
// OpenID Configuration discovery endpoint and a JWKS endpoint backed by a
// generated RSA key pair.
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

	// serverURL is set after the server starts; the handlers close over it so
	// they see the value at request time, not at registration time.
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

// mint creates a signed RS256 JWT using the server's private key.
func (s *testOIDCServer) mint(claims map[string]any) (string, error) {
	return signedJWT(s.key, s.keyID, claims)
}

// signedJWT builds a minimal RS256 JWT from the given claims.
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

// rsaJWK returns the JWK representation of an RSA public key.
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

// cloneClaims makes a shallow copy of a claims map. This is sufficient because
// we replace map values wholesale (not mutate nested structures) in the tests.
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

	// Generate a second key for the bad-signature case. It is NOT registered in
	// the test server's JWKS, so tokens signed with it fail signature verification.
	badKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate bad RSA key: %v", err)
	}

	const (
		audience    = "test-audience"
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

	// downstream echos context claims as response headers for assertion.
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tenant", auth.TenantFromContext(r.Context()))
		w.Header().Set("X-User", auth.UserFromContext(r.Context()))
		w.WriteHeader(http.StatusOK)
	})

	handler := mw(downstream)

	// --- pre-mint test tokens ---

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

	// Token is signed with badKey but claims srv.keyID as the kid.
	// go-oidc fetches the key for that kid (the valid public key) and
	// the signature check fails because the signing key does not match.
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
			path:       "/v1/agents",
			authHeader: "Bearer " + validTok,
			wantStatus: http.StatusOK,
			wantTenant: testTenant,
			wantUser:   testUser,
		},
		{
			name:       "no auth header",
			path:       "/v1/agents",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong bearer prefix",
			path:       "/v1/agents",
			authHeader: "Token " + validTok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "bearer with empty token",
			path:       "/v1/agents",
			authHeader: "Bearer ",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "garbage token value",
			path:       "/v1/agents",
			authHeader: "Bearer notavalidjwt",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "expired token",
			path:       "/v1/agents",
			authHeader: "Bearer " + expiredTok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong audience",
			path:       "/v1/agents",
			authHeader: "Bearer " + wrongAudTok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "bad signature",
			path:       "/v1/agents",
			authHeader: "Bearer " + badSigTok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing tenant claim",
			path:       "/v1/agents",
			authHeader: "Bearer " + noTenantTok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing user claim",
			path:       "/v1/agents",
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

func TestNewMiddleware_MissingConfig(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := auth.NewMiddleware(context.Background(), auth.Config{Audience: "aud", TenantClaim: "org_id"}, logger); err == nil {
		t.Error("NewMiddleware with no IssuerURL: want error, got nil")
	}
	if _, err := auth.NewMiddleware(context.Background(), auth.Config{IssuerURL: "http://example.invalid", TenantClaim: "org_id"}, logger); err == nil {
		t.Error("NewMiddleware with no Audience: want error, got nil")
	}
	if _, err := auth.NewMiddleware(context.Background(), auth.Config{IssuerURL: "http://example.invalid", Audience: "aud"}, logger); err == nil {
		t.Error("NewMiddleware with no TenantClaim: want error, got nil")
	}
}

// TestMiddleware_MalformedScopeClaim verifies that when ScopeClaim points at a
// claim whose value is neither a string nor an array (a likely operator
// misconfiguration), the request still succeeds with zero scopes (fail-closed)
// AND the middleware logs a diagnostic warning — so the misconfiguration is not
// silent. A well-formed claim extracts scopes and logs nothing.
func TestMiddleware_MalformedScopeClaim(t *testing.T) {
	t.Parallel()

	srv := newTestOIDCServer(t)

	const (
		audience    = "test-audience"
		tenantClaim = "org_id"
		userClaim   = "sub"
		scopeClaim  = "scope"
	)
	now := time.Now()

	baseClaims := map[string]any{
		"iss":       srv.URL,
		"aud":       []string{audience},
		tenantClaim: "tenant-abc",
		userClaim:   "user-xyz",
		"exp":       now.Add(time.Hour).Unix(),
		"iat":       now.Unix(),
	}

	cfg := auth.Config{
		IssuerURL:   srv.URL,
		Audience:    audience,
		TenantClaim: tenantClaim,
		UserClaim:   userClaim,
		ScopeClaim:  scopeClaim,
		HTTPClient:  srv.Client(),
	}

	// downstream echoes the extracted scopes so the test can assert them.
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Scopes", strings.Join(auth.ScopesFromContext(r.Context()), ","))
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name       string
		scope      any
		wantScopes string
		wantWarn   bool
	}{
		{
			name:       "well-formed array claim yields scopes without warning",
			scope:      []string{"invocations:read_body", "agents:read"},
			wantScopes: "invocations:read_body,agents:read",
			wantWarn:   false,
		},
		{
			name:       "numeric claim is malformed: zero scopes and a warning",
			scope:      123,
			wantScopes: "",
			wantWarn:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			claims := cloneClaims(baseClaims)
			claims[scopeClaim] = tc.scope
			tok, err := srv.mint(claims)
			if err != nil {
				t.Fatalf("mint token: %v", err)
			}

			var logBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logBuf, nil))
			mw, err := auth.NewMiddleware(context.Background(), cfg, logger)
			if err != nil {
				t.Fatalf("NewMiddleware: %v", err)
			}
			handler := mw(downstream)

			req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (request succeeds; scopes fail-closed)", rec.Code)
			}
			if got := rec.Header().Get("X-Scopes"); got != tc.wantScopes {
				t.Errorf("scopes = %q, want %q", got, tc.wantScopes)
			}
			warned := strings.Contains(logBuf.String(), "scope claim present but not a string")
			if warned != tc.wantWarn {
				t.Errorf("malformed-scope warning logged = %v, want %v; log:\n%s",
					warned, tc.wantWarn, logBuf.String())
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

	srv := newTestOIDCServer(t)

	const (
		audience    = "test-audience"
		tenantClaim = "org_id"
		userClaim   = "sub"
		leakTag     = "leaked-claim-4f9c2e1b"
	)
	now := time.Now()

	baseClaims := func() map[string]any {
		return map[string]any{
			"iss":       srv.URL,
			"aud":       []string{audience},
			tenantClaim: "tenant-abc",
			userClaim:   "user-xyz",
			"exp":       now.Add(time.Hour).Unix(),
			"iat":       now.Unix(),
		}
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
				claims := cloneClaims(baseClaims())
				claims["exp"] = now.Add(-time.Hour).Unix()
				claims["iat"] = now.Add(-2 * time.Hour).Unix()
				tok, err := srv.mint(claims)
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
				claims := cloneClaims(baseClaims())
				claims["aud"] = []string{leakTag}
				tok, err := srv.mint(claims)
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
				claims := cloneClaims(baseClaims())
				claims["iss"] = leakTag
				tok, err := srv.mint(claims)
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
				claims := cloneClaims(baseClaims())
				claims["nbf"] = now.Add(time.Hour).Unix()
				tok, err := srv.mint(claims)
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
				tok, err := signedJWT(badKey, srv.keyID, baseClaims())
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
				claims := cloneClaims(baseClaims())
				claims["_claim_names"] = map[string]string{"extra": ""}
				tok, err := srv.mint(claims)
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

			req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
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
