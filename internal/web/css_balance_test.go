package web

import (
	"os"
	"regexp"
	"testing"
)

// Why: an @media block of app.css lost its closing brace, so every rule after it (the whole
// rest of the sheet) applied only inside that media query and silently vanished on desktop.
// Nothing in the build parses the CSS, so this checks that braces balance (every { has its }).
func TestStylesheetBracesBalance(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(raw), "")
	css = regexp.MustCompile(`"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'`).ReplaceAllString(css, `""`)
	depth, line := 0, 1
	for _, r := range css {
		switch r {
		case '\n':
			line++
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				t.Fatalf("app.css: unmatched } near line %d (comments stripped)", line)
			}
		}
	}
	if depth != 0 {
		t.Errorf("app.css: %d unclosed block(s); the rules after the last unclosed @media apply only inside it", depth)
	}
}
