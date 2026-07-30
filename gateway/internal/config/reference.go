// Package config is the canonical, machine-readable reference for the
// environment variables the Farcaster gateway reads.
//
// The gateway has no central config loader — settings are read as scattered
// os.Getenv calls at boot (see cmd/farcaster, internal/auth, internal/kms).
// This package does not change that; it is a hand-maintained *manifest* of
// those variables that two things build on:
//
//   - cmd/gendocs renders Reference into the public config/env reference the
//     product website includes (make docs); and
//   - reference_test.go scans the gateway source for os.Getenv / os.LookupEnv
//     keys and fails if any is missing from (or stale in) Reference — so a new
//     environment variable cannot ship undocumented.
//
// Add a variable here in the same change that introduces it; `make check`
// enforces the pairing.
package config

// Var describes one environment variable the gateway reads.
type Var struct {
	// Name is the primary environment variable name.
	Name string
	// Aliases are additional names read as fallbacks for the same setting
	// (e.g. DATABASE_URL for FARCASTER_DATABASE_URL).
	Aliases []string
	// Group is the reference section this variable belongs to.
	Group string
	// Required reports whether the gateway refuses to start without it.
	Required bool
	// Default is the value used when the variable is unset. Only meaningful
	// when HasDefault is true.
	Default string
	// HasDefault distinguishes "defaults to the empty string" (HasDefault
	// true, Default "") from "has no default" (HasDefault false).
	HasDefault bool
	// Description is a one-to-three sentence explanation of the variable.
	Description string
}

// Group names, in the order they render in the reference.
const (
	groupServer  = "Server"
	groupStorage = "Storage"
	groupAuth    = "Authentication (OIDC)"
	groupSecrets = "Secrets & KMS" //nolint:gosec // G101 false positive: a UI section label, not a credential value
)

// Groups is the render order for the reference's sections.
var Groups = []string{groupServer, groupStorage, groupAuth, groupSecrets}

// Reference is the canonical list of environment variables the gateway honors.
// It reflects existing behavior only; no setting is invented here.
var Reference = []Var{
	{
		Name:        "FARCASTER_ADDR",
		Group:       groupServer,
		Default:     ":8080",
		HasDefault:  true,
		Description: "TCP address the gateway's HTTP server listens on.",
	},
	{
		Name:    "FARCASTER_DATABASE_URL",
		Aliases: []string{"DATABASE_URL"},
		Group:   groupStorage,
		Description: "PostgreSQL connection string (pgx DSN). When unset, the gateway " +
			"falls back to a non-durable in-memory store — development only; all " +
			"state is lost on restart. `DATABASE_URL` is honored as a fallback. " +
			"Migrations are applied automatically on boot.",
	},
	{
		Name:     "FARCASTER_OIDC_ISSUER",
		Group:    groupAuth,
		Required: true,
		Description: "OIDC issuer base URL used for provider discovery. The gateway " +
			"refuses to start without it — there is no unauthenticated path to bring it up.",
	},
	{
		Name:        "FARCASTER_OIDC_AUDIENCE",
		Group:       groupAuth,
		Required:    true,
		Description: "Expected value of the JWT `aud` claim. The gateway refuses to start without it.",
	},
	{
		Name:     "FARCASTER_OIDC_TENANT_CLAIM",
		Group:    groupAuth,
		Required: true,
		Description: "JWT claim carrying the tenant/client identifier. Provider-specific, " +
			"so there is no default — the gateway refuses to start without it.",
	},
	{
		Name:        "FARCASTER_OIDC_USER_CLAIM",
		Group:       groupAuth,
		Default:     "sub",
		HasDefault:  true,
		Description: "JWT claim carrying the acting user's subject. `sub` is the OIDC standard.",
	},
	{
		Name:       "FARCASTER_OIDC_SCOPE_CLAIM",
		Group:      groupAuth,
		Default:    "scope",
		HasDefault: true,
		Description: "JWT claim carrying the token's OAuth scopes (a space-separated string " +
			"or a JSON array). Leave empty to disable scope extraction. `scp` is common on Microsoft/Auth0.",
	},
	{
		Name:  "FARCASTER_KMS_KEY",
		Group: groupSecrets,
		Description: "Hex-encoded 32-byte (64 hex chars) master key for the local KMS " +
			"provider, which envelope-encrypts stored credentials. When unset, a random " +
			"ephemeral key is generated that does not survive a restart — never run " +
			"production without setting it.",
	},
}
