// Package authtest provides an in-process OIDC provider for tests that need
// real bearer tokens: it serves a discovery document and a JWKS backed by a
// generated RSA key pair, and mints RS256 tokens signed by that key.
//
// It exists so tests exercise the actual auth middleware — token in an
// Authorization header, signature and claims verified by go-oidc — rather than
// injecting an identity into a request context behind the middleware's back. A
// context seam would let a handler pass its tests while the running server has
// no authentication wired at all.
//
// It mirrors the gateway's own test OIDC provider; Rooms validates tokens the
// same way the gateway does but cannot import its helpers across the module
// boundary.
package authtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"time"
)

// KeyID is the "kid" the server publishes in its JWKS and stamps on every
// token it mints. Exported so a test can mint a token that names this key but
// is signed with a different one (the bad-signature case).
const KeyID = "test-key-1"

// Server is an in-process OIDC provider. The embedded *httptest.Server's URL
// is the issuer; pass it as auth.Config.IssuerURL and pass Client() as
// auth.Config.HTTPClient so discovery and JWKS fetches reach it.
//
// Close it when the tests using it are done — from t.Cleanup for a per-test
// server, or after m.Run for a package-level one.
type Server struct {
	*httptest.Server

	key *rsa.PrivateKey
}

// New starts an OIDC provider serving discovery and JWKS endpoints.
//
// It panics rather than returning an error: it is a test helper whose only
// failure modes are key generation and port binding, neither of which a test
// can meaningfully recover from, and panicking keeps call sites at one line
// even in a TestMain that has no *testing.T to fail.
func New() *Server {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(fmt.Sprintf("authtest: generate RSA key: %v", err))
	}

	// The discovery document must echo the issuer URL the client used, which
	// httptest only assigns once the server is listening — hence the closure
	// over a variable filled in below.
	var serverURL string

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{
			"issuer":   serverURL,
			"jwks_uri": serverURL + "/jwks.json",
		})
	})
	mux.HandleFunc("GET /jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []any{rsaJWK(KeyID, &key.PublicKey)}})
	})

	srv := httptest.NewServer(mux)
	serverURL = srv.URL

	return &Server{Server: srv, key: key}
}

// Mint returns an RS256 token carrying claims, signed with the server's key
// and naming KeyID, so the middleware accepts it as genuinely issued.
//
// Claims are passed verbatim: a test that wants an expired, wrong-audience, or
// claim-less token builds those claims itself. Claims returns a valid starting
// set to modify.
func (s *Server) Mint(claims map[string]any) (string, error) {
	return SignJWT(s.key, KeyID, claims)
}

// Claims returns the claim set of a valid token for tenant and user: issued by
// this server, for audience, expiring an hour out. tenantClaim and userClaim
// name the claims carrying the identity, matching the middleware's Config.
//
// Callers modify the result to build the rejection cases — overwrite "exp" for
// an expired token, "aud" for the wrong audience, delete a claim for a token
// that carries no identity.
func (s *Server) Claims(audience, tenantClaim, tenant, userClaim, user string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":       s.URL,
		"aud":       []string{audience},
		"exp":       now.Add(time.Hour).Unix(),
		"iat":       now.Unix(),
		tenantClaim: tenant,
		userClaim:   user,
	}
}

// SignJWT signs claims as an RS256 JWT with key, stamping kid in the header.
//
// Exported for the bad-signature case: sign with a key the server never
// published while naming a kid it did, so the middleware fetches the real
// public key and the signature check fails.
func SignJWT(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
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

	sigInput := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload)

	digest := sha256.Sum256([]byte(sigInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}

	return sigInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// CloneClaims returns a shallow copy of src, so a test can vary one claim of a
// valid set without disturbing the original.
func CloneClaims(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
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

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}
