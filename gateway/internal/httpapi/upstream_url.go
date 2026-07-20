package httpapi

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/zerkerlabs/farcaster/gateway/internal/ssrf"
)

// validateUpstreamURL applies input-time hygiene to a caller-supplied upstream
// URL. It requires an http(s) URL with a host and rejects hosts that are IP
// literals in a loopback, private, link-local, or unspecified range — a first
// line of defence against SSRF once the proxy layer (spec 0002, ticket #30)
// forwards requests to this URL. Without it, a tenant could register
// http://169.254.169.254/ (cloud metadata) or http://127.0.0.1/ and turn the
// gateway into a confused deputy.
//
// This is NOT complete SSRF protection: it does not stop a hostname that
// resolves to an internal IP (DNS rebinding). That has to be enforced at request
// time in the proxy, by resolving and re-checking the address before each
// forward. This guard just keeps the obviously dangerous values out of storage.
func validateUpstreamURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid upstream_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("upstream_url must be an http or https URL")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("upstream_url must include a host")
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("upstream_url must not target localhost")
	}
	if ip := net.ParseIP(host); ip != nil && ssrf.IsBlockedIP(ip) {
		return fmt.Errorf("upstream_url must not target a private, loopback, or link-local address")
	}
	return nil
}
