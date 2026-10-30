package extract

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reConstTable = regexp.MustCompile(`(?:(?:static|constexpr)\s+)*const(?:expr)?\s+char\s+(\w+)\s*\[\s*\w*\s*\]\s*(?:PROGMEM\s*)?=`)
	reFuncTable  = regexp.MustCompile(`(?:(?:static|constexpr)\s+)*void\s*\(\s*\*\s*(?:const\s*)?(\w+)\s*\[\s*\w*\s*\]\s*\)\s*\(\s*void\s*\)\s*(?:PROGMEM\s*)?=\s*\{`)
	reDecodeCall = regexp.MustCompile(`DecodeCommand\(\s*([\w:]+)\s*,\s*([\w:]+)\s*(?:,\s*([\w:]+)\s*)?\)`)
	reSynonyms   = regexp.MustCompile(`SO_SYNONYMS\(\s*(\w+)\s*,([^)]*)\)`)
)

func unqualified(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		return s[i+2:]
	}
	return s
}

// exprEnd returns the offset of the ';' that ends the initializer starting at from.
func exprEnd(text string, from int) int {
	depth := 0
	for j := from; j < len(text); j++ {
		switch text[j] {
		case '"', '\'':
			j = skipQuoted(text, j) - 1
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ';':
			if depth <= 0 {
				return j
			}
		}
	}
	return -1
}

type nameTable struct {
	file    *srcFile
	name    string
	prefix  string
	entries []nameEntry
	err     error
}

type nameEntry struct {
	name  string
	conds []string
}

type funcTable struct {
	file  *srcFile
	name  string
	elems []funcElem
}

type funcElem struct {
	handler string // "" when the element is not a plain function pointer
	raw     string
	conds   []string
}

type synTable struct {
	name    string
	indexes []int
}

// collectConsts registers every `const char NAME[] PROGMEM = expr;` so string constants can be
// used like macros, and keeps the offsets so command-name tables can be parsed on demand.
func (w *world) collectConsts() map[*srcFile]map[string][2]int {
	spans := map[*srcFile]map[string][2]int{}
	for _, f := range w.files {
		for _, m := range reConstTable.FindAllStringSubmatchIndex(f.text, -1) {
			name := f.text[m[2]:m[3]]
			start := m[1]
			end := exprEnd(f.text, start)
			if end < 0 {
				continue
			}
			if spans[f] == nil {
				spans[f] = map[string][2]int{}
			}
			spans[f][name] = [2]int{start, end}
			if w.consts == nil {
				w.consts = map[string][]string{}
			}
			expr := strings.TrimSpace(f.text[start:end])
			if f.shared {
				addDistinct(w.consts, name, expr)
			} else {
				addDistinct(w.consts, f.path+"\x00"+name, expr)
			}
		}
	}
	return spans
}

// parseNames reads a command-name table: the concatenated string is split at '|', the first
// segment is the prefix, and every name remembers the #if conditions where it starts.
func (w *world) parseNames(f *srcFile, name string, span [2]int) *nameTable {
	t := &nameTable{file: f, name: name}
	type piece struct {
		str   string
		conds []string
	}
	var pieces []piece
	s := f.text
	for i := span[0]; i < span[1]; {
		if s[i] == ' ' || s[i] == '\n' || s[i] == '\t' || s[i] == '\r' {
			i++
			continue
		}
		start := i
		switch s[i] {
		case '"':
			i = skipQuoted(s, i)
		case '(':
			e := matchClose(s, i)
			if e < 0 {
				t.err = fmt.Errorf("unbalanced parenthesis")
				return t
			}
			i = e + 1
		default:
			id := reIdent.FindString(s[i:])
			if id == "" {
				t.err = fmt.Errorf("unsupported token %q", clip(s[i:]))
				return t
			}
			i += len(id)
			j := i
			for j < span[1] && (s[j] == ' ' || s[j] == '\n') {
				j++
			}
			if j < span[1] && s[j] == '(' {
				e := matchClose(s, j)
				if e < 0 {
					t.err = fmt.Errorf("unbalanced parenthesis")
					return t
				}
				i = e + 1
			}
		}
		str, err := w.evalString(f, s[start:i])
		if err != nil {
			t.err = err
			return t
		}
		pieces = append(pieces, piece{str, f.condsAt(start)})
	}
	var cur strings.Builder
	var curConds []string
	started := false
	first := true
	push := func() {
		if first {
			t.prefix = cur.String()
			first = false
		} else {
			// an empty name is a real entry: the prefix alone is the command ("InfluxDb")
			t.entries = append(t.entries, nameEntry{cur.String(), curConds})
		}
		cur.Reset()
		started = false
	}
	for _, p := range pieces {
		for i := 0; i < len(p.str); i++ {
			if !started {
				curConds = p.conds
				started = true
			}
			if p.str[i] == '|' {
				push()
				continue
			}
			cur.WriteByte(p.str[i])
		}
	}
	if cur.Len() > 0 {
		push()
	}
	return t
}

// parseFuncs reads `void (* const Name[])(void) PROGMEM = { &CmndA, &CmndB, ... };`.
func parseFuncs(f *srcFile) map[string]*funcTable {
	out := map[string]*funcTable{}
	for _, m := range reFuncTable.FindAllStringSubmatchIndex(f.text, -1) {
		open := m[1] - 1
		end := matchClose(f.text, open)
		if end < 0 {
			continue
		}
		t := &funcTable{file: f, name: f.text[m[2]:m[3]]}
		body := f.text[open+1 : end]
		for _, r := range splitTopOffsets(body) {
			raw := strings.TrimSpace(body[r[0]:r[1]])
			if raw == "" {
				continue
			}
			el := funcElem{raw: raw, conds: f.condsAt(open + 1 + r[0] + strings.Index(body[r[0]:r[1]], raw[:1]))}
			if h := strings.TrimPrefix(raw, "&"); regexp.MustCompile(`^[\w:]+$`).MatchString(h) {
				el.handler = unqualified(h)
			}
			t.elems = append(t.elems, el)
		}
		out[t.name] = t
	}
	return out
}

func splitTopOffsets(s string) [][2]int {
	var out [][2]int
	depth, start := 0, 0
	for j := 0; j < len(s); j++ {
		switch s[j] {
		case '"', '\'':
			j = skipQuoted(s, j) - 1
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, [2]int{start, j})
				start = j + 1
			}
		}
	}
	return append(out, [2]int{start, len(s)})
}

func (w *world) parseSynonyms(f *srcFile) map[string]*synTable {
	out := map[string]*synTable{}
	for _, m := range reSynonyms.FindAllStringSubmatch(f.text, -1) {
		t := &synTable{name: m[1]}
		for _, a := range strings.Split(m[2], ",") {
			if strings.TrimSpace(a) == "" {
				continue
			}
			v, _ := w.evalInt(f, a, 0) // an unresolved index stays 0 and is reported as unknown
			t.indexes = append(t.indexes, v)
		}
		out[t.name] = t
	}
	return out
}
