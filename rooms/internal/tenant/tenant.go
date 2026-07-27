// Package tenant provides the request-context seam that carries the caller's
// tenant identifier into Rooms HTTP handlers.
//
// Bearer-token authentication lands in a separate change; until then (and
// unconditionally in tests thereafter), WithTenant is how a tenant identifier
// is placed on a request context. Handlers read it back with FromContext. This
// package does no token parsing — that is deliberately out of scope here.
package tenant

import "context"

// contextKey is an unexported type for this package's context key, so it
// cannot collide with keys set by other packages.
type contextKey int

const keyTenant contextKey = iota

// WithTenant returns a copy of ctx carrying tenantID. The bearer-auth
// middleware will call this once it lands; tests call it directly to inject
// an authenticated tenant without a real token.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, keyTenant, tenantID)
}

// FromContext returns the tenant identifier placed on ctx by WithTenant.
// Returns an empty string if none was set.
func FromContext(ctx context.Context) string {
	v, _ := ctx.Value(keyTenant).(string)
	return v
}
