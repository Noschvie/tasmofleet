package app

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
)

// overrideSource is the single upstream used by FIRMWARE_UPSTREAM_OVERRIDE
// (tests and dev): an ota.tasmota.com style listing at the given URL, or, when
// the URL path ends in /github, a GitHub releases feed (several releases).
func overrideSource(raw string) (firmware.Source, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return firmware.Source{}, fmt.Errorf("TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: invalid URL")
	}
	kind := "ota-tasmota"
	if strings.HasSuffix(u.Path, "/github") {
		kind = "github-releases"
	}
	return firmware.Source{Name: "override", Kind: kind, BaseURL: strings.TrimSuffix(raw, "/"),
		AllowedHosts: []string{u.Host}, AllowHTTP: u.Scheme == "http"}, nil
}

// buildSources maps FIRMWARE_SOURCES names to upstream definitions (spec 8.1).
func buildSources(names []string) ([]firmware.Source, error) {
	var out []firmware.Source
	for _, n := range names {
		switch n {
		case "github":
			out = append(out, firmware.Source{
				Name: "github", Kind: "github-releases", BaseURL: "https://api.github.com/repos/arendst/Tasmota",
				AllowedHosts: []string{"api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"},
			})
		case "ota.tasmota.com":
			out = append(out, firmware.Source{
				Name: "ota.tasmota.com", Kind: "ota-tasmota", BaseURL: "https://ota.tasmota.com",
				AllowedHosts: []string{"ota.tasmota.com"},
			})
		default:
			return nil, fmt.Errorf("TASMOFLEET_FIRMWARE_SOURCES: unknown source %q (known: github, ota.tasmota.com)", n)
		}
	}
	return out, nil
}

// sourceText says where releases come from, for the firmware page: the subtitle ("GitHub
// releases (arendst/Tasmota)") and the short name its status line and failure banner use
// ("GitHub"). It names sources, never their URLs: an override URL may carry credentials.
func sourceText(srcs []firmware.Source) (long, short string) {
	var longs, shorts []string
	for _, s := range srcs {
		switch {
		case s.Name == "override":
			longs, shorts = append(longs, "test upstream (override)"), append(shorts, "the test upstream")
		case s.Kind == "github-releases":
			longs, shorts = append(longs, "GitHub releases (arendst/Tasmota)"), append(shorts, "GitHub")
		default:
			longs, shorts = append(longs, s.Name), append(shorts, s.Name)
		}
	}
	switch len(srcs) {
	case 0:
		return "", ""
	case 1:
		return longs[0], shorts[0]
	}
	return strings.Join(longs, " and "), "the release sources"
}
