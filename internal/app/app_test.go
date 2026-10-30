package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/changelog"
	"github.com/nofuturekid/tasmofleet/internal/config"
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

// Why (#250): the emergency runbook tells the operator to compare the certificate fingerprint
// with the start log (trust on first use for a self-signed certificate); without the log line
// the comparison cannot be made.
func TestTLSStartLogsCertificateFingerprintAndExpiry(t *testing.T) {
	var buf bytes.Buffer
	a := &runtime{log: slog.New(slog.NewTextHandler(&buf, nil)), cfg: config.Config{TLSSelfSigned: true, DataDir: t.TempDir()}}
	if _, err := a.tlsConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "fingerprint=") || !strings.Contains(out, "expires=") {
		t.Errorf("the start log lacks the certificate fingerprint or expiry: %s", out)
	}
}
