package web

import (
	"os"
	"regexp"
	"strconv"
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
	// tokens of ours that the prototype writes inline instead of as a token, and the one documented
	// deviation (design-system.md "Deviations from the prototype" 13: --color-control-border, WCAG 1.4.11, #829)
	extra := map[string]bool{"--color-grip-hover": true, "--color-control-border": true}

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

// Why: a finger needs 44 px (Apple HIG 44 pt, WCAG 2.5.5 44 px; the status bar row was 44 already), a
// pointer does not, so the phone block of app.css raises the three control tokens and the desktop sizes
// stay (#1169). Every touch target is written as one of these tokens, never as a number, so this block is
// the one place that decides what a phone control measures; the e2e check (mobile-density.spec.ts)
// measures the result. The design system's Controls row states both values; a row that disagrees with the
// stylesheet sends the next person to the wrong number.
func TestPhoneControlTokensAreTouchTargets(t *testing.T) {
	css := readRepoFile(t, "static/app.css")
	desktop := cssDecls(t, css, baseHeader)
	phone := cssDecls(t, css, "@media (max-width: 44.99rem) {\n  :root {")
	var controls string
	for _, line := range strings.Split(readRepoFile(t, "../../docs/spec/design-system.md"), "\n") {
		if strings.HasPrefix(line, "| Controls |") {
			controls = line
		}
	}
	if controls == "" {
		t.Fatal("design-system.md has no Controls row in the token table")
	}
	wantDesktop := map[string]string{"--ctl": "34px", "--ctl-sm": "32px", "--ctl-lg": "40px"}
	for name, want := range wantDesktop {
		if desktop[name] != want {
			t.Errorf("desktop %s = %q, want %q (a pointer needs no more, desktop density stays)", name, desktop[name], want)
		}
		got, ok := phone[name]
		if !ok {
			t.Errorf("the phone :root does not set %s: a phone control would measure %s", name, want)
			continue
		}
		if px, err := strconv.Atoi(strings.TrimSuffix(got, "px")); err != nil || px < 44 {
			t.Errorf("phone %s = %q, want at least 44px (thumb size)", name, got)
		}
		row := regexp.MustCompile("`" + regexp.QuoteMeta(name) + "` (\\d+px)(?: \\((\\d+px) on phones\\))?[,|:]")
		m := row.FindStringSubmatch(controls)
		if m == nil {
			t.Errorf("design-system.md has no `%s` value with its phone size in the Controls row", name)
			continue
		}
		if m[1] != want || m[2] != got {
			t.Errorf("design-system.md says %s is %s (%s on phones), app.css has %s (%s on phones)", name, m[1], m[2], want, got)
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

// ---- the stylesheet writes tokens, not values (#1099) ----------------------------------------------
//
// A value written twice is a value that drifts: the scrim was written four ways, the bottom bars had
// four shadows, the focus ring had nine spellings (UI audit DS-019..DS-032). The design system's tokens
// are only the design system when the stylesheet cannot write the value any other way, so these guards
// read every declaration outside the token blocks.

type cssDecl struct {
	prop, val string
	line      int
}

type cssRule struct {
	header string
	decls  []cssDecl
}

var (
	cssCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssDataURLRe = regexp.MustCompile(`url\((?:"[^"]*"|'[^']*'|[^)]*)\)`)
	cssStringRe  = regexp.MustCompile(`"[^"\n]*"|'[^'\n]*'`)
)

// blankKeepingLines replaces every match of re by repl and keeps the line breaks it covered, so line
// numbers of what follows stay the file's.
func blankKeepingLines(re *regexp.Regexp, s, repl string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string { return repl + strings.Repeat("\n", strings.Count(m, "\n")) })
}

// cssRules returns every block of css that holds declarations (nested at-rules included), with its
// selector text. Comments, url() contents and quoted strings are blanked first: a data URI or a
// content string is not a declaration to judge (an SVG cannot read a custom property).
func cssRules(css string) []cssRule {
	css = blankKeepingLines(cssCommentRe, css, " ")
	css = blankKeepingLines(cssDataURLRe, css, "url()")
	css = blankKeepingLines(cssStringRe, css, `""`)
	type frame struct {
		header string
		decls  []cssDecl
	}
	var (
		rules []cssRule
		stack []*frame
		cur   strings.Builder
		line  = 1
		start = 1
	)
	flushDecl := func() {
		text := strings.TrimSpace(cur.String())
		cur.Reset()
		if text == "" || len(stack) == 0 {
			return
		}
		if prop, val, ok := strings.Cut(text, ":"); ok {
			f := stack[len(stack)-1]
			f.decls = append(f.decls, cssDecl{strings.TrimSpace(prop), strings.Join(strings.Fields(val), " "), start})
		}
	}
	for _, r := range css {
		switch r {
		case '{':
			stack = append(stack, &frame{header: strings.Join(strings.Fields(cur.String()), " ")})
			cur.Reset()
		case ';':
			flushDecl()
		case '}':
			flushDecl()
			if len(stack) > 0 {
				f := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if len(f.decls) > 0 {
					rules = append(rules, cssRule{f.header, f.decls})
				}
			}
		default:
			if cur.Len() == 0 || strings.TrimSpace(cur.String()) == "" {
				if !unicode.IsSpace(r) {
					start = line
				}
			}
			cur.WriteRune(r)
		}
		if r == '\n' {
			line++
		}
	}
	return rules
}

// tokenBlockRe matches the selector of a token block: :root and the theme attributes, nothing else.
var tokenBlockRe = regexp.MustCompile(`^(?::root|\[data-theme[^\]]*\]|:root\[data-theme[^\]]*\])(?:\s*,\s*(?::root|\[data-theme[^\]]*\]|:root\[data-theme[^\]]*\]))*$`)

// outsideTokens calls fn for every declaration that is not in a token block.
func outsideTokens(t *testing.T, fn func(r cssRule, d cssDecl)) {
	t.Helper()
	rules := cssRules(readRepoFile(t, "static/app.css"))
	if len(rules) < 2000 {
		t.Fatalf("parsed %d rules from app.css, the walker lost its way", len(rules))
	}
	for _, r := range rules {
		if tokenBlockRe.MatchString(r.header) {
			continue
		}
		for _, d := range r.decls {
			fn(r, d)
		}
	}
}

func stripImportant(v string) string { return strings.TrimSpace(strings.TrimSuffix(v, "!important")) }

// splitTop splits v at the commas that are not inside parentheses.
func splitTop(v string) []string {
	var parts []string
	depth, from := 0, 0
	for i, r := range v {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(v[from:i]))
				from = i + 1
			}
		}
	}
	return append(parts, strings.TrimSpace(v[from:]))
}

var (
	colourLiteralRe = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(`)
	timeLiteralRe   = regexp.MustCompile(`(?:^|[\s,(])(\d*\.?\d+)(ms|s)\b`)
	lengthRe        = regexp.MustCompile(`\d*\.?\d+(?:px|rem|em)\b`)
	radiusPropRe    = regexp.MustCompile(`^border(?:-[a-z]+)*-radius$`)
	zLiteralRe      = regexp.MustCompile(`^-?\d+$`)
	shadowTokenRe   = regexp.MustCompile(`^var\(--shadow-[\w-]+\)$`)
	ringTokenRe     = regexp.MustCompile(`^var\(--focus-ring\)$`)
	offsetTokenRe   = regexp.MustCompile(`^(?:var\(--focus-offset\)|calc\(-1 \* var\(--focus-offset\)\))$`)
	radiusTokenRe   = regexp.MustCompile(`var\(--r-[\w-]+\)`)
	focusSelectorRe = regexp.MustCompile(`:focus(?:-visible|-within)?\b|\.is-focus\b`)
)

// componentLocalDurations are the animation durations the Motion row of design-system.md lists as
// belonging to one component (spinners, busy pulses, shimmer): the backticked bare times in that row.
func componentLocalDurations(t *testing.T) map[string]bool {
	t.Helper()
	for _, line := range strings.Split(readRepoFile(t, "../../docs/spec/design-system.md"), "\n") {
		if !strings.HasPrefix(line, "| Motion |") {
			continue
		}
		m := map[string]bool{}
		for _, c := range regexp.MustCompile("`(\\d*\\.?\\d+m?s)`").FindAllStringSubmatch(line, -1) {
			m[c[1]] = true
		}
		if len(m) == 0 {
			t.Fatal("the Motion row of design-system.md lists no component-local keyframe duration")
		}
		return m
	}
	t.Fatal("design-system.md has no Motion row in the More tokens table")
	return nil
}

// Why: the design system says a colour, a shadow, a layer, a duration, a radius and the focus ring each
// have one spelling. Without a check the next component copies a neighbour's literal, and a theme
// change or a token change no longer reaches it (the white knob, the scrim in four spellings, three
// bars with their own shadow, radii that equal a token by accident). Every declaration outside the token
// blocks is judged; each subtest fails on the stylesheet before #1099 at the lines the message names.
func TestStylesheetWritesTokens(t *testing.T) {
	report := func(t *testing.T, r cssRule, d cssDecl, why string) {
		t.Helper()
		sel := r.header
		if len(sel) > 70 {
			sel = sel[:70] + "…"
		}
		t.Errorf("app.css:%d %s { %s: %s }: %s", d.line, sel, d.prop, d.val, why)
	}

	t.Run("colours are tokens", func(t *testing.T) {
		outsideTokens(t, func(r cssRule, d cssDecl) {
			if colourLiteralRe.MatchString(d.val) {
				report(t, r, d, "a colour literal; use a --color-* token (a mask writes black)")
			}
		})
	})

	t.Run("shadows are tokens", func(t *testing.T) {
		outsideTokens(t, func(r cssRule, d cssDecl) {
			if d.prop != "box-shadow" && d.prop != "text-shadow" {
				return
			}
			v := stripImportant(d.val)
			if v == "none" || v == "0" || shadowTokenRe.MatchString(v) {
				return
			}
			// an inset indicator (selected row, active tab) is written with a colour token or transparent
			inset := true
			for _, part := range splitTop(v) {
				if !strings.HasPrefix(part, "inset ") || (!strings.Contains(part, "var(") && !strings.Contains(part, "transparent")) {
					inset = false
				}
			}
			if !inset {
				report(t, r, d, "a shadow literal; use --shadow-lg, --shadow-bar or --shadow-knob")
			}
		})
	})

	t.Run("z-index is a layer token or a local 0 to 4", func(t *testing.T) {
		outsideTokens(t, func(r cssRule, d cssDecl) {
			if d.prop != "z-index" || !zLiteralRe.MatchString(stripImportant(d.val)) {
				return
			}
			if n, _ := strconv.Atoi(stripImportant(d.val)); n > 4 {
				report(t, r, d, "a layer above the local stacking values; use a --z-* token")
			}
		})
	})

	t.Run("transitions use duration tokens", func(t *testing.T) {
		outsideTokens(t, func(r cssRule, d cssDecl) {
			if d.prop != "transition" && d.prop != "transition-duration" && d.prop != "transition-delay" {
				return
			}
			for _, m := range timeLiteralRe.FindAllStringSubmatch(d.val, -1) {
				if f, _ := strconv.ParseFloat(m[1], 64); f != 0 {
					report(t, r, d, "a duration literal; use --dur-fast, --dur-base or --dur-slow")
				}
			}
		})
	})

	t.Run("animations use --pulse or a duration the design system lists", func(t *testing.T) {
		local := componentLocalDurations(t)
		outsideTokens(t, func(r cssRule, d cssDecl) {
			if d.prop != "animation" && d.prop != "animation-duration" {
				return
			}
			for _, one := range splitTop(d.val) {
				m := timeLiteralRe.FindStringSubmatch(one) // the first time of an animation is its duration, the second its delay
				if m == nil {
					continue
				}
				if f, _ := strconv.ParseFloat(m[1], 64); f != 0 && !local[m[1]+m[2]] {
					report(t, r, d, "a duration that is neither --pulse nor listed as component-local in design-system.md (More tokens > Motion)")
				}
			}
		})
	})

	t.Run("radii are --r-* tokens", func(t *testing.T) {
		outsideTokens(t, func(r cssRule, d cssDecl) {
			if !radiusPropRe.MatchString(d.prop) {
				return
			}
			v := stripImportant(d.val)
			if strings.Contains(v, "var(--sp-") {
				report(t, r, d, "a spacing token as a radius")
			}
			if lengthRe.MatchString(radiusTokenRe.ReplaceAllString(v, "")) {
				report(t, r, d, "a length literal; use a --r-* token (--r-mark for a 2 px mark, --r-pill for a bar)")
			}
		})
	})

	t.Run("one focus ring", func(t *testing.T) {
		outsideTokens(t, func(r cssRule, d cssDecl) {
			if !focusSelectorRe.MatchString(r.header) {
				return
			}
			v := stripImportant(d.val)
			switch d.prop {
			case "outline":
				if !ringTokenRe.MatchString(v) {
					report(t, r, d, "a focus rule that does not draw var(--focus-ring)")
				}
			case "outline-offset":
				if !offsetTokenRe.MatchString(v) {
					report(t, r, d, "a focus offset that is not var(--focus-offset) or its negative (inside a clipping container)")
				}
			}
		})
	})
}

// Why: the design system page is what the next person reads. The tokens #1099 adds (scrim, on-colours,
// focus, motion, the mark radius) are written there with their values; a value that differs from
// app.css, or a token that app.css has and the page does not name, sends them to the wrong number.
func TestMoreTokensRowsEqualStylesheet(t *testing.T) {
	css := readRepoFile(t, "static/app.css")
	base := cssDecls(t, css, baseHeader)
	doc := readRepoFile(t, "../../docs/spec/design-system.md")
	a := strings.Index(doc, "More tokens.")
	b := strings.Index(doc, "No element is a size container")
	if a < 0 || b < a {
		t.Fatal("design-system.md has no More tokens table")
	}
	more := doc[a:b]
	written := regexp.MustCompile("`(--[\\w-]+): ([^`]+)`")
	seen := map[string]bool{}
	for _, m := range written.FindAllStringSubmatch(more, -1) {
		seen[m[1]] = true
		if got, ok := base[m[1]]; !ok {
			t.Errorf("design-system.md writes %s in More tokens, app.css has no such token on :root", m[1])
		} else if got != normValue(m[2]) {
			t.Errorf("design-system.md says %s is %q, app.css has %q", m[1], m[2], got)
		}
	}
	for _, name := range []string{"--color-scrim", "--color-on-danger", "--color-knob", "--backdrop", "--focus-ring", "--focus-offset", "--dur-fast", "--dur-base", "--dur-slow", "--pulse", "--r-mark"} {
		if _, ok := base[name]; !ok {
			t.Errorf("app.css has no %s on :root", name)
		}
		if !seen[name] {
			t.Errorf("design-system.md does not write %s with its value in More tokens", name)
		}
	}
	// the radii row names every --r-* token with its pixel value
	var radii string
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "| Radii |") {
			radii = line
		}
	}
	for name, val := range base {
		if !strings.HasPrefix(name, "--r-") {
			continue
		}
		m := regexp.MustCompile("`" + regexp.QuoteMeta(name) + "` (\\d+px)").FindStringSubmatch(radii)
		if m == nil || m[1] != val {
			t.Errorf("the Radii row of design-system.md does not say `%s` %s", name, val)
		}
	}
}

// Why: the design system promises that motion stops under prefers-reduced-motion, and most
// transitions are not switched off one by one. The duration tokens are zero there, so every
// transition that reads them stops, and the next one does too without its own media query.
func TestDurationTokensStopUnderReducedMotion(t *testing.T) {
	css := cssDecls(t, readRepoFile(t, "static/app.css"), "@media (prefers-reduced-motion: reduce) {\n  :root {")
	for _, name := range []string{"--dur-fast", "--dur-base", "--dur-slow"} {
		if css[name] != "0s" {
			t.Errorf("%s under prefers-reduced-motion is %q, want 0s", name, css[name])
		}
	}
}
