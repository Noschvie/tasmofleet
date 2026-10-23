package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
)

// Why: firmware is flashed onto the whole fleet, so no download may travel over plain
// HTTP by default or through an environment proxy that could rewrite it.
func TestDefaultSourcesAreHTTPSOnly(t *testing.T) {
	srcs, err := buildSources([]string{"github", "ota.tasmota.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if s.AllowHTTP || len(s.BaseURL) < 8 || s.BaseURL[:8] != "https://" {
			t.Errorf("%s must be https only: %+v", s.Name, s)
		}
	}
}

func TestFirmwareClientIgnoresEnvProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	tr, ok := firmwareClient().Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatal("the firmware client must not use proxy settings from the environment")
	}
}

func TestOverrideSourceAllowsOnlyItsHost(t *testing.T) {
	s, err := overrideSource("http://127.0.0.1:5555/")
	if err != nil || len(s.AllowedHosts) != 1 || s.AllowedHosts[0] != "127.0.0.1:5555" || !s.AllowHTTP {
		t.Fatalf("%+v %v", s, err)
	}
}

// The devenv serves several releases only through the GitHub feed format; the
// listing format carries one, so the override URL picks the format.
func TestOverrideSourceKindFollowsURL(t *testing.T) {
	for url, kind := range map[string]string{
		"http://127.0.0.1:5555":        "ota-tasmota",
		"http://127.0.0.1:5555/github": "github-releases",
	} {
		s, err := overrideSource(url)
		if err != nil || s.Kind != kind {
			t.Fatalf("%s: kind %q, want %q (%v)", url, s.Kind, kind, err)
		}
	}
}

// Why: the firmware page names where its releases come from and which source failed. It must
// name the sources a reader knows, and never print a source URL (an override URL can carry
// credentials).
func TestSourceText(t *testing.T) {
	both, err := buildSources([]string{"github", "ota.tasmota.com"})
	if err != nil {
		t.Fatal(err)
	}
	gh, _ := buildSources([]string{"github"})
	ota, _ := buildSources([]string{"ota.tasmota.com"})
	over, err := overrideSource("http://user:secret@127.0.0.1:5555/github")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name        string
		srcs        []firmware.Source
		long, short string
	}{
		{"github", gh, "GitHub releases (arendst/Tasmota)", "GitHub"},
		{"ota", ota, "ota.tasmota.com", "ota.tasmota.com"},
		{"both", both, "GitHub releases (arendst/Tasmota) and ota.tasmota.com", "the release sources"},
		{"override", []firmware.Source{over}, "test upstream (override)", "the test upstream"},
		{"none", nil, "", ""},
	} {
		long, short := sourceText(c.srcs)
		if long != c.long || short != c.short {
			t.Errorf("%s: %q / %q, want %q / %q", c.name, long, short, c.long, c.short)
		}
		if strings.Contains(long+short, "secret") || strings.Contains(long+short, "://") {
			t.Errorf("%s: a URL leaked into %q / %q", c.name, long, short)
		}
	}
}
