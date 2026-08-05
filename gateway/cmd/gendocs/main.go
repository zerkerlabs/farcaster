// Command gendocs emits the gateway's config/env reference as a Markdown
// partial the product website includes.
//
// It renders gateway/internal/config.Reference — the canonical manifest of the
// environment variables the gateway reads — into the Starlight partial the
// docs' "Configuration reference" page imports. Run it via `make -C gateway
// docs`; `make -C gateway check` runs it in -check mode and fails if the
// committed partial is stale, so a config change can't ship without its docs.
//
// The output lives under www/ (not gateway/) because the website builds with
// www/ as its root and must have the committed artifact available at build
// time — nothing runs Go during the site build.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/zerkerlabs/gateway/gateway/internal/config"
)

// defaultOut is relative to the gateway module root, where `make -C gateway`
// runs. The leading `_` marks it a Starlight partial (imported, not routed).
const defaultOut = "../www/src/content/docs/self-hosting/_config-reference.md"

func main() {
	out := flag.String("out", defaultOut, "path to write the config reference Markdown partial")
	check := flag.Bool("check", false, "verify the file at -out matches the generated output; exit non-zero if stale")
	flag.Parse()

	rendered := config.RenderMarkdown()

	if *check {
		existing, err := os.ReadFile(*out) //nolint:gosec // G304: -out is an operator-provided path, read only to compare against generated output
		if err != nil {
			fmt.Fprintf(os.Stderr, "gendocs -check: %v\n", err)
			os.Exit(1)
		}
		if string(existing) != string(rendered) {
			fmt.Fprintf(os.Stderr,
				"gendocs -check: %s is stale — run `make -C gateway docs` and commit the result.\n", *out)
			os.Exit(1)
		}
		return
	}

	if err := os.WriteFile(*out, rendered, 0o644); err != nil { //nolint:gosec // G306: a generated docs artifact is intentionally world-readable
		fmt.Fprintf(os.Stderr, "gendocs: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", *out)
}
