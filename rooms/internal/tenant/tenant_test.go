package tenant_test

import (
	"context"
	"testing"

	"github.com/zerkerlabs/farcaster/rooms/internal/tenant"
)

func TestWithTenant_RoundTrips(t *testing.T) {
	t.Parallel()

	ctx := tenant.WithTenant(context.Background(), "tenant-1")
	if got := tenant.FromContext(ctx); got != "tenant-1" {
		t.Errorf("FromContext() = %q, want %q", got, "tenant-1")
	}
}

func TestFromContext_EmptyWhenUnset(t *testing.T) {
	t.Parallel()

	if got := tenant.FromContext(context.Background()); got != "" {
		t.Errorf("FromContext() = %q, want empty string", got)
	}
}
