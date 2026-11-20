package web

import (
	"io/fs"
	"os"
	"path/filepath"
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

// The pre-v2 component layer (the v1 "Modernist" classes) was removed from app.css (#1069). The
// design system's components are .btn, .bdg, .ucard, .utabs and the tone classes t-*; a v1 class
// that is written again, in the stylesheet or in a template, is a second component system that
// nothing keeps in step with the first (two badges and two buttons in one product, which is how the
// audit log came to render the old glyph badge next to the design system's).
var removedV1Classes = []string{
	"button", "badge", "card", "empty-state", "stats", "tabs", "theme-switch", "navpanel", "tone-info", "tone-bad",
}

// stripCSSComments blanks every comment, keeping the text length.
func stripCSSComments(css string) string {
	return regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllStringFunc(css, func(c string) string {
		return strings.Repeat(" ", len(c))
	})
}

// cssPreludes is every selector list (and at-rule prelude) of a stylesheet with the offset it starts at.
func cssPreludes(css string) (preludes []string, offsets []int) {
	for _, m := range regexp.MustCompile(`[^{};]+\{`).FindAllStringIndex(css, -1) {
		lead := len(css[m[0]:m[1]]) - len(strings.TrimLeft(css[m[0]:m[1]], " \t\r\n"))
		preludes = append(preludes, css[m[0]:m[1]-1])
		offsets = append(offsets, m[0]+lead)
	}
	return preludes, offsets
}

func TestNoPreV2ComponentSelectors(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for _, c := range removedV1Classes {
		removed[c] = true
	}
	classRe := regexp.MustCompile(`\.([A-Za-z_][\w-]*)`)
	preludes, _ := cssPreludes(stripCSSComments(string(raw)))
	for _, p := range preludes {
		// toast.js names its toast "toast tone-<tone>" at run time ("info" for a note that is not a result,
		// form.js), so this one rule keeps the class until the toast has its own tone names (#1102)
		if strings.Contains(p, ".toast.tone-info") {
			continue
		}
		for _, m := range classRe.FindAllStringSubmatch(p, -1) {
			if removed[m[1]] {
				t.Errorf("app.css has a rule for the removed v1 class .%s: %s", m[1], strings.Join(strings.Fields(p), " "))
			}
		}
	}

	// the templates: class="..." and class={ "a", "b-" + x } (the quoted parts are the class names)
	attrRe := regexp.MustCompile(`\bclass=(?:"([^"]*)"|\{([^}]*)\})`)
	literalRe := regexp.MustCompile(`"([^"]*)"`)
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".templ") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range attrRe.FindAllStringSubmatch(string(src), -1) {
			words := m[1]
			for _, l := range literalRe.FindAllStringSubmatch(m[2], -1) {
				words += " " + l[1]
			}
			for _, w := range strings.Fields(words) {
				if removed[w] {
					t.Errorf("%s uses the removed v1 class %q (class=%s); use the design system's component", path, w, m[0])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The bulk bar is one block of the stylesheet. It was declared four times (a v1 block, the nav-patterns
// block, a phone block and the prototype block), each overriding the one before, so a change to the bar
// had to be made in all of them and the winning value was a matter of source order.
func TestBulkBarIsOneBlock(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := stripCSSComments(string(raw))
	start := strings.Index(string(raw), "/* Bulk bar (prototype)")
	end := strings.Index(string(raw), "/* ==== page: Devices (/devices")
	if start < 0 || end < start {
		t.Fatal("the bulk bar block's comment or the Devices page header after it is gone; update this test with the stylesheet")
	}
	preludes, offsets := cssPreludes(css)
	n := 0
	for i, p := range preludes {
		for _, sel := range strings.Split(p, ",") {
			if !strings.HasPrefix(strings.TrimSpace(sel), ".bulkbar") && !strings.HasPrefix(strings.TrimSpace(sel), "body:has(.bulkbar") {
				continue
			}
			n++
			if offsets[i] < start || offsets[i] >= end {
				line := strings.Count(string(raw[:offsets[i]]), "\n") + 1
				t.Errorf("app.css:%d declares %q outside the bulk bar block; the bar's rules live after \"Bulk bar (prototype)\" only", line, strings.TrimSpace(sel))
			}
		}
	}
	if n == 0 {
		t.Error("no bulk bar rule found: the test would pass on nothing")
	}
}
