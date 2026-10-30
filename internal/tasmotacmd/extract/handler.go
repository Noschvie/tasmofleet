package extract

import (
	"regexp"
	"sort"
	"strings"
)

// Placeholders inside a reconstructed reply text.
const (
	phCmd    = "\x00" // XdrvMailbox.command: the command name as the table spells it
	phDynStr = "\x01" // a string only known at run time
	phChar   = "\x02"
	phNum    = "\x03"
	phIdx    = "\x04" // XdrvMailbox.index
)

type replyKey struct{ tmpl, kind string }

// reply is what the static reading of one handler (and the functions it calls) found.
type reply struct {
	keys       []replyKey
	loose      []string // reasons the keys are incomplete or unknown
	via        []string
	started    bool // some reply-building call is reachable
	cleared    bool // some path ends in ResponseClear without building a new reply
	publish    bool // MqttPublish* is called directly
	indexUse   bool
	appendOnly bool     // ResponseAppend without a reply start in the same function
	topics     []string // literal sub-topics the handler publishes on besides RESULT (Status1, ...)
}

func (r *reply) addKey(k replyKey) {
	for i, e := range r.keys {
		if e.tmpl == k.tmpl {
			if e.kind != k.kind {
				r.keys[i].kind = "mixed"
			}
			return
		}
	}
	r.keys = append(r.keys, k)
}

func (r *reply) addLoose(s string) {
	for _, e := range r.loose {
		if e == s {
			return
		}
	}
	r.loose = append(r.loose, s)
}

var reCall = regexp.MustCompile(`([A-Za-z_]\w*)\s*\(`)

type call struct {
	name     string
	args     []string
	pos, end int // byte offsets of the call in the body
}

// calls lists the function calls of a body in source order. Member calls (x.f(), x->f()) are
// skipped; they never build replies.
func calls(body string) []call {
	var out []call
	for _, m := range reCall.FindAllStringSubmatchIndex(body, -1) {
		if m[0] > 0 {
			p := body[m[0]-1]
			if p == '.' || p == '>' || p == '"' {
				continue
			}
		}
		open := m[1] - 1
		end := matchClose(body, open)
		if end < 0 {
			continue
		}
		out = append(out, call{name: body[m[2]:m[3]], args: splitTop(body[open+1 : end]), pos: m[2], end: end + 1})
	}
	return out
}

type analyzer struct {
	w     *world
	funcs map[string][]fnDef
	memo  map[string]*reply
	busy  map[string]bool
}

func (a *analyzer) lookup(name string, prefer *srcFile) []fnDef {
	defs := a.funcs[name]
	var same []fnDef
	for _, d := range defs {
		if d.file == prefer {
			same = append(same, d)
		}
	}
	if len(same) > 0 {
		return same
	}
	return defs
}

// analyze reads the handler `name` and everything it calls that builds a reply.
func (a *analyzer) analyze(name string, prefer *srcFile, depth int, consts map[string]string) *reply {
	key := name
	if prefer != nil {
		key += "\x00" + prefer.path
	}
	var cs []string
	for k, v := range consts {
		cs = append(cs, k+"="+v)
	}
	sort.Strings(cs)
	key += "\x00" + strings.Join(cs, ",")
	if r, ok := a.memo[key]; ok {
		return r
	}
	r := &reply{}
	defs := a.lookup(name, prefer)
	if len(defs) == 0 || depth > 6 || a.busy[key] {
		return r // unknown or cyclic function: contributes nothing
	}
	a.busy[key] = true
	for _, d := range defs {
		if len(consts) > 0 {
			d.body = pruneBranches(d.body, consts)
		}
		a.body(d, r, depth)
	}
	delete(a.busy, key)
	a.memo[key] = r
	return r
}

var simpleReplies = map[string]struct {
	kind string
	idx  bool
}{
	"ResponseCmndNumber":    {"number", false},
	"ResponseCmndFloat":     {"number", false},
	"ResponseCmndIdxNumber": {"number", true},
	"ResponseCmndIdxFloat":  {"number", true},
	"ResponseCmndChar":      {"text", false},
	"ResponseCmndChar_P":    {"text", false},
	"ResponseCmndStateText": {"state", false},
	"ResponseCmndDone":      {"text", false},
	"ResponseCmndError":     {"text", false},
	"ResponseCmndFailed":    {"text", false},
	"ResponseCmndIdxChar":   {"text", true},
	"ResponseCmndAll":       {"text", true},
}

func (a *analyzer) body(d fnDef, r *reply, depth int) {
	if strings.Contains(d.body, "XdrvMailbox.index") {
		r.indexUse = true
	}
	var chain strings.Builder
	rename := ""          // set when the handler overwrites XdrvMailbox.command (SO19 is answered as SetOption19)
	var suffixes []string // literals the handler appends to XdrvMailbox.command (Module2)
	cmdName := func(t string) string {
		if rename == "" {
			return t
		}
		return strings.ReplaceAll(t, phCmd, rename)
	}
	// spellings lists the command name as the reply may spell it: plain, and with each appended suffix
	spellings := func(t string) []string {
		out := []string{cmdName(t)}
		for _, sfx := range suffixes {
			out = append(out, strings.ReplaceAll(t, phCmd, phCmd+sfx))
		}
		return out
	}
	addKey := func(t, kind string) {
		for _, v := range spellings(t) {
			r.addKey(replyKey{v, kind})
		}
	}
	chainErr := ""
	active := false
	flush := func() {
		if active {
			if chainErr != "" {
				r.addLoose(chainErr)
			} else {
				for _, v := range spellings(chain.String()) {
					a.parseChain(v, r)
				}
			}
		}
		chain.Reset()
		chainErr = ""
		active = false
	}
	all := calls(d.body)
	// builds reports whether a call starts or extends a reply (directly or through a callee).
	builds := func(c call) bool {
		if _, ok := simpleReplies[c.name]; ok {
			return true
		}
		switch c.name {
		case "ResponseCmnd", "Response_P", "ResponseAppend_P", "ResponseAppend":
			return true
		}
		if blockedCallees[c.name] || len(a.funcs[c.name]) == 0 || c.name == d.name {
			return false
		}
		return a.analyze(c.name, d.file, depth+1, a.literalArgs(c)).started
	}
	for ci, c := range all {
		if s, ok := simpleReplies[c.name]; ok {
			flush()
			k := phCmd
			if s.idx {
				k += phIdx
			}
			addKey(k, s.kind)
			r.started = true
			continue
		}
		switch c.name {
		case "snprintf_P", "snprintf", "strlcpy", "strlcpy_P", "strlcat", "strcat", "strcpy", "strcpy_P", "sprintf", "sprintf_P":
			if len(c.args) < 2 || strings.TrimSpace(c.args[0]) != "XdrvMailbox.command" {
				break
			}
			lit := c.args[len(c.args)-1]
			if strings.HasPrefix(c.name, "snprintf") && len(c.args) >= 3 {
				lit = c.args[2]
			} else if len(c.args) >= 2 {
				lit = c.args[1]
			}
			v, err := a.w.evalString(d.file, lit)
			switch {
			case err != nil || strings.Contains(v, "%"):
				r.addLoose("the handler rewrites XdrvMailbox.command with a run-time value (" + c.name + ")")
			case c.name == "strlcat" || c.name == "strcat":
				suffixes = append(suffixes, v)
			default:
				rename = v
			}
		case "ResponseCmnd":
			flush()
			chain.WriteString(`{"` + phCmd + `":`)
			active = true
			r.started = true
		case "Response_P":
			flush()
			active = true
			r.started = true
			chainErr = a.writeFormat(d.file, c.args, &chain)
		case "ResponseAppend_P", "ResponseAppend":
			if active {
				if e := a.writeFormat(d.file, c.args, &chain); chainErr == "" {
					chainErr = e
				}
			} else {
				r.appendOnly = true
			}
		case "ResponseJsonEnd":
			if active {
				chain.WriteString("}")
			}
		case "ResponseJsonEndEnd":
			if active {
				chain.WriteString("}}")
			}
		case "ResponseClear":
			flush()
			// the clear stands when its block ends without building a new reply
			stop := blockEnd(d.body, c.end)
			restarted := false
			for _, n := range all[ci+1:] {
				if n.pos >= stop {
					break
				}
				if builds(n) {
					restarted = true
					break
				}
			}
			if !restarted {
				r.cleared = true
			}
		default:
			if strings.HasPrefix(c.name, "MqttPublish") {
				// the text built so far goes out on a topic of its own, it is not the command's reply
				sub, elsewhere := a.publishesElsewhere(d.file, c)
				if elsewhere {
					r.topics = append(r.topics, sub)
				}
				if active {
					var tmp reply
					if chainErr == "" {
						a.parseChain(chain.String(), &tmp)
					}
					if len(tmp.keys) > 0 && !elsewhere {
						r.addLoose("a reply with the keys " + keyList(tmp.keys) + " is published with " + c.name + " instead of being returned")
					}
					chain.Reset()
					chainErr = ""
					active = false
				}
				r.publish = true
				continue
			}
			if blockedCallees[c.name] || len(a.funcs[c.name]) == 0 || c.name == d.name {
				continue
			}
			sub := a.analyze(c.name, d.file, depth+1, a.literalArgs(c))
			if !sub.started && !sub.cleared {
				if active && sub.appendOnly {
					r.addLoose("keys appended by " + c.name + " are not read")
				}
				continue // the chain goes on after the helper
			}
			flush()
			for _, k := range sub.keys {
				addKey(k.tmpl, k.kind)
			}
			for _, l := range sub.loose {
				r.addLoose(l)
			}
			r.via = append(r.via, c.name)
			r.via = append(r.via, sub.via...)
			r.started = r.started || sub.started
			r.publish = r.publish || sub.publish
			r.topics = append(r.topics, sub.topics...)
			if sub.cleared {
				// a clear inside a callee stands unless this handler builds a reply afterwards
				restarted := false
				for _, n := range all[ci+1:] {
					if builds(n) {
						restarted = true
						break
					}
				}
				r.cleared = r.cleared || !restarted
			}
		}
	}
	flush()
}

// blockEnd returns the offset where the block that contains offset from closes.
func blockEnd(body string, from int) int {
	depth := 0
	for j := from; j < len(body); j++ {
		switch body[j] {
		case '"', '\'':
			j = skipQuoted(body, j) - 1
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				return j
			}
		}
	}
	return len(body)
}

// blockedCallees never answer on behalf of the calling command: they run another command.
var blockedCallees = map[string]bool{
	"ExecuteCommand": true, "ExecuteCommandBlock": true, "ExecuteWebCommand": true,
}

var reConv = regexp.MustCompile(`%[-+ #0]*(\*|\d+)?(?:\.(?:\*|\d+))?(?:hh|h|ll|l|z|j|t)?_?([diuoxXfFeEgGaAcspn%])`)

// writeFormat appends the format of a Response_P-style call with every conversion replaced by a
// placeholder, or by the literal when the argument is a string literal / XdrvMailbox.command.
func (a *analyzer) writeFormat(f *srcFile, args []string, chain *strings.Builder) string {
	if len(args) == 0 {
		return ""
	}
	format, err := a.w.evalString(f, args[0])
	if err != nil {
		chain.WriteString(phDynStr)
		return "reply format is not a literal: " + err.Error()
	}
	next := 1
	arg := func() string {
		if next < len(args) {
			next++
			return args[next-1]
		}
		return ""
	}
	chain.WriteString(reConv.ReplaceAllStringFunc(format, func(m string) string {
		sub := reConv.FindStringSubmatch(m)
		conv := sub[2]
		if conv == "%" {
			return "%"
		}
		for i := 0; i < strings.Count(m, "*"); i++ {
			arg() // width and precision taken from the argument list
		}
		v := strings.TrimSpace(arg())
		switch conv {
		case "s":
			if v == "XdrvMailbox.command" {
				return phCmd
			}
			if lit, err := a.w.evalString(f, v); err == nil && v != "" {
				return lit
			}
			return phDynStr
		case "c":
			return phChar
		default:
			if v == "XdrvMailbox.index" {
				return phIdx
			}
			return phNum
		}
	}))
	return ""
}

// parseChain reads the top-level keys of one reconstructed reply text.
func (a *analyzer) parseChain(text string, r *reply) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "{") {
		r.addLoose("reply does not start with a literal JSON object")
		return
	}
	depth := 0
	found := false
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '"':
			j := strings.IndexByte(t[i+1:], '"')
			if j < 0 {
				r.addLoose("unbalanced quotes in the reply text")
				return
			}
			str := t[i+1 : i+1+j]
			end := i + 1 + j
			rest := strings.TrimLeft(t[end+1:], " ")
			if depth == 1 && strings.HasPrefix(rest, ":") {
				found = true
				switch {
				case strings.ContainsAny(str, phDynStr+phChar) || strings.Contains(str, "%"):
					r.addLoose("a reply key is built at run time")
				case strings.Contains(str, phNum):
					r.addLoose("a reply key ends in a number chosen at run time: " + strings.ReplaceAll(strings.ReplaceAll(str, phNum, "<#>"), phCmd, "<cmd>"))
				default:
					r.addKey(replyKey{str, valueKind(strings.TrimLeft(rest[1:], " "))})
				}
			}
			i = end
		}
	}
	if !found {
		r.addLoose("reply has no literal top-level key")
	}
}

func valueKind(v string) string {
	if v == "" {
		return ""
	}
	switch {
	case v[0] == '"':
		return "text"
	case v[0] == '[':
		return "array"
	case v[0] == '{':
		return "object"
	case strings.HasPrefix(v, phNum), v[0] >= '0' && v[0] <= '9', v[0] == '-':
		return "number"
	}
	return ""
}

// finish turns the findings of one handler into the catalogue fields for the command `cmd`.
func finish(r *reply, cmd string) (keys []string, kind string, loose string) {
	kinds := map[string]bool{}
	for _, k := range r.keys {
		t := strings.ReplaceAll(strings.ReplaceAll(k.tmpl, phCmd, cmd), phIdx, "<n>")
		dup := false
		for _, e := range keys {
			dup = dup || e == t
		}
		if !dup {
			keys = append(keys, t)
		}
		kinds[k.kind] = true
	}
	delete(kinds, "")
	switch len(kinds) {
	case 0:
	case 1:
		for k := range kinds {
			kind = k
		}
	default:
		kind = "mixed"
	}
	reasons := append([]string{}, r.loose...)
	if r.appendOnly && !r.started {
		reasons = append(reasons, "the reply is built with ResponseAppend only: its keys are not read")
	}
	if r.cleared {
		reasons = append(reasons, "some paths clear the reply (ResponseClear): the answer is published by other code or by other commands")
	}
	if len(keys) == 0 && r.publish && len(reasons) == 0 {
		reasons = append(reasons, "the reply goes out with MqttPublish* on a topic of its own, not as a RESULT")
	}
	if len(keys) == 0 && len(reasons) == 0 {
		reasons = append(reasons, "no reply call found in the handler or the functions it calls (it may dispatch through XdrvCall or a function pointer)")
	}
	if len(reasons) > 0 {
		if topics := dedupe(r.topics); len(topics) > 0 {
			if len(topics) > 4 {
				topics = topics[:4]
			}
			reasons = append([]string{"also publishes on topics of its own (stat/<topic>/" + strings.Join(topics, ", ") + ")"}, reasons...)
		}
		loose = strings.Join(reasons, "; ")
	}
	return keys, kind, loose
}

func keyList(keys []replyKey) string {
	var s []string
	for _, k := range keys {
		s = append(s, strings.ReplaceAll(strings.ReplaceAll(k.tmpl, phCmd, "<cmd>"), phIdx, "<n>"))
	}
	return strings.Join(s, ", ")
}

var reLiteralArg = regexp.MustCompile(`^(?:0|1|true|false)$`)

// literalArgs maps the parameters of the called function to the literal 0/1/true/false values
// the call passes, so branches on those parameters can be pruned.
func (a *analyzer) literalArgs(c call) map[string]string {
	defs := a.funcs[c.name]
	if len(defs) != 1 {
		return nil
	}
	var out map[string]string
	for i, arg := range c.args {
		if i < len(defs[0].params) && defs[0].params[i] != "" && reLiteralArg.MatchString(arg) {
			if out == nil {
				out = map[string]string{}
			}
			out[defs[0].params[i]] = arg
		}
	}
	return out
}

var topicOptions = map[string]bool{"STAT": true, "TELE": true, "CMND": true, "RESULT_OR_STAT": true, "RESULT_OR_TELE": true}

// publishesElsewhere reports whether a MqttPublishPrefixTopic*_P call names a literal sub-topic
// other than RESULT, i.e. the text goes to stat/<topic>/<subtopic> and never competes with the reply.
func (a *analyzer) publishesElsewhere(f *srcFile, c call) (string, bool) {
	if !strings.HasPrefix(c.name, "MqttPublishPrefixTopic") || len(c.args) < 2 {
		return "", false
	}
	sub, err := a.w.evalString(f, c.args[1])
	return sub, err == nil && sub != "RESULT" && topicOptions[strings.TrimSpace(c.args[0])]
}
