package sysconfig

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/store"
)

// FoldWindow is the longest gap between two saves of the same actor that still
// folds them into one history entry.
const FoldWindow = 2 * time.Minute

// HistoryEntry is one line of the policy history: a single version, or a run of
// quick saves folded into one. Folding is display only; nothing is stored.
type HistoryEntry struct {
	// Versions of the entry, newest first. More than one when Folded.
	Versions []store.PolicyRuleVersion
	// Changes is the net effect: the version before the entry's oldest version
	// against its newest.
	Changes []RuleChange
	// Summary is the one-line text of Changes.
	Summary string
	Folded  bool
	// Reordered: the rules only changed their order (no other change), which
	// matters because the first matching rule wins.
	Reordered bool
	// Day is local midnight, in the zone given to BuildHistory, of the newest
	// version. Zero for RuleHistory (no zone given).
	Day time.Time
}

// HistoryPage is one page of the history, newest first.
type HistoryPage struct {
	Entries []HistoryEntry
	// NextBefore is the cursor for the older page (History's before argument),
	// 0 when there is none.
	NextBefore int64
}

// buildHistory turns versions (newest first) into entries. prev is the version
// before the last one in the list, nil when there is none; the oldest entry is
// diffed against it. firstPage says versions[0] is the current version (later
// pages have none, so their newest entry may fold). Consecutive versions of one
// actor with every gap at most FoldWindow fold into one entry; a revert, an
// import and the current version never fold.
//
// A version whose rules cannot be decoded gets the entry "Unreadable version";
// the entries above it are diffed against the nearest readable older version
// instead of an empty list, so one bad row cannot make its successor claim
// that every rule was added.
func buildHistory(versions []store.PolicyRuleVersion, prev *store.PolicyRuleVersion, loc *time.Location, firstPage bool) []HistoryEntry {
	if loc == nil {
		loc = time.UTC
	}
	n := len(versions)
	specs := make([][]RuleSpec, n+1)
	ok := make([]bool, n+1)
	for i, v := range versions {
		specs[i], ok[i] = readSpecs(v)
	}
	ok[n] = true // no version before the oldest: an empty base
	if prev != nil {
		specs[n], ok[n] = readSpecs(*prev)
	}
	var out []HistoryEntry
	for i := 0; i < n; {
		j := i + 1
		for j < n && (!firstPage || i > 0) && canFold(versions[j-1], versions[j]) {
			j++
		}
		e := HistoryEntry{Versions: versions[i:j:j], Folded: j-i > 1, Day: dayOf(versions[i].At, loc)}
		base := j // nearest readable version at or below the run
		for base < n && !ok[base] {
			base++
		}
		switch {
		case !ok[i]:
			e.Summary = "Unreadable version"
		case !ok[base]:
			e.Summary = "Changes unknown (earlier version unreadable)"
		default:
			e.Changes = DiffSpecs(specs[base], specs[i]) // before the oldest of the run -> its newest
			if len(e.Changes) == 0 && reordered(specs[base], specs[i]) {
				e.Reordered, e.Summary = true, "Reordered rules"
			} else {
				e.Summary = Summarize(e.Changes)
			}
		}
		out = append(out, e)
		i = j
	}
	return out
}

// readSpecs decodes a stored version; ok is false when it cannot be read.
func readSpecs(v store.PolicyRuleVersion) ([]RuleSpec, bool) {
	s, err := decodeSpecs([]byte(v.Rules))
	return s, err == nil
}

// lenientSpecs decodes a stored version for display; an unreadable one counts
// as empty so one bad row cannot take a page down.
func lenientSpecs(v store.PolicyRuleVersion) []RuleSpec {
	s, err := decodeSpecs([]byte(v.Rules))
	if err != nil {
		return nil
	}
	return s
}

// reordered reports whether b holds the same rules as a in another order. A
// rule is identified by its real id, else (versions stored before 0.19) by its
// name key, like DiffSpecs matches them.
func reordered(a, b []RuleSpec) bool {
	ident := func(specs []RuleSpec) []string {
		out := make([]string, len(specs))
		for i, s := range specs {
			if s.ID != "" && !isLegacyID(s.ID) {
				out[i] = "id:" + s.ID
			} else {
				out[i] = "key:" + ruleKey(s)
			}
		}
		return out
	}
	return !slices.Equal(ident(a), ident(b))
}

func dayOf(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// canFold reports whether newer and the older version below it belong to one
// run of quick saves.
func canFold(newer, older store.PolicyRuleVersion) bool {
	return newer.Actor == older.Actor && !standsAlone(newer) && !standsAlone(older) &&
		newer.At.Sub(older.At) <= FoldWindow
}

func standsAlone(v store.PolicyRuleVersion) bool { return v.RevertOf != 0 || v.Note == importNote }

// Summarize says in one line what changes did. One change names the rule; more
// are counted ("2 rules changed, 1 removed").
func Summarize(changes []RuleChange) string {
	switch len(changes) {
	case 0:
		return "No rule changes"
	case 1:
		c := changes[0]
		switch c.Kind {
		case "added":
			return "Added " + c.Name
		case "removed":
			return "Removed " + c.Name
		}
		if len(c.Fields) == 0 {
			return "Changed " + c.Name
		}
		return "Changed " + c.Name + " · " + strings.Join(c.Fields, ", ")
	}
	var added, removed, changed int
	for _, c := range changes {
		switch c.Kind {
		case "added":
			added++
		case "removed":
			removed++
		default:
			changed++
		}
	}
	var parts []string
	if added == len(changes) {
		return strconv.Itoa(added) + " rules added"
	}
	if removed == len(changes) {
		return strconv.Itoa(removed) + " rules removed"
	}
	if changed > 0 {
		word := " rules changed"
		if changed == 1 {
			word = " rule changed"
		}
		parts = append(parts, strconv.Itoa(changed)+word)
	}
	if added > 0 {
		parts = append(parts, strconv.Itoa(added)+" added")
	}
	if removed > 0 {
		parts = append(parts, strconv.Itoa(removed)+" removed")
	}
	return strings.Join(parts, ", ")
}

// RuleHistory lists the versions that touched one rule, newest first, one entry
// per version (a rule's page does not fold), each with only that rule's
// changes. versions is newest first; the oldest of them is diffed against
// nothing, so pass the whole history. A rule is followed by its id; a position
// id of a version stored before 0.19 names a slot, so those are matched by
// name (ruleName: the rule's name, or its command when unnamed).
func RuleHistory(versions []store.PolicyRuleVersion, ruleID, ruleName string) []HistoryEntry {
	var out []HistoryEntry
	for i, v := range versions {
		var prev []RuleSpec
		if i+1 < len(versions) {
			prev = lenientSpecs(versions[i+1])
		}
		var mine []RuleChange
		for _, c := range DiffSpecs(prev, lenientSpecs(v)) {
			if touches(c, ruleID, ruleName) {
				mine = append(mine, c)
			}
		}
		if len(mine) == 0 {
			continue
		}
		out = append(out, HistoryEntry{Versions: versions[i : i+1 : i+1], Changes: mine, Summary: Summarize(mine)})
	}
	return out
}

func touches(c RuleChange, ruleID, ruleName string) bool {
	if c.RuleID != "" && !isLegacyID(c.RuleID) {
		return c.RuleID == ruleID
	}
	return c.Name == ruleName
}

// DefaultHistoryLimit is the page size History uses for a limit below 1.
const DefaultHistoryLimit = 50

// History returns one page of the policy history: up to limit versions older
// than the version before (0: from the newest), folded, newest first.
func (r *Rules) History(ctx context.Context, before int64, limit int) (HistoryPage, error) {
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	vs, err := r.o.Store.ListPolicyRuleVersionsBefore(ctx, before, limit+1)
	if err != nil {
		return HistoryPage{}, err
	}
	var page HistoryPage
	var prev *store.PolicyRuleVersion
	if len(vs) > limit { // the extra one is the diff base of the oldest, and proves an older page exists
		prev = &vs[limit]
		vs = vs[:limit]
		page.NextBefore = vs[limit-1].ID
	}
	page.Entries = buildHistory(vs, prev, time.Local, before == 0)
	return page, nil
}
