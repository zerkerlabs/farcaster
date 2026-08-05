module github.com/zerkerlabs/gateway/gateway

go 1.26.4

require (
	github.com/coreos/go-oidc/v3 v3.19.0
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1
	github.com/getkin/kin-openapi v0.141.0
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/zerkerlabs/gateway/x402types v0.0.0
	golang.org/x/crypto v0.53.0
	golang.org/x/time v0.15.0
)

// x402types is an in-repo workspace module (ADR-0010), not published. The
// replace lets `go mod tidy` and per-module CI resolve it by path; go.work
// covers multi-module local builds.
replace github.com/zerkerlabs/gateway/x402types => ../x402types

require (
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-openapi/jsonpointer v0.22.5 // indirect
	github.com/go-openapi/swag/jsonname v0.25.5 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/oasdiff/yaml v0.1.1 // indirect
	github.com/oasdiff/yaml3 v0.0.14 // indirect
	github.com/rogpeppe/go-internal v1.15.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
)
