package account_test

import (
	"testing"

	"github.com/zerkerlabs/gateway/facilitator/internal/account"
	"github.com/zerkerlabs/gateway/facilitator/internal/mtls/mtlstest"
)

func TestFingerprint(t *testing.T) {
	t.Parallel()

	ca := mtlstest.NewCA(t)
	certA := ca.IssueLeaf(t, "cert-a")
	certAAgain := certA // same parsed certificate, computed twice
	certB := ca.IssueLeaf(t, "cert-b")

	fpA1 := account.Fingerprint(certA.Cert)
	fpA2 := account.Fingerprint(certAAgain.Cert)
	fpB := account.Fingerprint(certB.Cert)

	if fpA1 != fpA2 {
		t.Fatalf("fingerprint not deterministic: %q != %q", fpA1, fpA2)
	}
	if fpA1 == fpB {
		t.Fatalf("distinct certificates produced the same fingerprint: %q", fpA1)
	}
	if len(fpA1) != 64 { // hex-encoded SHA-256
		t.Fatalf("fingerprint length = %d, want 64 (hex SHA-256)", len(fpA1))
	}
}
