package resource_test

import (
	"strings"
	"testing"

	"github.com/zerkerlabs/farcaster/gateway/internal/resource"
)

func TestNew_Prefix(t *testing.T) {
	t.Parallel()

	id, err := resource.New("agt")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(id, "agt_") {
		t.Errorf("id %q does not start with %q", id, "agt_")
	}
}

func TestNew_Unique(t *testing.T) {
	t.Parallel()

	const n = 20
	seen := make(map[string]bool, n)
	for range n {
		id, err := resource.New("agt")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate ID generated: %q", id)
		}
		seen[id] = true
	}
}

func TestNew_DifferentPrefixes(t *testing.T) {
	t.Parallel()

	prefixes := []string{"agt", "usr", "ten"}
	for _, p := range prefixes {
		id, err := resource.New(p)
		if err != nil {
			t.Fatalf("New(%q): %v", p, err)
		}
		if !strings.HasPrefix(id, p+"_") {
			t.Errorf("id %q does not start with %q", id, p+"_")
		}
	}
}
