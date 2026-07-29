// Package auth provides OAuth 2.0 / OIDC bearer-token validation middleware
// for the Farcaster gateway. The middleware is provider-agnostic at runtime:
// all provider-specific parameters (issuer URL, audience, claim names) are
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
	keyScopes
)

// Config holds the OIDC validation parameters. All fields are
// provider-agnostic; the concrete identity provider and the claim→identity
// mapping are PO decisions (see the needs-po-decision section in the PR body).
type Config struct {
	// IssuerURL is the OIDC issuer base URL used for provider discovery.
	// Environment variable: FARCASTER_OIDC_ISSUER
	IssuerURL string

	// Audience is the expected value of the "aud" JWT claim.
	// Environment variable: FARCASTER_OIDC_AUDIENCE
	Audience string

	// TenantClaim names the JWT claim that carries the tenant/client identifier.
	// The correct name is provider-specific (needs-po-decision).
	// Environment variable: FARCASTER_OIDC_TENANT_CLAIM
	TenantClaim string

	// UserClaim names the JWT claim that carries the acting user's subject.
	// "sub" is the OIDC standard; override if the provider uses a different claim.
	// Environment variable: FARCASTER_OIDC_USER_CLAIM
	UserClaim string

	// ScopeClaim names the JWT claim that carries the token's OAuth scopes.
	// The claim value may be a space-separated string (RFC 6749 §3.3) or a
	// JSON array of strings; both are supported. Leave empty to disable scope
	// extraction (all tokens will carry no scopes). "scope" is the standard
	// value; "scp" is common in Microsoft/Auth0 environments.
	// Environment variable: FARCASTER_OIDC_SCOPE_CLAIM
	ScopeClaim string

	// HTTPClient overrides the HTTP client used for OIDC discovery and JWKS
	// fetching. Leave nil to use the default client. Intended for tests that
	// point at an in-process mock OIDC server.
	HTTPClient *http.Client
}

// ConfigFromEnv reads OIDC configuration from environment variables.
// IssuerURL and Audience must be non-empty for the middleware to initialise.
// Claim name defaults are placeholders; confirm both with the PO before
// production use (see needs-po-decision in the PR body for issue #5).
func ConfigFromEnv() Config {
	return Config{
		IssuerURL:   os.Getenv("FARCASTER_OIDC_ISSUER"),
		Audience:    os.Getenv("FARCASTER_OIDC_AUDIENCE"),
		TenantClaim: envOrDefault("FARCASTER_OIDC_TENANT_CLAIM", ""),
		UserClaim:   envOrDefault("FARCASTER_OIDC_USER_CLAIM", "sub"),
		ScopeClaim:  envOrDefault("FARCASTER_OIDC_SCOPE_CLAIM", "scope"),
	}
}

// TenantFromContext returns the tenant identifier placed in ctx by the auth
// middleware. Returns an empty string if the middleware has not run.
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(keyTenant).(string)
	return v
}

// UserFromContext returns the user subject placed in ctx by the auth middleware.
// Returns an empty string if the middleware has not run.
func UserFromContext(ctx context.Context) string {
	v, _ := ctx.Value(keyUser).(string)
	return v
}

// ScopesFromContext returns the OAuth scopes placed in ctx by the auth
// middleware. Returns nil if the middleware has not run or the token carried
// no scope claim.
func ScopesFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(keyScopes).([]string)
	return v
}

// HasScope reports whether ctx carries the given OAuth scope. Returns false if
// the middleware has not run, the token had no scope claim, or the scope is
// absent from the token's scope set.
func HasScope(ctx context.Context, scope string) bool {
	for _, s := range ScopesFromContext(ctx) {
		if s == scope {
			return true
		}
	}
	return false
}

// NewMiddleware initialises an OIDC provider by discovering cfg.IssuerURL and
// returns an HTTP middleware that validates bearer tokens on every request.
//
// The paths /healthz and /version bypass authentication. All other paths require
// a valid bearer token; missing or invalid tokens return 401 with no body
// (invariants #1 and #3, AGENTS.md §3). Token values are never logged
// (invariant #4).
//
// IssuerURL and Audience must be non-empty; NewMiddleware returns an error if
// either is missing (fail-closed — the server must not start without auth).
func NewMiddleware(ctx context.Context, cfg Config, logger *slog.Logger) (func(http.Handler) http.Handler, error) {
	if cfg.IssuerURL == "" {
		return nil, fmt.Errorf("auth: IssuerURL is required (set FARCASTER_OIDC_ISSUER)")
	}
	if cfg.Audience == "" {
		return nil, fmt.Errorf("auth: Audience is required (set FARCASTER_OIDC_AUDIENCE)")
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

			scopes, malformed := parseScopes(claims, cfg.ScopeClaim)
			if malformed {
				// The claim is present but is neither a string nor an array, so
				// no scopes could be extracted. Fail-closed (zero scopes) is the
				// right posture, but a misconfigured ScopeClaim would otherwise be
				// invisible — log the claim name and value type (never the value)
				// so an operator can diagnose it.
				logger.Warn("auth: scope claim present but not a string or string array; treating as no scopes",
					"scope_claim", cfg.ScopeClaim,
					"value_type", fmt.Sprintf("%T", claims[cfg.ScopeClaim]))
			}

			rctx := context.WithValue(r.Context(), keyTenant, tenant)
			rctx = context.WithValue(rctx, keyUser, user)
			rctx = context.WithValue(rctx, keyScopes, scopes)
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
// This function is duplicated in rooms/internal/auth/auth.go (see that
// package's comment for why rooms cannot import this package). Keep the two
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

// parseScopes extracts OAuth scopes from the named claim in claims. The claim
// value may be a space-separated string (RFC 6749 §3.3) or a JSON array of
// strings. Returns nil scopes when the claim name is empty or the claim is
// absent.
//
// The malformed return is true only when the claim is present but its value is
// neither a string nor an array — a likely ScopeClaim misconfiguration. Scopes
// are nil (fail-closed) in that case too; the flag lets the caller surface a
// diagnostic that a plain "no scopes" result would hide.
func parseScopes(claims map[string]any, claimName string) (scopes []string, malformed bool) {
	if claimName == "" {
		return nil, false
	}
	raw, ok := claims[claimName]
	if !ok {
		return nil, false
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil, false
		}
		return strings.Fields(v), false
	case []any:
		scopes := make([]string, 0, len(v))
		for _, s := range v {
			if str, ok := s.(string); ok && str != "" {
				scopes = append(scopes, str)
			}
		}
		return scopes, false
	default:
		return nil, true
	}
}
