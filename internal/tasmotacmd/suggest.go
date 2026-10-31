package tasmotacmd

import (
	"sort"
	"strings"
)

// Suggestion is one command offered while the person types.
type Suggestion struct {
	// Name is the command as the table spells it.
	Name string
	// Indexed commands take a number after the name, Lo to Hi (SetOption0 to SetOption177).
	Indexed bool
	Lo, Hi  int
	// Needs names the define a build must have for the command, empty when it is in every build.
	Needs string
	// Desc is our own one-line description, empty when we have none.
	Desc string
	// Starts is true when the name starts with the query, false when it only contains it.
	Starts bool
}

// Suggestions is the answer to one query.
type Suggestions struct {
	List []Suggestion
	// Total is the number of distinct commands in the catalogues consulted.
	Total int
	// Versions are the Tasmota versions consulted.
	Versions []string
}

// Suggest lists commands whose name starts with or contains query, from the catalogues of the
// given firmware versions (the newest pinned one when none is given). keep, when not nil,
// leaves out commands the caller refuses anyway. At most limit are returned; a command that
// starts with the query comes before one that contains it, and one we describe before one we do not.
func (s *Set) Suggest(query string, versions []string, limit int, keep func(name string) bool) Suggestions {
	var cats []*Catalogue
	seenCat := map[*Catalogue]bool{}
	for _, v := range versions {
		if c, _, ok := s.For(v); ok && !seenCat[c] {
			seenCat[c] = true
			cats = append(cats, c)
		}
	}
	if len(cats) == 0 {
		cats = []*Catalogue{s.Newest()}
	}
	sort.Slice(cats, func(i, j int) bool { return cmpOf(cats[i], cats[j]) < 0 })
	out := Suggestions{}
	for _, c := range cats {
		out.Versions = append(out.Versions, c.Tasmota)
	}
	// newest catalogue first, so a command takes the entry of the newest version that has it
	best := map[string]*Command{}
	var order []string
	for i := len(cats) - 1; i >= 0; i-- {
		for j := range cats[i].Commands {
			cmd := &cats[i].Commands[j]
			k := strings.ToLower(cmd.Command)
			if _, ok := best[k]; !ok {
				best[k] = cmd
				order = append(order, k)
			}
		}
	}
	out.Total = len(order)
	q := strings.ToLower(strings.TrimSpace(query))
	q = strings.TrimRight(q, "0123456789") // the index is typed in its own field
	var starts, contains []Suggestion
	for _, k := range order {
		if q == "" || !strings.Contains(k, q) {
			continue
		}
		cmd := best[k]
		if keep != nil && !keep(cmd.Command) {
			continue
		}
		sg := Suggestion{Name: cmd.Command, Desc: Describe(cmd.Command), Starts: strings.HasPrefix(k, q), Needs: neededDefine(cmd)}
		sg.Lo, sg.Hi, sg.Indexed = cmd.IndexSpan()
		if sg.Starts {
			starts = append(starts, sg)
		} else {
			contains = append(contains, sg)
		}
	}
	rank := func(l []Suggestion) {
		sort.SliceStable(l, func(i, j int) bool {
			if (l[i].Desc != "") != (l[j].Desc != "") {
				return l[i].Desc != ""
			}
			return strings.ToLower(l[i].Name) < strings.ToLower(l[j].Name)
		})
	}
	rank(starts)
	rank(contains)
	out.List = append(starts, contains...)
	if limit > 0 && len(out.List) > limit {
		out.List = out.List[:limit]
	}
	return out
}

func cmpOf(a, b *Catalogue) int {
	va, _ := ParseVersion(a.Tasmota)
	vb, _ := ParseVersion(b.Tasmota)
	return cmpVer(va, vb)
}

// neededDefine is the first USE_ define the command's table entry depends on.
func neededDefine(c *Command) string {
	for _, r := range c.Requires {
		for _, n := range CondNames(r) {
			if strings.HasPrefix(n, "USE_") {
				return n
			}
		}
	}
	return ""
}

// ValueKind is the kind of input that suits a command's value.
type ValueKind string

// Value kinds. Unknown kinds stay free text.
const (
	ValueText   ValueKind = "text"
	ValueNumber ValueKind = "number"
	ValueState  ValueKind = "state"
)

// ValueSpec is what the catalogue says about the value of a command.
type ValueSpec struct {
	Kind    ValueKind
	Decimal bool
	// Min and Max are the range the handler accepts; nil when it checks none.
	Min, Max *int
}

// Value reads the value kind of a found command. Only a kind every entry agrees on is used; a
// command that is "mixed", an object or not derivable stays free text.
func (f Found) Value() ValueSpec {
	spec := ValueSpec{Kind: ValueText}
	kind := ""
	for i, e := range f.Entries {
		k := e.Kind
		if f.HasIndex && e.Indexed != nil && e.Indexed.Kind != "" {
			k = e.Indexed.Kind
		}
		if i > 0 && k != kind {
			return spec
		}
		kind = k
	}
	switch kind {
	case "number":
		spec.Kind = ValueNumber
		spec.Decimal = f.Entries[0].Decimal
		if v := f.Entries[0].Value; v != nil && v.Min != nil && v.Max != nil {
			spec.Min, spec.Max = v.Min, v.Max
		}
	case "state":
		spec.Kind = ValueState
	}
	return spec
}
