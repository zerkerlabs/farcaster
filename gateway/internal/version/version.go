// Package version exposes build metadata for the Zerker gateway.
//
// Values are overridden at build time via -ldflags, e.g.:
//
//	go build -ldflags "-X github.com/zerkerlabs/gateway/gateway/internal/version.Version=v0.1.0"
package version

// Version is the semantic version of the build. "dev" for local builds.
var Version = "dev"

// Commit is the git SHA of the build. "unknown" for local builds.
var Commit = "unknown"
