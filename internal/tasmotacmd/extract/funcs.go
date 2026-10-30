package extract

import (
	"regexp"
	"strings"
)

type fnDef struct {
	name   string
	params []string // parameter names, in order
	body   string
	file   *srcFile
}

var (
	reFnName    = regexp.MustCompile(`([\w:~]+)\s*\(`)
	reNotFunc   = regexp.MustCompile(`^(?:if|for|while|switch|else|return|case|do|typedef|struct|class|enum|union|namespace|extern|using|template|static_assert|sizeof)\b`)
	reFuncStart = regexp.MustCompile(`^[A-Za-z_]`)
)

// scanFuncs finds the function definitions of a file. Tasmota writes them at column 0, so a
// definition is a line starting in column 0 whose header (up to the first '{' or ';') has a
// parameter list and no '='. Each definition is found independently of the others, so a
// brace that only balances under one #if cannot hide the functions behind it; a body also ends
// where the next definition starts.
func scanFuncs(f *srcFile) []fnDef {
	var defs []fnDef
	type head struct {
		name      string
		params    []string
		off       int
		bodyStart int
	}
	var heads []head
	s := f.text
	for l := 0; l < len(f.lineAt); l++ {
		off := f.lineAt[l]
		if off >= len(s) || !reFuncStart.MatchString(s[off:min(off+1, len(s))]) {
			continue
		}
		if reNotFunc.MatchString(s[off:min(off+16, len(s))]) {
			continue
		}
		end := off
		for end < len(s) && s[end] != '{' && s[end] != ';' && end-off < 600 {
			if s[end] == '"' || s[end] == '\'' {
				end = skipQuoted(s, end)
				continue
			}
			end++
		}
		if end >= len(s) || s[end] != '{' {
			continue
		}
		header := strings.TrimSpace(s[off:end])
		if strings.Contains(header, "=") || !strings.Contains(header, "(") || !isFuncHeader(header) {
			continue
		}
		m := reFnName.FindStringSubmatch(header)
		if m == nil || reNotFunc.MatchString(m[1]) {
			continue
		}
		heads = append(heads, head{unqualified(m[1]), paramNames(header), off, end})
	}
	for i, h := range heads {
		stop := len(s)
		if i+1 < len(heads) {
			stop = heads[i+1].off
		}
		if stop <= h.bodyStart {
			continue // a header that runs into the next definition is not a definition
		}
		end := matchClose(s, h.bodyStart)
		if end < 0 || end > stop {
			end = stop
		}
		defs = append(defs, fnDef{name: h.name, params: h.params, body: s[h.bodyStart+1 : end], file: f})
	}
	return defs
}

func isFuncHeader(h string) bool {
	h = strings.TrimSpace(h)
	for _, suf := range []string{"const", "noexcept", "override", "PROGMEM", "IRAM_ATTR"} {
		h = strings.TrimSpace(strings.TrimSuffix(h, suf))
	}
	return strings.HasSuffix(h, ")")
}

var reLastIdent = regexp.MustCompile(`(\w+)\s*(?:\[\s*\w*\s*\])?\s*(?:=.*)?$`)

// paramNames returns the parameter names of a function header.
func paramNames(header string) []string {
	open := strings.Index(header, "(")
	if open < 0 {
		return nil
	}
	end := matchClose(header, open)
	if end < 0 {
		return nil
	}
	var out []string
	for _, p := range splitTop(header[open+1 : end]) {
		if p == "" || p == "void" {
			continue
		}
		if m := reLastIdent.FindStringSubmatch(p); m != nil {
			out = append(out, m[1])
		} else {
			out = append(out, "")
		}
	}
	return out
}

// pruneBranches resolves `if (param) {A} else {B}` and `if (!param) ...` for parameters that
// the caller passed as literals, keeping only the branch that runs. Other uses stay as they are.
func pruneBranches(body string, consts map[string]string) string {
	for param, val := range consts {
		re := regexp.MustCompile(`\bif\s*\(\s*(!?)\s*` + regexp.QuoteMeta(param) + `\s*\)\s*\{`)
		truth := val == "1" || val == "true"
		for {
			m := re.FindStringSubmatchIndex(body)
			if m == nil {
				break
			}
			open := m[1] - 1
			end := matchClose(body, open)
			if end < 0 {
				break
			}
			thenPart := body[open+1 : end]
			elsePart, stop := "", end+1
			if em := regexp.MustCompile(`^\s*else\s*\{`).FindStringIndex(body[end+1:]); em != nil {
				eOpen := end + 1 + em[1] - 1
				if eEnd := matchClose(body, eOpen); eEnd >= 0 {
					elsePart, stop = body[eOpen+1:eEnd], eEnd+1
				}
			}
			keep := elsePart
			if truth != (body[m[2]:m[3]] == "!") {
				keep = thenPart
			}
			body = body[:m[0]] + keep + body[stop:]
		}
	}
	return body
}
