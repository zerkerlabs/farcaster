// Package depguard holds the dependency-boundary guard for surface-7 (spec 0007
// T8): an always-on test asserting the chain/crypto/KMS weight the facilitator
// owns never leaks into the gateway module (ADR-0010). It runs in the default
// `make check` — unlike the flag-gated Sepolia e2e — so a stray import that
// pulls go-ethereum or the AWS KMS SDK into gateway/go.mod fails CI immediately.
package depguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// quarantined are module paths ADR-0010 keeps out of the gateway module: the
// heavy chain/RPC/KMS dependencies that belong only to facilitator/. (pgx and
// other shared infra are intentionally not listed — the gateway uses them too.)
var quarantined = []string{
	"github.com/ethereum/go-ethereum",
	"github.com/aws/aws-sdk-go-v2/service/kms",
}

func TestGatewayGoModExcludesQuarantinedDeps(t *testing.T) {
	root := repoRoot(t)
	gatewayGoMod := filepath.Join(root, "gateway", "go.mod")
	data, err := os.ReadFile(gatewayGoMod) //nolint:gosec // fixed in-repo path derived from go.work
	if err != nil {
		t.Fatalf("read %s: %v", gatewayGoMod, err)
	}
	text := string(data)

	for _, mod := range quarantined {
		if strings.Contains(text, mod) {
			t.Errorf("gateway/go.mod must not depend on %q — chain/crypto/KMS deps are facilitator-only (ADR-0010). "+
				"If a gateway change needs this, it belongs behind the x402types contract or in facilitator/.", mod)
		}
	}
}

// repoRoot walks up from the test's working directory to the workspace root,
// identified by go.work, so the guard is independent of where `go test` is run.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate workspace root (go.work not found walking up from the test dir)")
		}
		dir = parent
	}
}
