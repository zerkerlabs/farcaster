package ssrf_test

import (
	"net"
	"testing"

	"github.com/zerkerlabs/farcaster/gateway/internal/ssrf"
)

func TestIsBlockedIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ip   string
		want bool
	}{
		// Loopback.
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		// Private (RFC 1918 / RFC 4193).
		{"private 10/8", "10.0.0.1", true},
		{"private 172.16/12", "172.16.0.1", true},
		{"private 192.168/16", "192.168.1.1", true},
		{"unique-local v6", "fc00::1", true},
		// CGNAT shared space (RFC 6598) — and its boundaries.
		{"cgnat low", "100.64.0.1", true},
		{"cgnat high", "100.127.255.255", true},
		{"just below cgnat is public", "100.63.255.255", false},
		{"just above cgnat is public", "100.128.0.0", false},
		// Link-local unicast (incl. cloud metadata 169.254.169.254).
		{"link-local unicast v4", "169.254.169.254", true},
		{"link-local unicast v6", "fe80::1", true},
		// Multicast (full 224.0.0.0/4, not just link-local).
		{"link-local multicast", "224.0.0.1", true},
		{"admin-scoped multicast", "239.255.255.255", true},
		{"multicast v6", "ff02::1", true},
		// IPv4 reserved/future (240.0.0.0/4) incl. the limited broadcast address.
		{"reserved", "240.0.0.1", true},
		{"limited broadcast", "255.255.255.255", true},
		// Unspecified.
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},
		// Public — must NOT be blocked.
		{"public 1.1.1.1", "1.1.1.1", false},
		{"public 8.8.8.8", "8.8.8.8", false},
		{"public v6", "2606:4700::1", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("ParseIP(%q) returned nil", tc.ip)
			}
			if got := ssrf.IsBlockedIP(ip); got != tc.want {
				t.Errorf("IsBlockedIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}
