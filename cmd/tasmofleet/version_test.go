package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/changelog"
)

// Why (#124): `tasmofleet version` is what people paste into bug reports; it must say the same
// as the header, About and the startup log, which name the release a "dev" build is based on.
func TestVersionCommandPrintsTheDisplayForm(t *testing.T) {
	old := version
	version = "dev"
	t.Cleanup(func() { version = old })
	var out bytes.Buffer
	if err := run(context.Background(), []string{"version"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	want := changelog.Latest(changelog.Embedded()) + "+dev\n"
	if out.String() != want {
		t.Fatalf("version printed %q, want %q", out.String(), want)
	}
}
