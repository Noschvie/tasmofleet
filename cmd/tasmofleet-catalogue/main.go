// Command tasmofleet-catalogue generates internal/tasmotacmd/catalogue-<version>.json from an
// unpacked Tasmota source tree. It reads local files only; `make catalogue` fetches and verifies
// the pinned tarball first. See docs/research/tasmota-command-catalogue.md.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/nofuturekid/tasmofleet/internal/tasmotacmd/extract"
)

func main() {
	src := flag.String("src", "", "the `tasmota` directory of the unpacked Tasmota source")
	version := flag.String("version", "", "Tasmota version the source belongs to, e.g. 16.0.0")
	sha := flag.String("sha256", "", "sha256 of the source tarball, recorded in the output")
	out := flag.String("out", "", "output file")
	flag.Parse()
	if *src == "" || *version == "" || *sha == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	cat, err := extract.Extract(os.DirFS(*src), *version, *sha)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tasmofleet-catalogue:", err)
		os.Exit(1)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(cat); err != nil {
		fmt.Fprintln(os.Stderr, "tasmofleet-catalogue:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil { //nolint:gosec // a checked-in, world-readable data file
		fmt.Fprintln(os.Stderr, "tasmofleet-catalogue:", err)
		os.Exit(1)
	}
	loose := 0
	for _, c := range cat.Commands {
		if c.Loose != "" {
			loose++
		}
	}
	fmt.Printf("%d commands, %d loose, %d skipped tables\n", len(cat.Commands), loose, len(cat.SkippedTables))
}
