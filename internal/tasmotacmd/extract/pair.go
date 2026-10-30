package extract

import (
	"fmt"
	"sort"
	"strings"
)

// A command-name table and its handler table are two parallel lists. Under #if/#else the
// lists contain the alternatives of every branch, so positions only line up inside one
// configuration. pairEntries tries every combination of the conditions that tell entries
// apart, and pairs entries positionally in each combination where both lists have the same
// length. A name that is paired with different handlers in different configurations keeps
// all of them.
type pairing struct {
	elems map[int][]int // name index -> handler table indexes
	syn   map[int]int   // name index -> position in the synonym table
}

type atomCond struct {
	atom string
	neg  bool
}

func parseCond(c string) atomCond {
	switch {
	case strings.HasPrefix(c, "!(") && strings.HasSuffix(c, ")"):
		return atomCond{c[2 : len(c)-1], true}
	case strings.HasPrefix(c, "!"):
		return atomCond{c[1:], true}
	}
	return atomCond{c, false}
}

const maxAtoms = 12

func pairEntries(names []nameEntry, nsyn int, elems []funcElem) (*pairing, error) {
	nCond := func(i int) []string { return names[i].conds }
	eCond := func(i int) []string { return elems[i].conds }
	total := len(names) + len(elems)

	count := map[atomCond]int{}
	for i := range names {
		for _, c := range nCond(i) {
			count[parseCond(c)]++
		}
	}
	for i := range elems {
		for _, c := range eCond(i) {
			count[parseCond(c)]++
		}
	}
	// an atom every entry carries with the same polarity does not tell entries apart
	var atoms []string
	seen := map[string]bool{}
	for ac, n := range count {
		if n == total {
			continue
		}
		if !seen[ac.atom] {
			seen[ac.atom] = true
			atoms = append(atoms, ac.atom)
		}
	}
	sort.Strings(atoms)
	if len(atoms) > maxAtoms {
		return nil, fmt.Errorf("the tables depend on %d conditions: too many configurations to pair", len(atoms))
	}
	idx := map[string]int{}
	for i, a := range atoms {
		idx[a] = i
	}
	active := func(conds []string, mask int) bool {
		for _, c := range conds {
			ac := parseCond(c)
			i, ok := idx[ac.atom]
			if !ok {
				continue
			}
			if (mask>>i&1 == 1) == ac.neg {
				return false
			}
		}
		return true
	}
	p := &pairing{elems: map[int][]int{}, syn: map[int]int{}}
	valid := 0
	var lastN, lastE int
	for mask := 0; mask < 1<<len(atoms); mask++ {
		var ns, es []int
		for i := range names {
			if active(nCond(i), mask) {
				ns = append(ns, i)
			}
		}
		for i := range elems {
			if active(eCond(i), mask) {
				es = append(es, i)
			}
		}
		lastN, lastE = len(ns), len(es)
		if len(ns)-nsyn != len(es) || len(ns) < nsyn {
			continue
		}
		valid++
		for j, ni := range ns {
			if j < nsyn {
				p.syn[ni] = j
				continue
			}
			p.elems[ni] = appendUnique(p.elems[ni], es[j-nsyn])
		}
	}
	if valid == 0 {
		return nil, fmt.Errorf("name table (%d entries, %d synonyms) and handler table (%d entries) never have the same length, e.g. %d against %d in the last configuration tried", len(names), nsyn, len(elems), lastN-nsyn, lastE)
	}
	return p, nil
}

func appendUnique(s []int, v int) []int {
	for _, e := range s {
		if e == v {
			return s
		}
	}
	return append(s, v)
}
