package ssrf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrBlocked is returned by SafeDialContext when the destination address (or
// every address a hostname resolves to) falls into a blocked range (see
// IsBlockedIP). The block is deterministic — re-resolving and re-dialing the
// same target would just block again — so callers should treat it as
// non-retriable rather than a transient network failure.
var ErrBlocked = errors.New("ssrf: destination address is in a blocked range")

// safeDial carries the Timeout and KeepAlive that http.DefaultTransport sets on
// its own dialer, so a Transport that swaps in SafeDialContext keeps the same
// dial behaviour it would otherwise get for free.
var safeDial = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}

// SafeDialContext is a net.Dialer.DialContext-compatible function that
// prevents DNS-rebinding SSRF attacks: it resolves the target hostname,
// rejects the dial if any resolved address is in a blocked range, and then
// dials directly to a verified IP — closing the TOCTOU window between the
// DNS check and the actual TCP connect. Any caller wiring a tenant-supplied
// egress URL into an http.Transport (the proxy's upstream calls, the x402
// facilitator settle client) should set Transport.DialContext to this
// function so every such egress point shares one guard.
//
// For IP-literal addresses that somehow reach this layer (defense in depth),
// the check is applied directly without a DNS lookup.
func SafeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("ssrf dialer: parse addr: %w", err)
	}

	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return nil, fmt.Errorf("ssrf: destination %s is in a blocked range: %w", host, ErrBlocked)
		}
		return safeDial.DialContext(ctx, network, addr)
	}

	resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("ssrf dialer: resolve %s: %w", host, err)
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("ssrf dialer: no addresses resolved for %s", host)
	}

	// Reject immediately if any resolved address is in a blocked range. A
	// hostname that resolves to even one blocked IP is treated as unsafe —
	// allowing "the other" addresses would leave a rebinding window.
	for _, a := range resolved {
		if IsBlockedIP(a.IP) {
			return nil, fmt.Errorf("ssrf: host %s resolves to a blocked address (%s): %w", host, a.IP, ErrBlocked)
		}
	}

	// Dial each verified IP in order, returning the first successful
	// connection. Dialing a specific IP (rather than the hostname) closes the
	// TOCTOU window between the check above and the actual TCP SYN.
	var lastErr error
	for _, a := range resolved {
		var conn net.Conn
		conn, lastErr = safeDial.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
		if lastErr == nil {
			return conn, nil
		}
	}
	return nil, lastErr
}
