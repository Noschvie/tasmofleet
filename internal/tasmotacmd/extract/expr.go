package extract

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// world is every file that was read plus the lookup rules for macros: a macro defined in the
// file itself wins, otherwise the shared files (include/, tasmota_support/, language/en_GB.h)
// decide. A name with two different definitions is ambiguous and never guessed.
type world struct {
	files  []*srcFile
	shared map[string][]string // macro -> distinct values from shared files
	consts map[string][]string // const char NAME[] PROGMEM = expr; (raw expressions)
}

func addDistinct(m map[string][]string, k, v string) {
	for _, e := range m[k] {
		if e == v {
			return
		}
	}
	m[k] = append(m[k], v)
}

func (w *world) lookup(f *srcFile, name string) ([]string, bool) {
	if v, ok := distinctOf(f.defs[name]); ok {
		return v, true
	}
	if v, ok := w.shared[name]; ok {
		return v, true
	}
	if v, ok := w.consts[f.path+"\x00"+name]; ok {
		return v, true
	}
	if v, ok := w.consts[name]; ok {
		return v, true
	}
	return nil, false
}

func distinctOf(vals []string) ([]string, bool) {
	var out []string
	for _, v := range vals {
		dup := false
		for _, o := range out {
			dup = dup || o == v
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out, len(out) > 0
}

var reIdent = regexp.MustCompile(`^[A-Za-z_]\w*`)

// evalString evaluates a C string-literal expression: literals, PSTR(...), parentheses and
// macros that expand to such expressions, concatenated. Anything else is an error that names
// the cause; that error becomes the "loose" reason of whatever depended on it.
func (w *world) evalString(f *srcFile, expr string) (string, error) {
	return w.evalStringDepth(f, expr, 0)
}

func (w *world) evalStringDepth(f *srcFile, expr string, depth int) (string, error) {
	if depth > 12 {
		return "", fmt.Errorf("macro expansion too deep")
	}
	var out strings.Builder
	s := strings.TrimSpace(expr)
	for len(s) > 0 {
		switch s[0] {
		case '"':
			j := skipQuoted(s, 0)
			if j > len(s) || s[j-1] != '"' {
				return "", fmt.Errorf("unterminated string literal")
			}
			lit, err := unescape(s[1 : j-1])
			if err != nil {
				return "", err
			}
			out.WriteString(lit)
			s = strings.TrimSpace(s[j:])
		case '(':
			end := matchClose(s, 0)
			if end < 0 {
				return "", fmt.Errorf("unbalanced parenthesis")
			}
			in, err := w.evalStringDepth(f, s[1:end], depth+1)
			if err != nil {
				return "", err
			}
			out.WriteString(in)
			s = strings.TrimSpace(s[end+1:])
		default:
			id := reIdent.FindString(s)
			if id == "" {
				return "", fmt.Errorf("unsupported token %q", clip(s))
			}
			rest := strings.TrimSpace(s[len(id):])
			if (id == "PSTR" || id == "F") && strings.HasPrefix(rest, "(") {
				end := matchClose(rest, 0)
				if end < 0 {
					return "", fmt.Errorf("unbalanced parenthesis")
				}
				in, err := w.evalStringDepth(f, rest[1:end], depth+1)
				if err != nil {
					return "", err
				}
				out.WriteString(in)
				s = strings.TrimSpace(rest[end+1:])
				continue
			}
			vals, ok := w.lookup(f, id)
			if !ok {
				return "", fmt.Errorf("%s is not a macro or constant of the scanned source", id)
			}
			if len(vals) > 1 {
				return "", fmt.Errorf("macro %s has %d different definitions", id, len(vals))
			}
			in, err := w.evalStringDepth(f, vals[0], depth+1)
			if err != nil {
				return "", fmt.Errorf("%s: %w", id, err)
			}
			out.WriteString(in)
			s = rest
		}
	}
	return out.String(), nil
}

func clip(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}

func unescape(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"', '\\', '\'':
			b.WriteByte(s[i])
		case 'x':
			j := i + 1
			for j < len(s) && j < i+3 && strings.ContainsRune("0123456789abcdefABCDEF", rune(s[j])) {
				j++
			}
			v, err := strconv.ParseUint(s[i+1:j], 16, 8)
			if err != nil {
				return "", fmt.Errorf("bad escape in %q", clip(s))
			}
			b.WriteByte(byte(v))
			i = j - 1
		default:
			if s[i] >= '0' && s[i] <= '7' {
				j := i
				for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7' {
					j++
				}
				v, _ := strconv.ParseUint(s[i:j], 8, 8)
				b.WriteByte(byte(v))
				i = j - 1
				continue
			}
			return "", fmt.Errorf("unsupported escape \\%c", s[i])
		}
	}
	return b.String(), nil
}

// matchClose returns the index of the bracket that closes the one at s[i], or -1.
func matchClose(s string, i int) int {
	open := s[i]
	closeCh := map[byte]byte{'(': ')', '[': ']', '{': '}'}[open]
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '"', '\'':
			j = skipQuoted(s, j) - 1
		case open:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// splitTop splits s at top-level commas (outside brackets and strings).
func splitTop(s string) []string {
	var parts []string
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
				parts = append(parts, strings.TrimSpace(s[start:j]))
				start = j + 1
			}
		}
	}
	return append(parts, strings.TrimSpace(s[start:]))
}

var reNumber = regexp.MustCompile(`^(0[xX][0-9a-fA-F]+|\d+)[uUlL]*$`)

// evalInt resolves a plain integer expression: a literal, a macro for one, optionally in
// parentheses. Arithmetic and runtime values are not resolved (ok is false).
func (w *world) evalInt(f *srcFile, expr string, depth int) (int, bool) {
	s := strings.TrimSpace(expr)
	for strings.HasPrefix(s, "(") && matchClose(s, 0) == len(s)-1 {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if m := reNumber.FindStringSubmatch(s); m != nil {
		v, err := strconv.ParseInt(m[1], 0, 32)
		return int(v), err == nil
	}
	if depth > 8 || !regexp.MustCompile(`^[A-Za-z_]\w*$`).MatchString(s) {
		return 0, false
	}
	vals, ok := w.lookup(f, s)
	if !ok || len(vals) != 1 {
		return 0, false
	}
	return w.evalInt(f, vals[0], depth+1)
}
