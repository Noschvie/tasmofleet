package web

import (
	"html"
	"html/template"
	"regexp"
)

// longToken matches an unbreakable run of 32 or more identifier characters: a
// SHA-1/SHA-256 digest, an OIDC subject. Such a token cannot wrap, so inside a
// table cell it makes the whole page wider than a phone.
var longToken = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)

// shortIDs shows every long token of s as its first 8 characters with the full
// value in the title attribute. The rest of s is escaped as usual.
func shortIDs(s string) template.HTML {
	var out []byte
	last := 0
	for _, loc := range longToken.FindAllStringIndex(s, -1) {
		out = append(out, html.EscapeString(s[last:loc[0]])...)
		full := s[loc[0]:loc[1]]
		out = append(out, `<span class="shortid" title="`...)
		out = append(out, html.EscapeString(full)...)
		out = append(out, `">`...)
		out = append(out, full[:8]...)
		out = append(out, `</span>`...)
		last = loc[1]
	}
	out = append(out, html.EscapeString(s[last:])...)
	return template.HTML(out) //nolint:gosec // G203: every byte is escaped above; the tokens are [A-Za-z0-9_-] only
}
