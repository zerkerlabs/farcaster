package auth

import "context"

// WithIdentity returns a copy of ctx with tenant and user set, exactly as the
// auth middleware would after validating a bearer token carrying those claims.
// This is a test helper; call via authtest.WithIdentity rather than directly
// from production code.
func WithIdentity(ctx context.Context, tenant, user string) context.Context {
	ctx = context.WithValue(ctx, keyTenant, tenant)
	return context.WithValue(ctx, keyUser, user)
}

// WithScopes returns a copy of ctx with the given OAuth scopes set, exactly as
// the auth middleware would after parsing the scope claim from a bearer token.
// This is a test helper; call via authtest.WithScopes rather than directly
// from production code.
func WithScopes(ctx context.Context, scopes []string) context.Context {
	return context.WithValue(ctx, keyScopes, scopes)
}
