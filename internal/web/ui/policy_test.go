package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// forbidden are the templ features that produce inline script or style, or turn data into
// markup. The content security policy is `default-src 'self'` without 'unsafe-inline', so a
// page using any of them is blocked by the browser; templ.Raw would also bypass escaping.
// Server-built URLs go through templ.URL / templ.SafeURL, which stay allowed.
var forbidden = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`(?m)^\s*script\s+\w+\(`), "script component (inline script)"},
	{regexp.MustCompile(`(?m)^\s*css\s+\w+\(`), "css component (inline style)"},
	{regexp.MustCompile(`templ\.(Raw|Unsafe|JSFuncCall|JSExpression|JSUnsafeFuncCall|ComponentScript|JSONScript|ConstantScript|WithNonce|OnceHandle)\b`), "templ feature that emits markup or script from data"},
	{regexp.MustCompile(`(?i)<script\b[^>]*>\s*[^<\s]`), "inline <script>"},
	{regexp.MustCompile(`(?i)<style\b`), "<style> element"},
	{regexp.MustCompile(`(?i)\sstyle\s*=`), "style attribute"},
	{regexp.MustCompile(`(?i)\son[a-z]+\s*=`), "inline event handler"},
}

func violations(src string) []string {
	var out []string
	for _, f := range forbidden {
		if f.re.MatchString(src) {
			out = append(out, f.what)
		}
	}
	return out
}

// Why: see forbidden. The check runs on the sources, so a violation fails here with the
// reason instead of showing up as a blocked script in somebody's browser.
func TestTemplSourcesUseNoForbiddenFeatures(t *testing.T) {
	files, err := filepath.Glob("*.templ")
	if err != nil {
		t.Fatal(err)
	}
	more, err := filepath.Glob("../*.templ")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, more...)
	if len(files) == 0 {
		t.Fatal("no .templ files found: the check looks in the wrong place")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range violations(string(b)) {
			t.Errorf("%s: %s", f, v)
		}
	}
}

// Why: a check that never fires protects nothing. Each forbidden feature must be caught.
func TestForbiddenFeaturesAreDetected(t *testing.T) {
	for src, want := range map[string]string{
		"script confirm(msg string) {\n}":               "script component",
		"css bar() {\n}":                                "css component",
		"@templ.Raw(x)":                                 "templ feature",
		"<script>alert(1)</script>":                     "inline <script>",
		"<style>a{}</style>":                            "<style>",
		`<div style="width:3px">`:                       "style attribute",
		`<button onclick="x()">`:                        "inline event handler",
		`<script src="/a.js"></script> <p style={ x }>`: "style attribute",
	} {
		got := strings.Join(violations(src), ";")
		if !strings.Contains(got, want) {
			t.Errorf("%q: got %q, want a violation containing %q", src, got, want)
		}
	}
	if v := violations(`<script src="/a.js" defer></script><a href={ templ.SafeURL(u) }>x</a>`); len(v) > 0 {
		t.Errorf("allowed markup flagged: %v", v)
	}
}
