package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// envLiteral matches a quoted environment-variable name in Go source: either a
// ZERKER_-prefixed name or the DATABASE_URL fallback. Requiring the
// surrounding quotes means names mentioned in comments or embedded in larger
// strings (e.g. error messages) are ignored — only actual string literals
// passed to os.Getenv / os.LookupEnv / envOrDefault are matched.
var envLiteral = regexp.MustCompile(`"(ZERKER_[A-Z0-9_]+|DATABASE_URL)"`)

// skipDirs are subtrees of the gateway module that read env vars for tooling or
// dev-only purposes outside the gateway binary's production config surface.
var skipDirs = map[string]bool{"scripts": true, "bin": true, "node_modules": true}

// TestReferenceMatchesSource is the drift-guard behind "a new env var can't
// ship undocumented": it scans the gateway module's non-test source for
// environment-variable literals and asserts the set exactly equals the names
// (plus aliases) declared in Reference. Adding an os.Getenv("ZERKER_NEW")
// without a Reference entry fails here; so does leaving a Reference entry for a
// variable the code no longer reads. This test runs inside `make check`.
func TestReferenceMatchesSource(t *testing.T) {
	documented := map[string]bool{}
	for _, v := range Reference {
		documented[v.Name] = true
		for _, a := range v.Aliases {
			documented[a] = true
		}
	}

	found := map[string]string{} // env name -> first file it was seen in
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		// Skip test files and the manifest itself (reference.go documents the
		// names as literals, so scanning it would be circular).
		if !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") ||
			filepath.Base(path) == "reference.go" {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // G304: the test deliberately reads gateway source files under the module root
		if err != nil {
			return err
		}
		for _, m := range envLiteral.FindAllStringSubmatch(string(src), -1) {
			if _, seen := found[m[1]]; !seen {
				found[m[1]] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk gateway source: %v", err)
	}
	if len(found) == 0 {
		t.Fatalf("scanned no env-var literals under %s — is the path wrong?", root)
	}

	for name, path := range found {
		if !documented[name] {
			t.Errorf("env var %q is read in %s but not declared in config.Reference — "+
				"add it to reference.go and run `make -C gateway docs`", name, path)
		}
	}
	for name := range documented {
		if _, ok := found[name]; !ok {
			t.Errorf("config.Reference declares %q but no gateway source reads it — "+
				"remove the stale entry and run `make -C gateway docs`", name)
		}
	}
}

// TestRenderMarkdownDeterministic guards the emitter against nondeterministic
// output, which would make the -check drift-guard flap.
func TestRenderMarkdownDeterministic(t *testing.T) {
	first := string(RenderMarkdown())
	for i := 0; i < 5; i++ {
		if got := string(RenderMarkdown()); got != first {
			t.Fatalf("RenderMarkdown is not deterministic across calls")
		}
	}

	// Every documented variable name must appear in the rendered output.
	for _, v := range Reference {
		if !strings.Contains(first, v.Name) {
			t.Errorf("rendered reference is missing %q", v.Name)
		}
	}

	// Groups must render in declared order.
	var positions []int
	for _, g := range Groups {
		idx := strings.Index(first, "### "+g)
		if idx < 0 {
			continue // a group with no vars is legitimately absent
		}
		positions = append(positions, idx)
	}
	if !sort.IntsAreSorted(positions) {
		t.Errorf("group sections are out of declared order: %v", positions)
	}
}
