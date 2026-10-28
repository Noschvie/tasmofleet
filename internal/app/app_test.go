package app

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/changelog"
)

// Why (#124): the UI and the startup log show Version through DisplayVersion; reverting that
// wiring to the raw build value would hide the release a "dev" or edge build is based on.
func TestDisplayVersionNamesTheBaseRelease(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "dev"
	want := changelog.Latest(changelog.Embedded()) + "+dev"
	if got := DisplayVersion(); got != want || want == "+dev" {
		t.Fatalf("DisplayVersion() = %q, want %q", got, want)
	}
	Version = "v1.2.3"
	if got := DisplayVersion(); got != "v1.2.3" {
		t.Fatalf("a release tag must stay as it is: %q", got)
	}
}

// Why (#124): header and About read web.Deps.Version and the startup line reads the log; both
// must carry the display form, not the raw build value.
func TestDisplayVersionReachesTheUIAndTheLog(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "dev"
	want := changelog.Latest(changelog.Embedded()) + "+dev"
	var buf bytes.Buffer
	a := &runtime{svc: &Services{}, log: slog.New(slog.NewTextHandler(&buf, nil))}
	if got := a.webDeps(nil, nil).Version; got != want {
		t.Errorf("web.Deps.Version = %q, want %q", got, want)
	}
	a.logStarting()
	if !bytes.Contains(buf.Bytes(), []byte("version="+want)) {
		t.Errorf("startup line lacks version=%s: %s", want, buf.String())
	}
}
