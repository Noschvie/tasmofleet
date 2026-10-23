package web

import "unicode/utf8"

// Tooltip rule: a tooltip explains, it never lists data. One or two short
// sentences, at most tipMax characters; a button gets one only when its label
// does not explain itself. static/tooltip.js cuts at the same length in the
// browser. A title that carries data (a name, an error text) goes through the
// tip template function, which shortens the plain string BEFORE html/template
// escapes it into the attribute; nothing rewrites rendered HTML.
const tipMax = 160

// tip shortens s to at most tipMax characters (runes, never a split character),
// with an ellipsis. Shorter texts are returned as they are.
func tip(s string) string {
	if utf8.RuneCountInString(s) <= tipMax {
		return s
	}
	r := []rune(s)[:tipMax-1]
	for len(r) > 0 && (r[len(r)-1] == ' ' || r[len(r)-1] == '\n') {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}
