package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Why: browsers, home screens and proxies fetch icons without a session and
// by content type; a wrong type or a 404 shows a blank tab icon or breaks the
// PWA install prompt.
func TestBrandingFilesAreServedWithRightType(t *testing.T) {
	e := setup(t)
	for path, wantType := range map[string]string{
		"/favicon.ico":                 "image/",
		"/static/favicon.svg":          "image/svg+xml",
		"/static/apple-touch-icon.png": "image/png",
		"/static/icon-192.png":         "image/png",
		"/static/icon-512.png":         "image/png",
		"/static/manifest.webmanifest": "application/manifest+json",
	} {
		resp, body := e.do(t, "GET", path, "", nil)
		if resp.StatusCode != 200 || len(body) == 0 {
			t.Errorf("%s: status %d, %d bytes", path, resp.StatusCode, len(body))
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, wantType) {
			t.Errorf("%s: content type %q, want %q", path, ct, wantType)
		}
	}
}

// Why: every page must advertise the icons, under the base path.
func TestLayoutLinksIcons(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	for _, want := range []string{
		`rel="icon" href="/tf/favicon.ico" sizes="48x48"`,
		`rel="icon" href="/tf/static/favicon.svg?v=`,
		`rel="apple-touch-icon" href="/tf/static/apple-touch-icon.png?v=`,
		`rel="manifest" href="/tf/static/manifest.webmanifest"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("layout lacks %q", want)
		}
	}
}

// Why: the manifest must only name icons that exist, or installs fail silently.
func TestManifestIconsExist(t *testing.T) {
	raw, err := Assets.ReadFile("static/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Icons []struct{ Src string }
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Icons) < 2 {
		t.Fatalf("want 192 and 512 icons, got %d", len(m.Icons))
	}
	for _, i := range m.Icons {
		if _, err := Assets.ReadFile("static/" + i.Src); err != nil {
			t.Errorf("manifest icon %s: %v", i.Src, err)
		}
	}
}

// Why: the embedded copies are what ships; if they drift from the committed
// source in assets/branding the Unraid, HA and README icons no longer match
// the UI. Also: the CSP forbids inline script, so an SVG must not carry any.
func TestEmbeddedIconsMatchBrandingSources(t *testing.T) {
	for _, name := range []string{"favicon.ico", "favicon.svg", "apple-touch-icon.png", "icon-192.png", "icon-512.png"} {
		src, err := os.ReadFile(filepath.Join("..", "..", "assets", "branding", name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := Assets.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(src, got) {
			t.Errorf("%s differs from assets/branding; re-copy it", name)
		}
	}
	svgs, _ := filepath.Glob(filepath.Join("..", "..", "assets", "branding", "*.svg"))
	if len(svgs) == 0 {
		t.Fatal("no source SVGs")
	}
	for _, f := range svgs {
		b, _ := os.ReadFile(f)
		if bytes.Contains(bytes.ToLower(b), []byte("<script")) {
			t.Errorf("%s contains a script element", f)
		}
	}
}

// Why: SHA256SUMS pins the vendored scripts, the fonts and the icons (NOTICE points at it); a
// file changed without regenerating it (design/tools/brand-icons.sh) makes the pin a lie.
func TestStaticChecksumsMatch(t *testing.T) {
	raw, err := Assets.ReadFile("static/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			t.Fatalf("malformed line %q", line)
		}
		b, err := Assets.ReadFile("static/" + f[1])
		if err != nil {
			t.Errorf("%s: %v", f[1], err)
			continue
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != f[0] {
			t.Errorf("%s: checksum differs from SHA256SUMS", f[1])
		}
	}
}
