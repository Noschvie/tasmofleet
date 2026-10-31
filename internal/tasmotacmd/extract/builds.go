package extract

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/tasmotacmd"
)

// Which USE_ defines do the stock builds have? The answer is read from the three configuration
// files the build includes, in that order, with a small preprocessor: #define, #undef, #if/#ifdef/
// #ifndef/#elif/#else/#endif over defined(), !, && and ||. A condition it cannot decide (a
// comparison, a macro call) disables its block entirely, so a define is only listed when the
// source certainly makes it. Defines handed to the compiler by a platformio environment are not
// seen; the editor words its warning accordingly.
var buildFiles = [][]string{
	{"my_user_config.h", "include/my_user_config.h"},
	{"include/tasmota_configurations.h", "tasmota_configurations.h"},
	{"include/tasmota_configurations_ESP32.h", "tasmota_configurations_ESP32.h"},
}

var (
	reBuildDirective = regexp.MustCompile(`^\s*#\s*(\w+)\s*(.*)$`)
	reFirmwareIf     = regexp.MustCompile(`#\s*if(?:def)?\s+(FIRMWARE_\w+)`)
)

// extractBuilds computes the defined subset of wanted (USE_ names that some command requires)
// for every family and variant. It returns nil when the source has no my_user_config.h.
func extractBuilds(src fs.FS, wanted map[string]bool) map[string][]string {
	var texts [][]string // per file: its lines
	macros := map[string]bool{}
	for _, cands := range buildFiles {
		var lines []string
		for _, p := range cands {
			if raw, err := fs.ReadFile(src, p); err == nil {
				txt := stripComments(string(raw))
				txt = strings.ReplaceAll(txt, "\\\n", " ")
				lines = strings.Split(txt, "\n")
				for _, m := range reFirmwareIf.FindAllStringSubmatch(txt, -1) {
					macros[m[1]] = true
				}
				break
			}
		}
		texts = append(texts, lines)
	}
	if len(texts[0]) == 0 {
		return nil
	}
	variants := []string{""}
	for m := range macros {
		variants = append(variants, m)
	}
	sort.Strings(variants)
	out := map[string][]string{}
	for _, fam := range []string{"esp8266", "esp32"} {
		for _, v := range variants {
			defs := map[string]bool{strings.ToUpper(fam): true}
			if v != "" {
				defs[v] = true
			}
			for i, lines := range texts {
				if i == 2 && fam != "esp32" {
					continue // the ESP32 configurations are not compiled for an ESP8266
				}
				preprocess(lines, defs)
			}
			var list []string
			for n := range defs {
				if wanted[n] {
					list = append(list, n)
				}
			}
			sort.Strings(list)
			name := v
			if name == "" {
				name = "tasmota"
			}
			out[fam+"/"+name] = list
		}
	}
	return out
}

type ppFrame struct {
	parent bool // the enclosing block is active
	taken  bool // some branch of this #if was taken, or could not be decided
	active bool
}

func preprocess(lines []string, defs map[string]bool) {
	var stack []*ppFrame
	active := func() bool { return len(stack) == 0 || stack[len(stack)-1].active }
	isDef := func(n string) tasmotacmd.Tri {
		if defs[n] {
			return tasmotacmd.True
		}
		return tasmotacmd.False
	}
	branch := func(fr *ppFrame, c tasmotacmd.Tri) {
		fr.active = fr.parent && c == tasmotacmd.True && !fr.taken
		if c == tasmotacmd.True {
			fr.taken = true
		}
		if c == tasmotacmd.Unknown {
			fr.taken = true // an undecidable branch: nothing after it can be decided either
		}
	}
	for _, ln := range lines {
		m := reBuildDirective.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		name, arg := m[1], strings.TrimSpace(m[2])
		switch name {
		case "if", "ifdef", "ifndef":
			fr := &ppFrame{parent: active()}
			var c tasmotacmd.Tri
			switch name {
			case "if":
				c = tasmotacmd.EvalCond(arg, isDef)
			case "ifdef":
				c = isDef(firstWord(arg))
			default:
				c = isDef(firstWord(arg)).Not()
			}
			branch(fr, c)
			stack = append(stack, fr)
		case "elif":
			if n := len(stack); n > 0 {
				branch(stack[n-1], tasmotacmd.EvalCond(arg, isDef))
			}
		case "else":
			if n := len(stack); n > 0 {
				fr := stack[n-1]
				fr.active = fr.parent && !fr.taken
				fr.taken = true
			}
		case "endif":
			if n := len(stack); n > 0 {
				stack = stack[:n-1]
			}
		case "define":
			if active() {
				defs[firstWord(arg)] = true
			}
		case "undef":
			if active() {
				delete(defs, firstWord(arg))
			}
		}
	}
}

func firstWord(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return s[:i]
		}
	}
	return s
}

// requiredUseNames collects every USE_ name that a command's conditions mention.
func requiredUseNames(cmds []tasmotacmd.Command) map[string]bool {
	out := map[string]bool{}
	for _, c := range cmds {
		for _, r := range c.Requires {
			for _, n := range tasmotacmd.CondNames(r) {
				if strings.HasPrefix(n, "USE_") {
					out[n] = true
				}
			}
		}
	}
	return out
}
