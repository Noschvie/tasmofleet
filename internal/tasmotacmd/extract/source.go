package extract

import (
	"regexp"
	"sort"
	"strings"
)

// srcFile is one C/C++ source file after comment removal and a minimal preprocessor pass:
// directive lines are removed, every remaining line remembers the #if conditions around it,
// and #define lines are collected. Nothing is evaluated: this is a static reading, not a compiler.
type srcFile struct {
	path   string
	shared bool // macros of shared files are visible to every driver file
	text   string
	lineAt []int      // byte offset of the start of each line in text
	conds  [][]string // conditions of each line
	defs   map[string][]string
}

// condFrame is one open #if with the negations of its earlier branches.
type condFrame struct {
	neg []string
	cur string
}

// active lists the conditions that hold inside the current branch: the negations of the earlier
// branches and the branch's own condition (none for a plain #else).
func (f *condFrame) active() []string {
	var out []string
	for _, p := range append(append([]string{}, f.neg...), f.cur) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

var (
	reIntConst  = regexp.MustCompile(`(?m)^(?:static\s+)?const(?:expr)?\s+(?:u?int\d*_t|int|unsigned(?:\s+int)?)\s+(\w+)\s*=\s*([^;]+);`)
	reDirective = regexp.MustCompile(`^#\s*(\w+)\s*(.*)$`)
	reDefine    = regexp.MustCompile(`^([A-Za-z_]\w*)(\(?)\s*(.*)$`)
	reSpaces    = regexp.MustCompile(`\s+`)
)

func wrapCond(c string) string {
	if regexp.MustCompile(`^!?\w+$`).MatchString(c) {
		return c
	}
	return "(" + c + ")"
}

func negate(c string) string {
	if strings.HasPrefix(c, "!") && regexp.MustCompile(`^!\w+$`).MatchString(c) {
		return c[1:]
	}
	return "!" + wrapCond(c)
}

// stripComments blanks // and /* */ comments but keeps strings, char literals and line breaks.
func stripComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\'':
			j := skipQuoted(s, i)
			b.WriteString(s[i:j])
			i = j - 1
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			i-- // leave the newline to the loop
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			j := strings.Index(s[i+2:], "*/")
			end := len(s)
			if j >= 0 {
				end = i + 2 + j + 2
			}
			for k := i; k < end; k++ {
				if s[k] == '\n' {
					b.WriteByte('\n')
				}
			}
			b.WriteByte(' ')
			i = end - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// skipQuoted returns the index after the string or char literal that starts at s[i].
func skipQuoted(s string, i int) int {
	q := s[i]
	j := i + 1
	for j < len(s) && s[j] != '\n' {
		if s[j] == '\\' {
			j += 2
			continue
		}
		if s[j] == q {
			return j + 1
		}
		j++
	}
	if j > len(s) {
		j = len(s)
	}
	return j
}

func loadSource(path, raw string, shared bool) *srcFile {
	f := &srcFile{path: path, shared: shared, defs: map[string][]string{}}
	lines := strings.Split(stripComments(strings.ReplaceAll(raw, "\r\n", "\n")), "\n")
	var stack []*condFrame
	var b strings.Builder
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			for strings.HasSuffix(strings.TrimRight(trimmed, " \t"), "\\") && i+1 < len(lines) {
				trimmed = strings.TrimSuffix(strings.TrimRight(trimmed, " \t"), "\\") + " " + strings.TrimSpace(lines[i+1])
				i++
				// keep line numbering aligned: an empty line stands in for the continued one
				f.addLine(&b, "", stack)
			}
			m := reDirective.FindStringSubmatch(trimmed)
			if m != nil {
				stack = f.directive(stack, m[1], strings.TrimSpace(m[2]))
			}
			f.addLine(&b, "", stack)
			continue
		}
		f.addLine(&b, line, stack)
	}
	f.text = b.String()
	// typed integer constants (const uint8_t MAX_FRIENDLYNAMES = 8;) behave like numeric macros here
	for _, m := range reIntConst.FindAllStringSubmatch(f.text, -1) {
		f.defs[m[1]] = append(f.defs[m[1]], strings.TrimSpace(m[2]))
	}
	return f
}

func (f *srcFile) addLine(b *strings.Builder, text string, stack []*condFrame) {
	f.lineAt = append(f.lineAt, b.Len())
	var c []string
	for _, fr := range stack {
		c = append(c, fr.active()...)
	}
	f.conds = append(f.conds, c)
	b.WriteString(text)
	b.WriteByte('\n')
}

func (f *srcFile) directive(stack []*condFrame, name, arg string) []*condFrame {
	arg = reSpaces.ReplaceAllString(arg, " ")
	switch name {
	case "if":
		stack = append(stack, &condFrame{cur: arg})
	case "ifdef":
		stack = append(stack, &condFrame{cur: arg})
	case "ifndef":
		stack = append(stack, &condFrame{cur: negate(arg)})
	case "elif":
		if n := len(stack); n > 0 {
			fr := stack[n-1]
			fr.neg = append(fr.neg, negate(fr.cur))
			fr.cur = arg
		}
	case "else":
		if n := len(stack); n > 0 {
			fr := stack[n-1]
			fr.neg = append(fr.neg, negate(fr.cur))
			fr.cur = ""
		}
	case "endif":
		if n := len(stack); n > 0 {
			stack = stack[:n-1]
		}
	case "define":
		if m := reDefine.FindStringSubmatch(arg); m != nil && m[2] == "" {
			f.defs[m[1]] = append(f.defs[m[1]], strings.TrimSpace(m[3]))
		}
	}
	return stack
}

// lineOf returns the 0-based line index of a byte offset in f.text.
func (f *srcFile) lineOf(off int) int {
	return sort.Search(len(f.lineAt), func(i int) bool { return f.lineAt[i] > off }) - 1
}

func (f *srcFile) condsAt(off int) []string {
	l := f.lineOf(off)
	if l < 0 || l >= len(f.conds) {
		return nil
	}
	return f.conds[l]
}
