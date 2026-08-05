package proxy

import (
	"context"
	"errors"
	"net"

	"github.com/zerkerlabs/gateway/gateway/internal/ssrf"
)

// safeDialContext is a net.Dialer.DialContext-compatible function that prevents
// DNS-rebinding SSRF attacks. It delegates to ssrf.SafeDialContext — the single
// shared implementation of the resolve-then-check-then-dial-by-IP guard also
// used by the x402 facilitator settle client — and translates ssrf.ErrBlocked
// into the package-local errBlocked sentinel so existing errors.Is(err,
// errBlocked) call sites in this package keep working unchanged.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := ssrf.SafeDialContext(ctx, network, addr)
	if err != nil && errors.Is(err, ssrf.ErrBlocked) {
		return nil, errBlocked
	}
	return conn, err
}
