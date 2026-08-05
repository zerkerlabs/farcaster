package proxy

import (
	"net/http"
	"strings"

	"github.com/zerkerlabs/gateway/gateway/internal/credential"
)

// blockedHeaders is the set of caller headers that must never be forwarded to
// the upstream. The Authorization header is blocked because Zerker replaces
// it with the configured upstream credential (invariant #4, AGENTS.md; spec
// 0002 §Q5). The remaining entries are standard hop-by-hop headers that have
// no meaning beyond a single HTTP hop (RFC 7230 §6.1).
var blockedHeaders = map[string]struct{}{
	"Authorization":       {},
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {},
	"Te":                  {},
	"Trailers":            {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	// Content-Length is recomputed by the HTTP client from the buffered body we
	// replay; forwarding the caller's value verbatim can conflict with the real
	// length (e.g. an empty body still carrying a stale non-zero length).
	"Content-Length": {},
}

// spoofablePrefixes are header prefixes the gateway itself injects, so a
// caller-supplied header carrying one must never reach the upstream.
//
// X-Farcaster-* is the pre-rename prefix. It stays here because dropping it
// would REMOVE a protection: an upstream still keyed to the old contract would
// go from "value injected by the gateway, spoofing blocked" to "gateway no
// longer injects it, and the caller's value passes straight through." Safe to
// delete once no upstream reads X-Farcaster-* headers.
var spoofablePrefixes = []string{"X-Zerker-", "X-Farcaster-"}

// copyHeaders copies headers from src to dst, skipping any that appear in
// blockedHeaders and any header carrying a spoofablePrefixes prefix (to
// prevent callers from spoofing gateway metadata).
func copyHeaders(src, dst http.Header) {
	for k, vs := range src {
		canon := http.CanonicalHeaderKey(k)
		if _, blocked := blockedHeaders[canon]; blocked {
			continue
		}
		if hasSpoofablePrefix(canon) {
			continue
		}
		for _, v := range vs {
			dst.Add(canon, v)
		}
	}
}

// hasSpoofablePrefix reports whether canon (a canonicalized header key) starts
// with any prefix the gateway injects itself.
func hasSpoofablePrefix(canon string) bool {
	for _, p := range spoofablePrefixes {
		if strings.HasPrefix(canon, p) {
			return true
		}
	}
	return false
}

// injectCredential sets the upstream authentication header based on the
// credential's AuthType and resolved plaintext. For AuthTypeNone or an empty
// plaintext, no header is set.
//
// The plaintext must never be logged or echoed (invariant #4, AGENTS.md).
func injectCredential(h http.Header, authType credential.AuthType, plaintext []byte) {
	if len(plaintext) == 0 || authType == credential.AuthTypeNone {
		return
	}
	switch authType {
	case credential.AuthTypeBearer:
		h.Set("Authorization", "Bearer "+string(plaintext))
	case credential.AuthTypeAPIKey:
		h.Set("X-Api-Key", string(plaintext))
	}
}
