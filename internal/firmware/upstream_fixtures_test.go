package firmware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the parsers against real upstream captures
// (testdata/fixtures/firmware-upstream, see its README). The hand-written
// listings used elsewhere assumed quoted hrefs; the real page does not.

func upstreamFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "firmware-upstream", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseOTAListingRealCapture(t *testing.T) {
	cases := []struct {
		fixture string
		must    []string
		prefix  string // every file must have it (directory = family)
	}{
		{"tasmota-release.html", []string{"tasmota.bin.gz", "tasmota-minimal.bin.gz", "tasmota-sensors.bin.gz", "tasmota-lite.bin.gz", "tasmota-4M.bin.gz", "tasmota-DE.bin.gz"}, "tasmota-"},
		{"tasmota32-release.html", []string{"tasmota32.bin", "tasmota32c3.bin", "tasmota32solo1.bin", "tasmota32-safeboot.bin", "tasmota32-DE.bin"}, "tasmota32"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			ver, files, err := parseOTAListing(upstreamFixture(t, tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			// The version comes from the "<h2>Release binaries for Tasmota
			// firmware 16.0.0 Theo ..." heading.
			if ver != "16.0.0" {
				t.Fatalf("version = %q", ver)
			}
			set := map[string]bool{}
			for _, f := range files {
				set[f] = true
				if _, err := parseName(f); err != nil {
					t.Errorf("listed %q does not parse: %v", f, err)
				}
				if strings.Contains(f, "factory") || strings.HasSuffix(f, ".map.gz") {
					t.Errorf("non-OTA file listed: %s", f)
				}
			}
			for _, m := range tc.must {
				if !set[m] {
					t.Errorf("missing %s (got %d files)", m, len(files))
				}
			}
			// The ESP8266 page also links the uncompressed ".bin" "initial
			// firmware" next to every .bin.gz; those are serial-flash images.
			if tc.fixture == "tasmota-release.html" && set["tasmota.bin"] {
				t.Error("uncompressed ESP8266 .bin must not be offered as OTA target")
			}
		})
	}
}

func TestReleasesRealCaptures(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(upstreamFixture(t, "github-releases.json")))
	}))
	defer gh.Close()
	ota := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasmota/release/":
			_, _ = w.Write([]byte(upstreamFixture(t, "tasmota-release.html")))
		case "/tasmota32/release/":
			_, _ = w.Write([]byte(upstreamFixture(t, "tasmota32-release.html")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ota.Close()
	mk := func(src Source) Cache {
		c, err := New(Options{Dir: t.TempDir(), Sources: []Source{src}, BaseURL: "http://x", Inspect: testInspect, Logger: quietLog()}, newMemStore())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ghRel, err := mk(Source{Name: "github", Kind: "github-releases", BaseURL: gh.URL, AllowedHosts: []string{hostOf(gh.URL)}, AllowHTTP: true}).Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	otaRel, err := mk(Source{Name: "ota.tasmota.com", Kind: "ota-tasmota", BaseURL: ota.URL, AllowedHosts: []string{hostOf(ota.URL)}, AllowHTTP: true}).Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ghRel) != 2 || ghRel[0].Version != "16.0.0" || ghRel[1].Version != "15.6.0" {
		t.Fatalf("github releases = %v", versions(ghRel))
	}
	if len(otaRel) != 1 || otaRel[0].Version != "16.0.0" {
		t.Fatalf("ota releases = %v", versions(otaRel))
	}
	// The cross-check fetches every file from both sources, so each file ota
	// lists must also exist on GitHub, otherwise Ensure would reject it.
	onGH := map[string]bool{}
	for _, f := range ghRel[0].Files {
		onGH[f] = true
	}
	for _, f := range otaRel[0].Files {
		if !onGH[f] {
			t.Errorf("%s listed by ota.tasmota.com but not a GitHub 16.0.0 asset", f)
		}
	}
	if len(otaRel[0].Files) < 50 {
		t.Errorf("only %d files parsed from ota listings", len(otaRel[0].Files))
	}
}
