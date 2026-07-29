// Package auth provides OAuth 2.0 / OIDC bearer-token validation middleware
// for Rooms. Rooms is a separate deployable from the gateway (its own module,
// its own trust boundary), so it cannot import the gateway's internal auth
// package — but it validates tokens issued by the same identity provider, the
// same way: same issuer/audience configuration shape, same signature
// verification via go-oidc, same clock-skew handling, same failure behaviour.
// All provider-specific parameters (issuer URL, audience, claim names) are
// supplied through Config, not compiled in.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// contextKey is an unexported integer type for request-context keys.
// A named type prevents key collisions with keys stored by other packages.
type contextKey int

const (
	keyTenant contextKey = iota
	keyUser
)

// Config holds the OIDC validation parameters. All fields are
// provider-agnostic; the concrete identity provider and the claim→identity
// mapping are operator configuration, set to match the gateway's own OIDC
// setup so a caller's bearer token authenticates identically against both.
type Config struct {
	// IssuerURL is the OIDC issuer base URL used for provider discovery.
	// Environment variable: ROOMS_OIDC_ISSUER
	IssuerURL string

	// Audience is the expected value of the "aud" JWT claim.
	// Environment variable: ROOMS_OIDC_AUDIENCE
	Audience string

	// TenantClaim names the JWT claim that carries the tenant/client identifier.
	// Environment variable: ROOMS_OIDC_TENANT_CLAIM
	TenantClaim string

	// UserClaim names the JWT claim that carries the acting user's subject.
	// "sub" is the OIDC standard; override if the provider uses a different claim.
	// Environment variable: ROOMS_OIDC_USER_CLAIM
	UserClaim string

	// HTTPClient overrides the HTTP client used for OIDC discovery and JWKS
	// fetching. Leave nil to use the default client. Intended for tests that
	// point at an in-process mock OIDC server.
	HTTPClient *http.Client
}

// ConfigFromEnv reads OIDC configuration from environment variables.
// IssuerURL and Audience must be non-empty for the middleware to initialise.
func ConfigFromEnv() Config {
	return Config{
		IssuerURL:   os.Getenv("ROOMS_OIDC_ISSUER"),
		Audience:    os.Getenv("ROOMS_OIDC_AUDIENCE"),
		TenantClaim: os.Getenv("ROOMS_OIDC_TENANT_CLAIM"),
		UserClaim:   envOrDefault("ROOMS_OIDC_USER_CLAIM", "sub"),
	}
}

// TenantFromContext returns the tenant identifier placed in ctx by the auth
// middleware. Returns an empty string if the middleware has not run.
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(keyTenant).(string)
	return v
}

// UserFromContext returns the user subject placed in ctx by the auth
// middleware. Returns an empty string if the middleware has not run.
func UserFromContext(ctx context.Context) string {
	v, _ := ctx.Value(keyUser).(string)
	return v
}

// NewMiddleware initialises an OIDC provider by discovering cfg.IssuerURL and
// returns an HTTP middleware that validates bearer tokens on every request.
//
// The paths /healthz and /version bypass authentication. All other paths
// require a valid bearer token; missing, malformed, expired, or wrong-issuer/
// audience tokens return 401 with no body (AGENTS.md invariants #1 and #3).
// Token values are never logged (invariant #4).
//
// IssuerURL and Audience must be non-empty; NewMiddleware returns an error if
// either is missing (fail-closed — the server must not start without auth).
func NewMiddleware(ctx context.Context, cfg Config, logger *slog.Logger) (func(http.Handler) http.Handler, error) {
	if cfg.IssuerURL == "" {
		return nil, fmt.Errorf("auth: IssuerURL is required (set ROOMS_OIDC_ISSUER)")
	}
	if cfg.Audience == "" {
		return nil, fmt.Errorf("auth: Audience is required (set ROOMS_OIDC_AUDIENCE)")
	}

	if cfg.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, cfg.HTTPClient)
	}

	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("init OIDC provider %q: %w", cfg.IssuerURL, err)
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.Audience})

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Operational routes are exempt from authentication (invariant #1).
			path := r.URL.Path
			if path == "/healthz" || path == "/version" {
				next.ServeHTTP(w, r)
				return
			}

			raw, ok := bearerToken(r)
			if !ok {
				logger.Warn("auth: missing or malformed bearer token",
					"method", r.Method, "path", path)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			idToken, err := verifier.Verify(r.Context(), raw)
			if err != nil {
				logger.Warn("auth: token verification failed",
					"method", r.Method, "path", path, "reason", classifyVerifyError(err))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			var claims map[string]any
			if err := idToken.Claims(&claims); err != nil {
				logger.Warn("auth: failed to decode token claims",
					"method", r.Method, "path", path)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			tenant, _ := claims[cfg.TenantClaim].(string)
			user, _ := claims[cfg.UserClaim].(string)

			if tenant == "" || user == "" {
				logger.Warn("auth: required identity claims absent",
					"method", r.Method, "path", path,
					"tenant_claim", cfg.TenantClaim,
					"user_claim", cfg.UserClaim)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			logger.Info("auth: authenticated request", "method", r.Method, "path", path)

			rctx := context.WithValue(r.Context(), keyTenant, tenant)
			rctx = context.WithValue(rctx, keyUser, user)
			next.ServeHTTP(w, r.WithContext(rctx))
		})
	}, nil
}

// bearerToken extracts the raw token string from the Authorization header.
// Returns ("", false) if the header is absent or does not begin with "Bearer ".
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return "", false
	}
	return tok, true
}

// classifyVerifyError maps a go-oidc verification error to a stable category
// safe to log. go-oidc's errors are library-formatted text, and for some
// failure modes (audience mismatch, issuer mismatch) that text embeds the
// token's own claim values — logging err.Error() directly would write a
// rejected token's claims to the log, which is a credential leak (AGENTS.md
// invariant #4) just like logging the token itself. The category is enough to
// operate the service; the claim value is not needed and must never be
// logged.
//
// This function is duplicated in gateway/internal/auth/auth.go (see this
// package's comment for why rooms cannot import that package). Keep the two
// in lockstep: a category added here belongs there too, and vice versa.
func classifyVerifyError(err error) string {
	var expired *oidc.TokenExpiredError
	if errors.As(err, &expired) {
		return "expired"
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "expected audience"):
		return "audience_mismatch"
	case strings.Contains(msg, "issued by a different provider"):
		return "issuer_mismatch"
	case strings.Contains(msg, "before the nbf"):
		return "not_yet_valid"
	case strings.Contains(msg, "failed to verify signature"),
		strings.Contains(msg, "malformed jwt"),
		strings.Contains(msg, "id token not signed"),
		strings.Contains(msg, "multiple signatures"):
		return "signature_invalid"
	case strings.Contains(msg, "failed to unmarshal claims"),
		strings.Contains(msg, "failed to obtain source from claim name"),
		strings.Contains(msg, "source does not exist"):
		return "claims_invalid"
	default:
		return "unknown"
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
