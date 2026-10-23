package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The look is the Claude Design v2 prototype's, value for value (design/v2/README.md
// section 1). Re-deriving a colour "by eye" is how a port drifts from its reference, so the token
// layer of app.css is compared with the prototype's own token lines (design/v2/prototype-tokens.css)
// and with the table in docs/spec/design-system.md, which people read instead of the CSS.

// cssDecls returns the custom properties and color-scheme of the first rule whose selector text is
// header (up to the closing brace; token blocks have no nested rules). Whitespace and line breaks in
// header and stylesheet do not matter: a formatter (prettier) may wrap selectors and values, which
// changes no value.
func cssDecls(t *testing.T, css, header string) map[string]string {
	t.Helper()
	loc := headerRe(header).FindStringIndex(css)
	if loc == nil {
		t.Fatalf("no rule %q", header)
	}
	rest := css[loc[1]:]
	j := strings.Index(rest, "}")
	if j < 0 {
		t.Fatalf("rule %q is not closed", header)
	}
	return declMap(rest[:j])
}

// headerRe matches header with any whitespace (or none) between its characters.
func headerRe(header string) *regexp.Regexp {
	var parts []string
	for _, r := range header {
		if !unicode.IsSpace(r) {
			parts = append(parts, regexp.QuoteMeta(string(r)))
		}
	}
	return regexp.MustCompile(strings.Join(parts, `\s*`))
}

var declRe = regexp.MustCompile(`(--[\w-]+|color-scheme)\s*:\s*([^;]+);?`)

func declMap(body string) map[string]string {
	m := map[string]string{}
	body = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(body, "")
	for _, d := range declRe.FindAllStringSubmatch(body, -1) {
		m[d[1]] = normValue(d[2])
	}
	return m
}

var leadingZeroRe = regexp.MustCompile(`(^|[^\d.])0\.`)

// normValue makes "oklch(0.5 0.13 250)" and "oklch(.5 .13 250)", "rgba(0, 0, 0, 0.3)" and
// "rgba(0,0,0,.3)", "color-mix(in oklab, x)" and the same wrapped over lines
// ("color-mix(\n  in oklab,\n  x\n)") equal, and single quotes double.
func normValue(v string) string {
	v = strings.NewReplacer("'", `"`).Replace(strings.TrimSpace(v))
	v = regexp.MustCompile(`\s*,\s*`).ReplaceAllString(v, ",")
	v = regexp.MustCompile(`\(\s+`).ReplaceAllString(v, "(")
	v = regexp.MustCompile(`\s+\)`).ReplaceAllString(v, ")")
	v = regexp.MustCompile(`\s+`).ReplaceAllString(v, " ")
	for prev := ""; prev != v; { // overlapping matches ("0.5 0.13") need a second pass
		prev = v
		v = leadingZeroRe.ReplaceAllString(v, "${1}.")
	}
	return v
}

// ourName maps a prototype token to ours: the --color- prefix, except for the few that are not colours.
func ourName(proto string) string {
	switch proto {
	case "--sans":
		return "--font-sans"
	case "--mono":
		return "--font-mono"
	case "--ctl", "--shadow-lg":
		return proto
	}
	return "--color-" + strings.TrimPrefix(proto, "--")
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const (
	lightHeader = ":root,\n[data-theme=\"light\"] {"
	darkHeader  = ":root[data-theme=\"dark\"],\n[data-theme=\"dark\"] {"
	autoHeader  = ":root[data-theme-auto] {"
	baseHeader  = ":root {\n  /* type */"
)

// lightTokens is the light block plus the theme-independent :root block (--font-*, --ctl, --backdrop, ...).
func lightTokens(t *testing.T, css string) map[string]string {
	t.Helper()
	m := cssDecls(t, css, baseHeader)
	for k, v := range cssDecls(t, css, lightHeader) {
		m[k] = v
	}
	return m
}

// Why: app.css is prettier-formatted (the editor hook rewrites it on every edit), so a long value or
// selector may be wrapped over lines. A wrap is not a value change; the token comparison must see
// through it, and must still see a real value change inside a wrapped declaration.
func TestCSSDeclsIgnoreLineWraps(t *testing.T) {
	oneLine := ":root,\n[data-theme=\"light\"] {\n  --a: color-mix(in oklab, var(--b) 30%, transparent);\n  --s: 0 1px 2px rgba(0, 0, 0, 0.3), 0 2px 6px #000;\n}"
	wrapped := ":root,\n  [data-theme=\"light\"]\n{\n  --a: color-mix(\n    in oklab,\n    var(--b) 30%,\n    transparent\n  );\n  --s:\n    0 1px 2px rgba(0, 0, 0, .3),\n    0 2px 6px #000;\n}"
	want := cssDecls(t, oneLine, lightHeader)
	got := cssDecls(t, wrapped, lightHeader)
	if len(want) != 2 || len(got) != len(want) {
		t.Fatalf("parsed %d and %d declarations, want 2 each: %q / %q", len(want), len(got), want, got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: one line %q, wrapped %q", k, v, got[k])
		}
	}
	changed := cssDecls(t, strings.Replace(wrapped, "30%", "31%", 1), lightHeader)
	if changed["--a"] == want["--a"] {
		t.Errorf("a changed value inside a wrapped declaration compares equal: %q", changed["--a"])
	}
}

// Why: the prototype's values are the spec. A token that differs, is missing, or was invented in a
// theme the prototype does not redefine it in, means a page is not what Claude Design drew.
func TestTokensEqualThePrototype(t *testing.T) {
	css := readRepoFile(t, "static/app.css")
	proto := readRepoFile(t, "../../design/v2/prototype-tokens.css")
	pl := cssDecls(t, proto, `:root,[data-theme="light"]{`)
	pd := cssDecls(t, proto, `[data-theme="dark"]{`)
	light := lightTokens(t, css)
	dark := cssDecls(t, css, darkHeader)
	// tokens of ours that the prototype writes inline instead of as a token
	extra := map[string]bool{"--color-grip-hover": true}

	for name, want := range pl {
		if name == "color-scheme" {
			continue
		}
		if got := light[ourName(name)]; got != want {
			t.Errorf("light %s: prototype %q, app.css %q", ourName(name), want, got)
		}
	}
	for name, want := range pd {
		if name == "color-scheme" {
			if dark[name] != want {
				t.Errorf("dark color-scheme: %q, want %q", dark[name], want)
			}
			continue
		}
		if got := dark[ourName(name)]; got != want {
			t.Errorf("dark %s: prototype %q, app.css %q", ourName(name), want, got)
		}
	}
	// What the prototype does not redefine in dark (term-fg, term-dim, term-cmd, term-err, sans, mono,
	// ctl) is inherited from the light block, not set again.
	protoDark := map[string]bool{}
	for name := range pd {
		protoDark[ourName(name)] = true
	}
	protoDark["color-scheme"] = true
	for name := range dark {
		if !protoDark[name] && !extra[name] {
			t.Errorf("dark block sets %s, which the prototype does not redefine in dark", name)
		}
	}

	// The no-JS fallback (prefers-color-scheme) must be the explicit dark theme, token for token.
	auto := cssDecls(t, css, autoHeader)
	if len(auto) != len(dark) {
		t.Errorf("auto-dark block has %d declarations, explicit dark %d", len(auto), len(dark))
	}
	for name, want := range dark {
		if auto[name] != want {
			t.Errorf("auto-dark %s = %q, explicit dark %q", name, auto[name], want)
		}
	}
}

// Why: the spec page is what the next person reads; a value there that app.css does not have is a
// lie. Rows look like | `--color-bg` | `#f4f5f7` | `#0d1013` | (dark "same" = not redefined).
func TestDesignSystemTokenTableEqualsStylesheet(t *testing.T) {
	css := readRepoFile(t, "static/app.css")
	light := lightTokens(t, css)
	dark := cssDecls(t, css, darkHeader)
	doc := readRepoFile(t, "../../docs/spec/design-system.md")
	a := strings.Index(doc, "<!-- tokens:start -->")
	b := strings.Index(doc, "<!-- tokens:end -->")
	if a < 0 || b < a {
		t.Fatal("design-system.md has no <!-- tokens:start --> ... <!-- tokens:end --> table")
	}
	row := regexp.MustCompile("^\\| `(--[\\w-]+)` \\| `([^`]+)` \\| (?:`([^`]+)`|same) \\|")
	seen := map[string]bool{}
	for _, line := range strings.Split(doc[a:b], "\n") {
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		seen[name] = true
		if got := light[name]; got != normValue(m[2]) {
			t.Errorf("design-system.md light %s = %q, app.css %q", name, m[2], got)
		}
		wantDark := m[3]
		if wantDark == "" {
			if _, ok := dark[name]; ok {
				t.Errorf("design-system.md says %s is the same in dark, app.css redefines it", name)
			}
		} else if got := dark[name]; got != normValue(wantDark) {
			t.Errorf("design-system.md dark %s = %q, app.css %q", name, wantDark, got)
		}
	}
	for name := range light {
		if strings.HasPrefix(name, "--color-") && !seen[name] {
			t.Errorf("%s is in app.css but not in the design-system.md token table", name)
		}
	}
}

// Why: D2/D3 delete the v1 vocabulary without aliases. A v1 colour name that comes back means a rule is
// written against the old teal/ramp system again, which the prototype does not have.
func TestNoV1ColourVocabulary(t *testing.T) {
	css := readRepoFile(t, "static/app.css")
	v1 := regexp.MustCompile(`--(?:color-(?:success|warning|info)\b[\w-]*|color-(?:accent|danger|neutral)-\d{3}|color-(?:divider|rule|overlay|hover|press|link|focus|selected|power-on|on-power|border-overlay|accent-soft[\w-]*|accent-active)\b|prim-[\w-]+|space-\d+|radius-\w+|rule-w|control-[\w-]+|font-heading[\w-]*|font-body)`)
	for _, m := range v1.FindAllString(css, -1) {
		t.Errorf("v1 token %s is still in app.css", m)
	}
}
