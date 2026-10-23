package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The stylesheet speaks one token vocabulary: the design's (--color-*, --sp-*,
// --r-*, ...). The old app.css names (--bg, --muted, --brand, ...) were kept as aliases
// while the page rules still used them; those rules now use the design names, so an alias
// that comes back means a page rule is written against a second vocabulary again.
func TestStylesheetUsesOnlyDesignSystemTokens(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)

	legacy := []string{
		"bg", "surface", "surface-2", "fg", "muted", "line", "line-strong", "brand", "brand-hover", "brand-fg",
		"brand-soft", "brand-soft-fg", "link", "focus", "ok", "ok-bg", "ok-fg", "warn", "warn-bg", "warn-fg",
		"bad", "bad-bg", "bad-fg", "info", "info-bg", "info-fg", "neutral", "neutral-bg", "neutral-fg",
		"shadow", "radius", "font", "mono", "overlay", "overlay-line", "control-line", "shadow-overlay",
		"toggle-on", "toggle-on-fg",
	}
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?:^|[\s{;])(--[\w-]+)\s*:`).FindAllStringSubmatch(css, -1) {
		defined[m[1]] = true
	}
	for _, name := range legacy {
		if defined["--"+name] {
			t.Errorf("legacy alias token --%s is defined again; use the design-system token", name)
		}
	}

	// A var() of a token nobody defines is silently invalid (the declaration is dropped), which is
	// how a removed alias would fail: no error, just a missing colour.
	// The sticky offsets of pinned columns (--plN, --prN) are measured and set by tables.js on
	// the table element; the rules fall back to 0 until then.
	js, err := os.ReadFile("static/tables.js")
	if err != nil {
		t.Fatal(err)
	}
	runtime := regexp.MustCompile(`^--p[lr]\d+$`)
	if !strings.Contains(string(js), `setProperty("--pl" + n`) || !strings.Contains(string(js), `setProperty("--pr" + n`) {
		t.Error("tables.js no longer sets the pinned-column offsets --plN / --prN")
	}
	for _, m := range regexp.MustCompile(`var\((--[\w-]+)`).FindAllStringSubmatch(css, -1) {
		if !defined[m[1]] && !runtime.MatchString(m[1]) {
			t.Errorf("var(%s) is used but no rule defines it", m[1])
		}
	}
}

// .subnav was the side list of the settings pages until nav.tabs replaced it. No template uses
// it, so its rules (and the rule that hid it next to .jump) are dead weight.
func TestNoRulesForRemovedSubnav(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "subnav") {
		t.Error("app.css still has .subnav rules; the settings pages use nav.tabs")
	}
}
