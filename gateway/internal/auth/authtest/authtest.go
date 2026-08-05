// Package authtest provides test helpers for packages that depend on auth
// context. Import this package in test files to inject authenticated identity
// into a request context without a live OIDC provider.
package authtest

import (
	"context"

	"github.com/zerkerlabs/gateway/gateway/internal/auth"
)

// WithIdentity returns a copy of ctx with tenant and user set exactly as the
// auth middleware would after validating a bearer token carrying those claims.
// Use in tests that exercise handlers requiring an authenticated context.
func WithIdentity(ctx context.Context, tenant, user string) context.Context {
	return auth.WithIdentity(ctx, tenant, user)
}

// WithScopes returns a copy of ctx with the given OAuth scopes set exactly as
// the auth middleware would after parsing the scope claim from a bearer token.
// Use in tests that exercise handlers gated on specific OAuth scopes.
func WithScopes(ctx context.Context, scopes []string) context.Context {
	return auth.WithScopes(ctx, scopes)
}
