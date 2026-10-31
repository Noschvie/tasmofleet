// Package extract builds the Tasmota command catalogue from Tasmota source files. It is a static
// reader of C/C++ text, not a compiler: it reads command-name tables, handler tables and the
// reply calls in the handlers, and says "loose" with a reason where that is not enough.
package extract

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/tasmotacmd"
)

var (
	skipDirs  = map[string]bool{"berry": true, "lvgl_berry": true, "html_compressed": true, "html_uncompressed": true, "displaydesc": true}
	sourceExt = map[string]bool{".ino": true, ".h": true, ".c": true, ".cpp": true, ".hpp": true}
)

func isShared(p string) bool {
	return strings.HasPrefix(p, "include/") || strings.HasPrefix(p, "tasmota_support/") ||
		p == "language/en_GB.h" || p == "my_user_config.h"
}

// Extract reads the Tasmota `tasmota/` directory in src and returns the catalogue.
// version and sha256 are recorded as given; the caller verifies the tarball.
func Extract(src fs.FS, version, sha256 string) (*tasmotacmd.Catalogue, error) {
	w := &world{shared: map[string][]string{}}
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !sourceExt[path.Ext(p)] || (strings.HasPrefix(p, "language/") && p != "language/en_GB.h") {
			return nil
		}
		raw, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		f := loadSource(p, string(raw), isShared(p))
		w.files = append(w.files, f)
		if f.shared {
			for k, vs := range f.defs {
				for _, v := range vs {
					addDistinct(w.shared, k, v)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(w.files) == 0 {
		return nil, fmt.Errorf("no Tasmota source files found")
	}
	spans := w.collectConsts()
	a := &analyzer{w: w, funcs: map[string][]fnDef{}, memo: map[string]*reply{}, busy: map[string]bool{}}
	funcTables := map[*srcFile]map[string]*funcTable{}
	synTables := map[*srcFile]map[string]*synTable{}
	for _, f := range w.files {
		for _, d := range scanFuncs(f) {
			a.funcs[d.name] = append(a.funcs[d.name], d)
		}
		funcTables[f] = parseFuncs(f)
		synTables[f] = w.parseSynonyms(f)
	}

	cat := &tasmotacmd.Catalogue{Tasmota: version, SourceSHA256: sha256}
	done := map[string]bool{}
	for _, f := range w.files {
		for _, m := range reDecodeCall.FindAllStringSubmatch(f.text, -1) {
			names, funcsName, syn := unqualified(m[1]), unqualified(m[2]), unqualified(m[3])
			id := f.path + "\x00" + names + "\x00" + funcsName
			if done[id] {
				continue
			}
			done[id] = true
			// the tables normally sit in the file that calls DecodeCommand; a few drivers keep them in a sibling file or header
			nf := f
			span, ok := spans[f][names]
			for _, o := range w.files {
				if ok {
					break
				}
				if sp, found := spans[o][names]; found {
					nf, span, ok = o, sp, true
				}
			}
			if !ok {
				cat.SkippedTables = append(cat.SkippedTables, tasmotacmd.SkippedTable{File: f.path, Table: names, Reason: "name table is not defined in any scanned file"})
				continue
			}
			nt := w.parseNames(nf, names, span)
			if nt.err != nil {
				cat.SkippedTables = append(cat.SkippedTables, tasmotacmd.SkippedTable{File: f.path, Table: names, Reason: nt.err.Error()})
				continue
			}
			ft := funcTables[nf][funcsName]
			for _, o := range w.files {
				if ft != nil {
					break
				}
				ft = funcTables[o][funcsName]
			}
			if ft == nil {
				cat.SkippedTables = append(cat.SkippedTables, tasmotacmd.SkippedTable{File: f.path, Table: names, Reason: "handler table " + funcsName + " is not defined in any scanned file"})
				continue
			}
			var st *synTable
			if syn != "" {
				for _, o := range []*srcFile{f, nf} {
					if st == nil {
						st = synTables[o][syn]
					}
				}
				if st == nil {
					cat.SkippedTables = append(cat.SkippedTables, tasmotacmd.SkippedTable{File: f.path, Table: names, Reason: "synonym table " + syn + " is not defined in the scanned files"})
					continue
				}
			}
			cat.Commands = append(cat.Commands, a.commands(nt, ft, st)...)
		}
	}
	cat.Builds = extractBuilds(src, requiredUseNames(cat.Commands))
	sort.SliceStable(cat.Commands, func(i, j int) bool {
		x, y := cat.Commands[i], cat.Commands[j]
		if lx, ly := strings.ToLower(x.Command), strings.ToLower(y.Command); lx != ly {
			return lx < ly
		}
		if x.File != y.File {
			return x.File < y.File
		}
		return x.Table < y.Table
	})
	sort.SliceStable(cat.SkippedTables, func(i, j int) bool {
		if cat.SkippedTables[i].File != cat.SkippedTables[j].File {
			return cat.SkippedTables[i].File < cat.SkippedTables[j].File
		}
		return cat.SkippedTables[i].Table < cat.SkippedTables[j].Table
	})
	return cat, nil
}

func (a *analyzer) commands(nt *nameTable, ft *funcTable, st *synTable) []tasmotacmd.Command {
	nsyn := 0
	if st != nil {
		nsyn = len(st.indexes)
	}
	pr, perr := pairEntries(nt.entries, nsyn, ft.elems)
	var out []tasmotacmd.Command
	for i, e := range nt.entries {
		c := tasmotacmd.Command{
			Command:  nt.prefix + e.name,
			Prefix:   nt.prefix,
			Table:    nt.name,
			File:     nt.file.path,
			Requires: e.conds,
		}
		var handlers []string
		switch {
		case perr != nil:
			c.Loose = perr.Error()
		default:
			if j, ok := pr.syn[i]; ok {
				handlers = []string{"CmndSetoptionBase"}
				c.SynonymOf = "SetOption" + strconv.Itoa(st.indexes[j])
			}
			for _, k := range pr.elems[i] {
				el := ft.elems[k]
				if el.handler == "" {
					c.Loose = "handler " + el.raw + " is not a plain function pointer"
					continue
				}
				handlers = append(handlers, el.handler)
			}
			if len(handlers) == 0 && c.Loose == "" {
				c.Loose = "the entry has no partner in the handler table in any configuration"
			}
		}
		handlers = dedupe(handlers)
		c.Handler = strings.Join(handlers, ",")
		if len(handlers) > 0 {
			base := map[string]string{}
			if c.SynonymOf != "" {
				base["indexed"] = "0" // DecodeCommand calls CmndSetoptionBase(0) for synonyms
			}
			run := func(extra map[string]string) *reply {
				consts := map[string]string{}
				for k, v := range base {
					consts[k] = v
				}
				for k, v := range extra {
					consts[k] = v
				}
				r := &reply{}
				for _, h := range handlers {
					mergeReply(r, a.analyze(h, nt.file, 0, consts))
				}
				return r
			}
			splits := false
			for _, h := range handlers {
				splits = splits || a.branchesOnUsridx(h, nt.file)
			}
			var r *reply
			if splits {
				r = run(map[string]string{usridx: "0"})
				ri := run(map[string]string{usridx: "1"})
				ik, ikind, iloose := finish(ri, c.Command)
				c.Indexed = &tasmotacmd.IndexedReply{Keys: ik, Kind: ikind, Loose: iloose}
			} else {
				r = run(nil)
			}
			var loose string
			c.Keys, c.Kind, loose = finish(r, c.Command)
			if c.Loose == "" {
				c.Loose = loose
			} else if loose != "" {
				c.Loose += "; " + loose
			}
			if c.Indexed != nil && c.Indexed.Loose == c.Loose && c.Indexed.Kind == c.Kind && strings.Join(c.Indexed.Keys, ",") == strings.Join(c.Keys, ",") {
				c.Indexed = nil
			}
			splitSuffixKeys(&c)
			c.Via = dedupe(r.via)
			c.Index = a.indexOf(handlers[0], nt.file, c.SynonymOf == "")
			c.Value = a.valueBounds(handlers[0], nt.file)
			c.Decimal = a.usesCall(handlers[0], nt.file, "ResponseCmndFloat", "ResponseCmndIdxFloat")
		}
		out = append(out, c)
	}
	return out
}

// usridx is the mailbox flag that tells whether the user typed an index (SetOption19 against SetOption).
const usridx = "XdrvMailbox.usridx"

var reUsridxIf = regexp.MustCompile(`\bif\s*\(\s*!?\s*XdrvMailbox\.usridx\s*\)`)

func (a *analyzer) branchesOnUsridx(handler string, prefer *srcFile) bool {
	for _, d := range a.lookup(handler, prefer) {
		if reUsridxIf.MatchString(d.body) {
			return true
		}
	}
	return false
}

// splitSuffixKeys moves keys that carry an appended suffix (Module2) into the indexed reply: the
// bare command is answered under its own name, only the typed index adds the suffix.
func splitSuffixKeys(c *tasmotacmd.Command) {
	strip := func(keys []string) (plain, all []string) {
		for _, k := range keys {
			base := k
			if i := strings.Index(k, phSfx); i >= 0 {
				base = k[:i] + k[i+len(phSfx):]
			} else {
				plain = append(plain, k)
			}
			all = append(all, base)
		}
		return plain, all
	}
	plain, all := strip(c.Keys)
	if c.Indexed != nil {
		_, ik := strip(c.Indexed.Keys)
		c.Indexed.Keys = ik
	}
	if len(plain) == len(all) {
		return
	}
	c.Keys = plain
	if c.Indexed == nil {
		c.Indexed = &tasmotacmd.IndexedReply{Keys: all, Kind: c.Kind, Loose: c.Loose}
		return
	}
	for _, k := range all {
		if !contains(c.Indexed.Keys, k) {
			c.Indexed.Keys = append(c.Indexed.Keys, k)
		}
	}
}

func contains(l []string, s string) bool {
	for _, e := range l {
		if e == s {
			return true
		}
	}
	return false
}

func mergeReply(dst, src *reply) {
	for _, k := range src.keys {
		dst.addKey(k)
	}
	for _, l := range src.loose {
		dst.addLoose(l)
	}
	dst.via = append(dst.via, src.via...)
	dst.started = dst.started || src.started
	dst.cleared = dst.cleared || src.cleared
	dst.publish = dst.publish || src.publish
	dst.appendOnly = dst.appendOnly || src.appendOnly
	dst.topics = append(dst.topics, src.topics...)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// indexOf reports how the handler reads XdrvMailbox.index. Bounds are the first comparison
// against a literal or a name. When the handler itself compares nothing, the functions it calls
// are read (two levels), and a callee that gets XdrvMailbox.index as a parameter is read for
// comparisons of that parameter (SetOption: CmndSetoption, CmndSetoptionBase, SetoptionDecode).
func (a *analyzer) indexOf(handler string, prefer *srcFile, follow bool) *tasmotacmd.Index {
	defs := a.lookup(handler, prefer)
	if len(defs) == 0 {
		return nil
	}
	depth := 0
	if follow {
		depth = 2 // a SetOption synonym is called with a fixed index: its range is not the user's
	}
	ix := a.indexIn(defs[0], "XdrvMailbox.index", depth)
	if ix == nil && !strings.Contains(defs[0].body, "XdrvMailbox.index") {
		return nil
	}
	if ix == nil {
		ix = &tasmotacmd.Index{}
	}
	return ix
}

// indexIn reads the bounds of variable v in d; nil when v is not used or not bounded there and
// not in any callee within depth.
func (a *analyzer) indexIn(d fnDef, v string, depth int) *tasmotacmd.Index {
	if ix := a.boundsOf(d, v); ix != nil {
		return ix
	}
	if depth == 0 {
		return nil
	}
	for _, c := range calls(d.body) {
		callee := a.lookup(c.name, d.file)
		if len(callee) == 0 || callee[0].body == d.body {
			continue
		}
		for i, arg := range c.args {
			if strings.TrimSpace(arg) == v && i < len(callee[0].params) && callee[0].params[i] != "" {
				if ix := a.indexIn(callee[0], callee[0].params[i], depth-1); ix != nil {
					return ix
				}
			}
		}
		if strings.Contains(callee[0].body, "XdrvMailbox.index") && v == "XdrvMailbox.index" {
			if ix := a.indexIn(callee[0], v, depth-1); ix != nil {
				return ix
			}
		}
	}
	return nil
}

func (a *analyzer) boundsOf(d fnDef, v string) *tasmotacmd.Index {
	body, f := d.body, d.file
	q := regexp.QuoteMeta(v)
	lower1 := regexp.MustCompile(q + `\s*(>=|>)\s*([\w.]+)`)
	lower2 := regexp.MustCompile(`([\w.]+)\s*(<=|<)\s*` + q + `\b`)
	upper1 := regexp.MustCompile(q + `\s*(<=|<)\s*([\w.]+)`)
	upper2 := regexp.MustCompile(`([\w.]+)\s*(>=|>)\s*` + q + `\b`)
	bound := func(expr string, delta int) string {
		if n, ok := a.w.evalInt(f, expr, 0); ok {
			return strconv.Itoa(n + delta)
		}
		switch {
		case delta > 0:
			return expr + "+1"
		case delta < 0:
			return expr + "-1"
		}
		return expr
	}
	ix := &tasmotacmd.Index{}
	if m := lower1.FindStringSubmatch(body); m != nil {
		ix.Min = bound(m[2], map[string]int{">": 1, ">=": 0}[m[1]])
	} else if m := lower2.FindStringSubmatch(body); m != nil {
		ix.Min = bound(m[1], map[string]int{"<": 1, "<=": 0}[m[2]])
	}
	if m := upper1.FindStringSubmatch(body); m != nil {
		ix.Max = bound(m[2], map[string]int{"<": -1, "<=": 0}[m[1]])
	} else if m := upper2.FindStringSubmatch(body); m != nil {
		ix.Max = bound(m[1], map[string]int{">": -1, ">=": 0}[m[2]])
	}
	if ix.Min == "" && ix.Max == "" {
		return nil
	}
	return ix
}

// valueBounds reads the one range check Tasmota writes for a numeric payload: both bounds in one
// conjunction, "(XdrvMailbox.payload >= 0) && (XdrvMailbox.payload < 3601)". A lone
// comparison is not a range (it is often a clamp or a mode switch), so nothing is guessed from it.
func (a *analyzer) valueBounds(handler string, prefer *srcFile) *tasmotacmd.Bounds {
	defs := a.lookup(handler, prefer)
	if len(defs) == 0 {
		return nil
	}
	body, f := defs[0].body, defs[0].file
	for _, m := range rePayloadRange.FindAllStringSubmatch(body, -1) {
		lop, lo, hip, hi := m[1], m[2], m[3], m[4]
		l, ok1 := a.w.evalInt(f, lo, 0)
		h, ok2 := a.w.evalInt(f, hi, 0)
		if !ok1 || !ok2 {
			continue
		}
		if lop == ">" {
			l++
		}
		if hip == "<" {
			h--
		}
		if l > h {
			continue
		}
		return &tasmotacmd.Bounds{Min: &l, Max: &h}
	}
	return nil
}

// payload >= A && payload < B, the form Tasmota writes
var rePayloadRange = regexp.MustCompile(`XdrvMailbox\.payload\s*(>=|>)\s*(\w+)\s*\)?\s*&&\s*\(?\s*XdrvMailbox\.payload\s*(<=|<)\s*(\w+)`)

// usesCall reports whether the handler's own body calls one of names.
func (a *analyzer) usesCall(handler string, prefer *srcFile, names ...string) bool {
	defs := a.lookup(handler, prefer)
	if len(defs) == 0 {
		return false
	}
	for _, c := range calls(defs[0].body) {
		for _, n := range names {
			if c.name == n {
				return true
			}
		}
	}
	return false
}
