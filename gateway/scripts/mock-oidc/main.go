// Command mock-oidc is a dev-only OpenID Connect issuer for running Zerker
// locally with auth enforced. It is NOT for production: it mints a token for a
// fixed identity and serves discovery + JWKS over plain HTTP.
//
// It mirrors the RS256 / JWKS / discovery shape exercised by
// internal/auth/auth_test.go, but as a long-lived server rather than a test
// helper, so the gateway's auth middleware can initialise against it and verify
// the bearer token it mints.
//
// Configurable via flags; scripts/dev-auth.sh wires it to the gateway.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9099", "listen address")
	issuer := flag.String("issuer", "http://127.0.0.1:9099", "issuer URL (must match ZERKER_OIDC_ISSUER)")
	audience := flag.String("audience", "zerker-gateway", "aud claim (must match ZERKER_OIDC_AUDIENCE)")
	tenant := flag.String("tenant", "acme", "tenant claim value")
	subject := flag.String("subject", "user-alice", "sub claim value")
	tokenFile := flag.String("token-file", "", "if set, write the minted token to this path")
	flag.Parse()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("generate key: %v", err)
	}
	const keyID = "mock-key-1"

	now := time.Now()
	token, err := signJWT(key, keyID, map[string]any{
		"iss":    *issuer,
		"aud":    *audience,
		"sub":    *subject,
		"tenant": *tenant,
		"iat":    now.Unix(),
		"exp":    now.Add(time.Hour).Unix(),
	})
	if err != nil {
		log.Fatalf("sign token: %v", err)
	}
	if *tokenFile != "" {
		if err := os.WriteFile(*tokenFile, []byte(token), 0o600); err != nil {
			log.Fatalf("write token: %v", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"issuer": *issuer, "jwks_uri": *issuer + "/jwks.json"})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []any{jwk(keyID, &key.PublicKey)}})
	})

	fmt.Printf("mock-oidc: issuer %s (aud=%s tenant=%s sub=%s)\n", *issuer, *audience, *tenant, *subject)
	fmt.Printf("mock-oidc: token\n%s\n", token)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

func signJWT(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func jwk(kid string, pub *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}
