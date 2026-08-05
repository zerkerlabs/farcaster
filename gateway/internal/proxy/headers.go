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

// copyHeaders copies headers from src to dst, skipping any that appear in
// blockedHeaders and any X-Zerker-* headers (to prevent callers from
// spoofing gateway metadata).
func copyHeaders(src, dst http.Header) {
	for k, vs := range src {
		canon := http.CanonicalHeaderKey(k)
		if _, blocked := blockedHeaders[canon]; blocked {
			continue
		}
		if strings.HasPrefix(canon, "X-Zerker-") {
			continue
		}
		for _, v := range vs {
			dst.Add(canon, v)
		}
	}
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
