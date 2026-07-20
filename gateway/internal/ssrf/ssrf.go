// Package ssrf provides helpers for Server-Side Request Forgery (SSRF)
// prevention. It is the single authoritative location for the IP-range checks
// applied at both the CRUD input boundary (httpapi.validateUpstreamURL) and at
// proxy forward time (proxy.safeDialContext).
package ssrf

import "net"

// cgnatBlock is the RFC 6598 carrier-grade NAT shared address space
// (100.64.0.0/10). net.IP.IsPrivate covers RFC 1918 and RFC 4193 but not this
// range, which can reach internal infrastructure in some ISP/cloud environments.
var cgnatBlock = mustParseCIDR("100.64.0.0/10")

// reservedBlock is the IPv4 reserved/future-use range (240.0.0.0/4, RFC 1112).
// It has no business as an upstream target; block it conservatively.
var reservedBlock = mustParseCIDR("240.0.0.0/4")

// mustParseCIDR parses a CIDR literal at package initialisation, panicking on
// error so a malformed constant fails loudly at startup rather than as a nil
// dereference on the first IsBlockedIP call.
func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// IsBlockedIP reports whether ip must never be the destination of a proxied
// upstream connection. It covers loopback, private (RFC 1918 / unique-local
// IPv6), CGNAT shared space (RFC 6598), link-local unicast, all multicast
// (224.0.0.0/4 and ff00::/8), IPv4 reserved/future space (240.0.0.0/4), and
// unspecified addresses.
func IsBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		cgnatBlock.Contains(ip) ||
		reservedBlock.Contains(ip)
}
