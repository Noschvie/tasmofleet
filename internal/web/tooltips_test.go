package web

import (
	"html"
	"html/template"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// sentences counts the sentences of a tooltip text (a full stop, question or
// exclamation mark followed by a space or the end).
func sentences(s string) int {
	return len(regexp.MustCompile(`[.!?](\s|$)`).FindAllString(strings.TrimSpace(s), -1))
}

// Why: a title that carries data is shortened as a plain string before it is
// escaped into the attribute. Escaping stays html/template's job: a value with
// quotes, angle brackets, an ampersand and an event-handler payload must come
// out inert, and a long one must be cut on a character boundary.
func TestTipShortensBeforeEscaping(t *testing.T) {
	tpl := template.Must(template.New("t").Funcs(template.FuncMap{"tip": tip}).Parse(`<span title="{{tip .}}">x</span>`))
	render := func(v string) string {
		var b strings.Builder
		if err := tpl.Execute(&b, v); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	payload := `" onmouseover="alert(1)" x="<b>&'` + "`"
	out := render(payload)
	if strings.Contains(out, `" onmouseover=`) || strings.Contains(out, "<b>") {
		t.Fatalf("payload escaped the attribute: %s", out)
	}
	if !strings.Contains(out, "onmouseover=") || !strings.Contains(out, "&#34;") {
		t.Fatalf("the payload should stay visible, inert, as text: %s", out)
	}
	long := strings.Repeat("\u00e4\"<&", 100) // multi-byte characters, each next to an escaped one
	m := regexp.MustCompile(`title="([^"]*)"`).FindStringSubmatch(render(long))
	if m == nil {
		t.Fatalf("no title: %s", render(long))
	}
	plain := html.UnescapeString(m[1])
	if utf8.RuneCountInString(plain) > tipMax || !utf8.ValidString(plain) || !strings.HasSuffix(plain, "\u2026") {
		t.Fatalf("not cut cleanly: %d runes, valid=%v", utf8.RuneCountInString(plain), utf8.ValidString(plain))
	}
	if tip("short & sweet") != "short & sweet" {
		t.Error("a short text must stay as it is")
	}
}

// Why: the explanation texts the UI keeps in one place (status words, chips,
// column headers) must follow the rule themselves: 1-2 short sentences.
func TestKnownTooltipsAreShort(t *testing.T) {
	check := func(kind, key, text string) {
		if utf8.RuneCountInString(text) > tipMax {
			t.Errorf("%s %q is %d characters, the limit is %d", kind, key, utf8.RuneCountInString(text), tipMax)
		}
		if n := sentences(text); n > 2 {
			t.Errorf("%s %q has %d sentences, at most 2", kind, key, n)
		}
	}
	for k, v := range hints {
		check("hint", k, v)
	}
	for k, v := range colTips {
		check("column tip", k, v)
	}
	files, _ := fs.Glob(Assets, "templates/*.html")
	titleRe := regexp.MustCompile(`\btitle="([^"{]*)"`) // static titles; dynamic ones are capped at render time
	for _, f := range files {
		b, err := fs.ReadFile(Assets, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range titleRe.FindAllStringSubmatch(string(b), -1) {
			check(f, m[1][:min(len(m[1]), 30)], m[1])
		}
	}
}

// Why: the Preview button's label says what it does; its tooltip once listed the
// selected devices. No control of the update pages carries a tooltip with a list.
func TestUpdatePagesHaveNoListTooltips(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	_, body := e.do(t, "GET", "/updates/new?select=all", "operator", nil)
	titles := regexp.MustCompile(`\b(?:title|data-tip)="([^"]*)"`).FindAllStringSubmatch(body, -1)
	for _, m := range titles {
		if strings.Contains(m[1], "kitchen") || strings.Contains(m[1], "attic") {
			t.Errorf("a tooltip names devices: %q", m[1])
		}
	}
	for _, btn := range regexp.MustCompile(`<button[^>]*>Preview`).FindAllString(body, -1) {
		if strings.Contains(btn, "title=") || strings.Contains(btn, "data-tip=") {
			t.Errorf("the Preview button needs no tooltip: %s", btn)
		}
	}
}
