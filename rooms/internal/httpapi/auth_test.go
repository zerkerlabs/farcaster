package httpapi_test

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/auth/authtest"
)

// TestRoomRoutesRequireValidBearerToken checks every v1 room route against
// every way a credential can be unacceptable. The auth package's own tests
// cover the middleware in isolation; this covers the routes as mounted, so a
// route that gets registered outside the middleware later fails here.
//
// Each rejection is a 401 with an empty body: a caller learns its credential
// was not accepted and nothing else — not which check failed, not whether the
// room it named exists (AGENTS.md invariants #2 and #3).
func TestRoomRoutesRequireValidBearerToken(t *testing.T) {
	t.Parallel()

	// A second issuer, with its own key, that the middleware is not configured
	// to trust — a well-formed token from the wrong identity provider.
	otherIssuer := authtest.New()
	t.Cleanup(otherIssuer.Close)

	foreignKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate foreign RSA key: %v", err)
	}

	validClaims := oidc.Claims(testAudience, tenantClaim, tenantA, userClaim, "user-xyz")

	expiredClaims := authtest.CloneClaims(validClaims)
	expiredClaims["exp"] = time.Now().Add(-time.Hour).Unix()
	expiredClaims["iat"] = time.Now().Add(-2 * time.Hour).Unix()

	wrongAudClaims := authtest.CloneClaims(validClaims)
	wrongAudClaims["aud"] = []string{"some-other-service"}

	noTenantClaims := authtest.CloneClaims(validClaims)
	delete(noTenantClaims, tenantClaim)

	// Signed with a key the trusted issuer never published, but naming the kid
	// it did: the signature check, not key lookup, is what must reject this.
	forgedSig, err := authtest.SignJWT(foreignKey, authtest.KeyID, validClaims)
	if err != nil {
		t.Fatalf("sign forged token: %v", err)
	}

	credentials := []struct {
		name   string
		header string
	}{
		{"no authorization header", ""},
		{"wrong scheme", "Token " + mintToken(t, validClaims)},
		{"bearer with no token", "Bearer "},
		{"malformed token", "Bearer not.a.jwt"},
		{"expired token", "Bearer " + mintToken(t, expiredClaims)},
		{"wrong audience", "Bearer " + mintToken(t, wrongAudClaims)},
		{"wrong issuer", "Bearer " + mintToken2(t, otherIssuer, otherIssuer.Claims(testAudience, tenantClaim, tenantA, userClaim, "user-xyz"))},
		{"forged signature", "Bearer " + forgedSig},
		{"no tenant claim", "Bearer " + mintToken(t, noTenantClaims)},
	}

	mux, store := newMux(t)
	r := mustCreateRoom(t, store, "goal")

	routes := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"create room", http.MethodPost, "/v1/rooms", map[string]any{"goal": "goal"}},
		{"get room", http.MethodGet, "/v1/rooms/" + r.ID, nil},
		{"add member", http.MethodPost, "/v1/rooms/" + r.ID + "/members", map[string]any{"agent_id": "agt_x"}},
		{"post message", http.MethodPost, "/v1/rooms/" + r.ID + "/messages", map[string]any{"member_id": "mem_x", "body": "hi"}},
		{"get receipts", http.MethodGet, "/v1/rooms/" + r.ID + "/receipts", nil},
	}

	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()

			for _, cred := range credentials {
				t.Run(cred.name, func(t *testing.T) {
					t.Parallel()

					req := request(t, route.method, route.path, route.body)
					if cred.header != "" {
						req.Header.Set("Authorization", cred.header)
					}

					rec := httptest.NewRecorder()
					mux.ServeHTTP(rec, req)

					if rec.Code != http.StatusUnauthorized {
						t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
					}
					if rec.Body.Len() != 0 {
						t.Errorf("body = %q, want empty", rec.Body.String())
					}
				})
			}
		})
	}
}

// TestRoomRoutesAcceptValidBearerToken is the other half of the table above:
// the same routes, with a good token, must not 401. Without it, a middleware
// that rejected everything would pass the rejection cases perfectly.
func TestRoomRoutesAcceptValidBearerToken(t *testing.T) {
	t.Parallel()

	mux, store := newMux(t)
	r := mustCreateRoom(t, store, "goal")
	m := mustAddMember(t, store, r.ID, "agt_x")

	tests := []struct {
		name     string
		method   string
		path     string
		body     any
		wantCode int
	}{
		{"create room", http.MethodPost, "/v1/rooms", map[string]any{"goal": "goal"}, http.StatusCreated},
		{"get room", http.MethodGet, "/v1/rooms/" + r.ID, nil, http.StatusOK},
		{"add member", http.MethodPost, "/v1/rooms/" + r.ID + "/members", map[string]any{"agent_id": "agt_y"}, http.StatusCreated},
		{"post message", http.MethodPost, "/v1/rooms/" + r.ID + "/messages", map[string]any{"member_id": m.ID, "body": "hi"}, http.StatusCreated},
		{"get receipts", http.MethodGet, "/v1/rooms/" + r.ID + "/receipts", nil, http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, requestAs(t, tt.method, tt.path, tt.body, tenantA))

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

// TestTenantComesFromTokenNotRequest verifies the tenant is taken from the
// token's claims and from nowhere else: a request that names another tenant in
// its body, in a header, or in a query parameter is still acting as the tenant
// its token asserts, so it cannot reach that tenant's room.
func TestTenantComesFromTokenNotRequest(t *testing.T) {
	t.Parallel()

	mux, store := newMux(t)
	r := mustCreateRoom(t, store, "goal")

	// Authenticated as tenantB, claiming tenantA every other way a request can.
	req := requestAs(t, http.MethodGet, "/v1/rooms/"+r.ID+"?tenant="+tenantA, nil, tenantB)
	req.Header.Set("X-Tenant-Id", tenantA)
	req.Header.Set("X-Tenant", tenantA)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (tenantA's room must stay invisible to tenantB); body = %s",
			rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// mintToken2 mints from a caller-supplied issuer, for the wrong-issuer case;
// mintToken uses the one the middleware trusts.
func mintToken2(t *testing.T, srv *authtest.Server, claims map[string]any) string {
	t.Helper()
	tok, err := srv.Mint(claims)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}
