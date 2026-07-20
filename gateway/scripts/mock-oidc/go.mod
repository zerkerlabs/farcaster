// Separate module so this dev-only tool stays out of the gateway build,
// `go test ./...`, and `go vet ./...`. It is invoked by scripts/dev-auth.sh.
module farcaster.dev/mock-oidc

go 1.26
