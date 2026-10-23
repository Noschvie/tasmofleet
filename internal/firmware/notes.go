package firmware

import (
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Release notes come from the body of a GitHub release: text written by
// third parties and fetched from the internet, so it is never rendered as
// HTML. ParseNotes turns the small, common subset of Markdown (headings,
// lists, code, bold, links) into a plain data structure and drops everything
// else; the template then prints every string through html/template's
// escaping. There is no template.HTML anywhere on this path, so no input can
// produce markup, script, style or an attribute of its own. Links survive
// only if SafeLink accepts them.

// Limits that bound the work and the memory one release body can cause.
const (
	// MaxNotesBytes is how much of a release body the cache keeps.
	MaxNotesBytes = 32 << 10
	maxNoteLines  = 1500
	maxLineBytes  = 4000
	maxLinkBytes  = 2048
)

// Block kinds of a parsed note.
const (
	NoteParagraph = "p"
	NoteHeading   = "h"
	NoteBullets   = "ul"
	NoteOrdered   = "ol"
	NoteCode      = "pre"
	NoteQuote     = "quote"
)

// NoteBlock is one block of a release note.
type NoteBlock struct {
	Kind    string
	Inlines []Inline   // paragraph, heading, quote
	Items   [][]Inline // bullets, ordered
	Code    string     // pre
}

// Inline is a run of text: plain, code, bold, or a link (Href is set only
// for a link that passed SafeLink).
type Inline struct {
	Text   string
	Href   string
	Code   bool
	Strong bool
}

// SafeLink returns the normalised form of raw if it is an absolute http or
// https URL with a host and no user info, and false otherwise. It is the one
// gate for every link taken from upstream text: javascript:, data:, file:,
// relative and protocol-relative URLs, and anything with control characters
// or whitespace are refused.
func SafeLink(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxLinkBytes {
		return "", false
	}
	for _, r := range raw {
		if r < 0x21 || r == 0x7f || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\\' {
			return "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	if p := u.Port(); p != "" && (scheme != "http" || p != "80") && (scheme != "https" || p != "443") {
		return "", false // a non-standard port is a service, not a document
	}
	if !publicHostname(u.Hostname()) {
		return "", false
	}
	return u.String(), true
}

// publicHostname accepts a multi-label DNS name that is not a local or
// private suffix. IP literals (v4, v6), "localhost" and single-label names
// are refused: a link in third-party text must not point into the reader's
// own network (a Tasmota device without web password runs GET commands).
func publicHostname(h string) bool {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if h == "" || strings.ContainsAny(h, ":[]") {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	tld := labels[len(labels)-1]
	if tld == "" || (tld[0] >= '0' && tld[0] <= '9') { // 1.2.3, 0x7f.1, 127.1: old IP spellings
		return false
	}
	switch tld {
	case "local", "localhost", "internal", "lan", "home", "localdomain", "intranet", "corp", "private", "arpa", "test", "invalid", "example":
		return false
	}
	return true
}

// DefaultNoteLinkHosts are the hosts a link in release notes may point to.
// "*.example.org" matches any subdomain of example.org (not example.org itself).
var DefaultNoteLinkHosts = []string{"github.com", "*.github.com", "tasmota.github.io", "tasmota.com", "*.tasmota.com"}

// linkHostAllowed reports whether the (already SafeLink-checked) URL's host is
// on the allow-list; an empty list means DefaultNoteLinkHosts.
func linkHostAllowed(safe string, hosts []string) bool {
	if len(hosts) == 0 {
		hosts = DefaultNoteLinkHosts
	}
	u, err := url.Parse(safe)
	if err != nil {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	for _, a := range hosts {
		a = strings.ToLower(a)
		if suffix, wild := strings.CutPrefix(a, "*."); wild {
			if strings.HasSuffix(h, "."+suffix) {
				return true
			}
		} else if h == a {
			return true
		}
	}
	return false
}

// NoteLink is SafeLink plus the host allow-list. Everything else in release
// notes renders as plain text.
func NoteLink(raw string, hosts []string) (string, bool) {
	safe, ok := SafeLink(raw)
	if !ok || !linkHostAllowed(safe, hosts) {
		return "", false
	}
	return safe, true
}

// TruncateNotes cuts s to MaxNotesBytes on a rune boundary and reports
// whether it cut.
func TruncateNotes(s string) (string, bool) {
	if len(s) <= MaxNotesBytes {
		return s, false
	}
	s = s[:MaxNotesBytes]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s, true
}

var (
	headingLine = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)[ \t#]*$`)
	bulletLine  = regexp.MustCompile(`^( *)[-*+][ \t]+(.*)$`)
	orderedLine = regexp.MustCompile(`^( *)\d{1,9}[.)][ \t]+(.*)$`)
	ruleLine    = regexp.MustCompile(`^ {0,3}(?:-[ \t]*){3,}$|^ {0,3}(?:\*[ \t]*){3,}$|^ {0,3}(?:_[ \t]*){3,}$`)
	quoteLine   = regexp.MustCompile(`^ {0,3}>[ \t]?(.*)$`)
	// htmlBlockLine is a line that starts like a raw HTML block (<picture>,
	// <img ...>, </details>, <!-- ...). GitHub renders these; we never do, and
	// showing their source as text only litters the changelog, so they are
	// dropped. Inline HTML inside a text line stays literal text.
	htmlBlockLine = regexp.MustCompile(`^ {0,3}<(?:[A-Za-z][A-Za-z0-9-]*[ \t/>]|[A-Za-z][A-Za-z0-9-]*$|/[A-Za-z]|!)`)
)

// ParseNotes parses a release body. It never fails: unknown constructs
// become plain text.
func ParseNotes(src string) []NoteBlock { return ParseNotesWith(src, nil) }

// ParseNotesWith is ParseNotes with an explicit link host allow-list (nil =
// DefaultNoteLinkHosts).
func ParseNotesWith(src string, hosts []string) []NoteBlock {
	src, _ = TruncateNotes(src)
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(src, "\n")
	if len(lines) > maxNoteLines {
		lines = lines[:maxNoteLines]
	}
	var out []NoteBlock
	var para, quote []string
	var list *NoteBlock
	flush := func() {
		if len(para) > 0 {
			out = append(out, NoteBlock{Kind: NoteParagraph, Inlines: parseInline(hosts, strings.Join(para, " "))})
			para = nil
		}
		if len(quote) > 0 {
			out = append(out, NoteBlock{Kind: NoteQuote, Inlines: parseInline(hosts, strings.Join(quote, " "))})
			quote = nil
		}
		if list != nil {
			out = append(out, *list)
			list = nil
		}
	}
	addItem := func(kind, text string) {
		if list != nil && list.Kind != kind {
			flush()
		}
		if list == nil {
			list = &NoteBlock{Kind: kind}
		}
		list.Items = append(list.Items, parseInline(hosts, text))
	}
	inComment := false
	for i := 0; i < len(lines); i++ {
		line := clip(lines[i])
		if inComment {
			inComment = !strings.Contains(line, "-->")
			continue
		}
		if htmlBlockLine.MatchString(line) {
			if strings.HasPrefix(strings.TrimLeft(line, " "), "<!--") && !strings.Contains(line, "-->") {
				inComment = true
			}
			continue
		}
		if t := strings.TrimLeft(line, " "); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			flush()
			fence := t[:3]
			var code []string
			for i++; i < len(lines); i++ {
				l := clip(lines[i])
				if strings.HasPrefix(strings.TrimLeft(l, " "), fence) {
					break
				}
				code = append(code, l)
			}
			out = append(out, NoteBlock{Kind: NoteCode, Code: strings.Join(code, "\n")})
			continue
		}
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case ruleLine.MatchString(line):
			flush()
		case headingLine.MatchString(line):
			flush()
			m := headingLine.FindStringSubmatch(line)
			out = append(out, NoteBlock{Kind: NoteHeading, Inlines: parseInline(hosts, m[2])})
		case bulletLine.MatchString(line):
			if len(para) > 0 || len(quote) > 0 {
				flush()
			}
			addItem(NoteBullets, bulletLine.FindStringSubmatch(line)[2])
		case orderedLine.MatchString(line):
			if len(para) > 0 || len(quote) > 0 {
				flush()
			}
			addItem(NoteOrdered, orderedLine.FindStringSubmatch(line)[2])
		case quoteLine.MatchString(line):
			if len(para) > 0 || list != nil {
				flush()
			}
			quote = append(quote, strings.TrimSpace(quoteLine.FindStringSubmatch(line)[1]))
		case list != nil && strings.HasPrefix(line, " "):
			// continuation of the last list item
			last := &list.Items[len(list.Items)-1]
			*last = append(*last, parseInline(hosts, " "+strings.TrimSpace(line))...)
		default:
			if list != nil || len(quote) > 0 {
				flush()
			}
			para = append(para, strings.TrimSpace(line))
		}
	}
	flush()
	return out
}

func clip(s string) string {
	if len(s) <= maxLineBytes {
		return s
	}
	s = s[:maxLineBytes]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

// parseInline reads code spans, **bold**, [text](url), ![alt](url) (as a
// link: images are never loaded) and bare http(s) URLs. Anything else,
// including raw HTML, stays literal text.
func parseInline(hosts []string, s string) []Inline {
	var out []Inline
	var text strings.Builder
	emit := func() {
		if text.Len() > 0 {
			out = append(out, Inline{Text: text.String()})
			text.Reset()
		}
	}
	for i := 0; i < len(s); {
		rest := s[i:]
		switch {
		case rest[0] == '`':
			if end := strings.IndexByte(rest[1:], '`'); end > 0 {
				emit()
				out = append(out, Inline{Text: rest[1 : 1+end], Code: true})
				i += end + 2
				continue
			}
		case strings.HasPrefix(rest, "**"):
			if end := strings.Index(rest[2:], "**"); end > 0 {
				emit()
				out = append(out, Inline{Text: rest[2 : 2+end], Strong: true})
				i += end + 4
				continue
			}
		case rest[0] == '[' || strings.HasPrefix(rest, "!["):
			skip := 0
			if rest[0] == '!' {
				skip = 1
			}
			if label, href, n, ok := parseLink(rest[skip:]); ok {
				emit()
				if safe, good := NoteLink(href, hosts); good {
					out = append(out, Inline{Text: label, Href: safe})
				} else {
					text.WriteString(label) // unsafe target: keep the words, drop the link
					emit()
				}
				i += skip + n
				continue
			}
		case strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://"):
			end := strings.IndexAny(rest, " \t<>\"'`)]")
			if end < 0 {
				end = len(rest)
			}
			raw := strings.TrimRight(rest[:end], ".,;:!?")
			if safe, ok := NoteLink(raw, hosts); ok {
				emit()
				out = append(out, Inline{Text: raw, Href: safe})
				i += len(raw)
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(rest)
		text.WriteRune(r)
		i += size
	}
	emit()
	return out
}

// parseLink parses "[label](target)" at the start of s and returns the label,
// the target (without an optional "title") and the bytes consumed.
func parseLink(s string) (label, href string, n int, ok bool) {
	close := strings.IndexByte(s, ']')
	if close < 1 || !strings.HasPrefix(s[close:], "](") {
		return "", "", 0, false
	}
	end, depth := -1, 0
	for i := close + 2; i < len(s) && end < 0; i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				end = i - (close + 2)
			}
			depth--
		}
	}
	if end < 0 {
		return "", "", 0, false
	}
	target := strings.TrimSpace(s[close+2 : close+2+end])
	if sp := strings.IndexAny(target, " \t"); sp > 0 {
		target = target[:sp]
	}
	return s[1:close], target, close + 2 + end + 1, true
}

// ParseInline reads one line of text like a release note's paragraph: code spans, bold and
// links through NoteLink (hosts nil = DefaultNoteLinkHosts). Package changelog reuses it.
func ParseInline(s string, hosts []string) []Inline { return parseInline(hosts, s) }
