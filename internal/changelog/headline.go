package changelog

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hellwand/tasmofleet/internal/firmware"
)

// headlineMax is the longest headline in characters; a longer first clause is cut at a word and ends in "…".
const headlineMax = 140

var (
	issueRef = regexp.MustCompile(`\s*#\d+`)
	credit   = regexp.MustCompile(`(?:,\s*)?\bthanks\s+@[\w-]+|@[\w-]+`)
	spaces   = regexp.MustCompile(`\s+`)
	// abbreviations whose full stop does not end a sentence
	abbrev = regexp.MustCompile(`(?i)(?:^|[\s(])(?:e\.g|i\.e|etc|vs|approx)$`)
)

// Headline is the one line What's new shows for an item: what a user needs to know, not what a developer
// needs to find. It is the item's bold lead when it has one (a `user:` fragment line, #1108, or a lead the author
// wrote by hand), else the first clause: the text up to the first ": ", "; " or ". " that stands outside a code
// span, a quote and a parenthesis. Without what a user cannot use: an aside that holds code, an issue number or a
// credit is dropped, other code keeps its words as plain text, "#N" and "thanks @x" go. It is derived on every
// call from the item, so there is no second copy that can drift from the changelog line. "" means nothing is left.
func Headline(it Item) string {
	ins := it.Text
	if len(ins) > 0 && ins[0].Strong && !ins[0].Code {
		lead := tidy(ins[0].Text)
		if !strings.EqualFold(lead, "breaking") { // an old marker ("**breaking**: ..."), not a headline
			return shorten(lead)
		}
		ins = ins[1:]
	}
	return shorten(tidy(firstClause(ins)))
}

// glyph is one character of an item with the inline kind it came from.
type glyph struct {
	r    rune
	code bool
}

func glyphs(ins []firmware.Inline) []glyph {
	var out []glyph
	for _, in := range ins {
		for _, r := range in.Text {
			out = append(out, glyph{r, in.Code})
		}
	}
	return out
}

func text(g []glyph) string {
	var b strings.Builder
	for _, x := range g {
		b.WriteRune(x.r)
	}
	return b.String()
}

// firstClause cuts the text at the first ": ", "; " or ". " outside code, quotes and parentheses and drops the
// asides that only a developer reads.
func firstClause(ins []firmware.Inline) string {
	g := glyphs(ins)
	for len(g) > 0 && (g[0].r == ':' || unicode.IsSpace(g[0].r)) { // what follows an old "**breaking**" marker
		g = g[1:]
	}
	depth, quoted, curly := 0, false, 0
	cut := len(g)
scan:
	for i, x := range g {
		if x.code {
			continue
		}
		switch x.r {
		case '(':
			depth++
		case ')':
			depth = max(depth-1, 0)
		case '"':
			quoted = !quoted
		case '“':
			curly++
		case '”':
			curly = max(curly-1, 0)
		case ':', ';', '.':
			if depth > 0 || quoted || curly > 0 || (i+1 < len(g) && !unicode.IsSpace(g[i+1].r)) {
				continue
			}
			if x.r == '.' && abbrev.MatchString(text(g[:i])) {
				continue
			}
			cut = i
			break scan
		}
	}
	return text(dropAsides(g[:cut]))
}

// dropAsides removes every "(...)" that holds code, an issue number or a credit, with the space before it.
func dropAsides(g []glyph) []glyph {
	var out []glyph
	for i := 0; i < len(g); i++ {
		if g[i].code || g[i].r != '(' {
			out = append(out, g[i])
			continue
		}
		end, depth := len(g), 0
		for j := i; j < len(g); j++ {
			if g[j].code {
				continue
			}
			if g[j].r == '(' {
				depth++
			} else if g[j].r == ')' {
				if depth--; depth == 0 {
					end = j + 1
					break
				}
			}
		}
		aside := g[i:end]
		hasCode := false
		for _, x := range aside {
			hasCode = hasCode || x.code
		}
		if s := text(aside); hasCode || issueRef.MatchString(s) || credit.MatchString(s) {
			for len(out) > 0 && unicode.IsSpace(out[len(out)-1].r) {
				out = out[:len(out)-1]
			}
			i = end - 1
			continue
		}
		out = append(out, g[i])
	}
	return out
}

// tidy removes issue numbers and credits that are left, folds the spaces and trims what ends a clause.
func tidy(s string) string {
	s = issueRef.ReplaceAllString(s, "")
	s = credit.ReplaceAllString(s, "")
	s = strings.NewReplacer("()", "", "( )", "").Replace(s)
	s = spaces.ReplaceAllString(s, " ")
	return strings.Trim(s, " ,;:.")
}

// shorten cuts a headline longer than headlineMax at a word.
func shorten(s string) string {
	if utf8.RuneCountInString(s) <= headlineMax {
		return s
	}
	cut := string([]rune(s)[:headlineMax])
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return strings.Trim(cut, " ,;:.") + "…"
}
