package sysconfig

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/store"
)

var t0 = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func spec(id, name, want string) RuleSpec {
	return RuleSpec{ID: id, Name: name, Command: "SetOption1", Want: want}
}

// ver builds a stored version (newest-first order is up to the caller).
func ver(id int64, actor string, at time.Time, specs ...RuleSpec) store.PolicyRuleVersion {
	b, err := json.Marshal(specs)
	if err != nil {
		panic(err)
	}
	return store.PolicyRuleVersion{ID: id, Rules: string(b), Actor: actor, At: at}
}

func ids(e HistoryEntry) []int64 {
	var out []int64
	for _, v := range e.Versions {
		out = append(out, v.ID)
	}
	return out
}

func eqIDs(a []int64, b ...int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func quickSaves(actor string, gap time.Duration) []store.PolicyRuleVersion {
	// v1 (oldest, base), v2, v3, v4 newest; newest first. v4 is not the current
	// version in the callers that pass a newer one on top.
	a := spec("raaaaaaaaaa", "A", "1")
	return []store.PolicyRuleVersion{
		ver(4, actor, t0.Add(2*gap), a, spec("rbbbbbbbbbb", "B", "4")),
		ver(3, actor, t0.Add(gap), a, spec("rbbbbbbbbbb", "B", "3")),
		ver(2, actor, t0, a, spec("rbbbbbbbbbb", "B", "2")),
	}
}

// current returns a version on top that must stay alone.
func withCurrent(vs []store.PolicyRuleVersion, actor string, at time.Time) []store.PolicyRuleVersion {
	top := ver(vs[0].ID+1, actor, at, spec("rcccccccccc", "C", "9"))
	return append([]store.PolicyRuleVersion{top}, vs...)
}

// Why: a burst of saves while editing one thing is noise; the same actor's
// saves, each at most two minutes after the previous, read as one entry.
func TestHistoryFoldsQuickSavesOfOneActor(t *testing.T) {
	vs := withCurrent(quickSaves("thomas", 90*time.Second), "other", t0.Add(time.Hour))
	got := buildHistory(vs, nil, time.UTC, true)
	if len(got) != 2 || !eqIDs(ids(got[0]), 5) || !eqIDs(ids(got[1]), 4, 3, 2) || !got[1].Folded || got[0].Folded {
		t.Fatalf("entries %+v", got)
	}
}

// Why: the gap is measured between neighbours, so a long chain of 2-minute
// steps is still one editing session; exactly 2:00 folds, 2:01 does not.
func TestHistoryFoldWindowBoundary(t *testing.T) {
	vs := withCurrent(quickSaves("thomas", FoldWindow), "other", t0.Add(time.Hour))
	if got := buildHistory(vs, nil, time.UTC, true); len(got) != 2 || len(got[1].Versions) != 3 {
		t.Fatalf("exactly the window must fold: %+v", got)
	}
	vs = withCurrent(quickSaves("thomas", FoldWindow+time.Second), "other", t0.Add(time.Hour))
	if got := buildHistory(vs, nil, time.UTC, true); len(got) != 4 {
		t.Fatalf("2:01 apart must not fold: %d entries", len(got))
	}
}

// Why: who changed a rule matters in an audit trail; another person's save
// between two of mine must stay visible as its own entry.
func TestHistoryDifferentActorBreaksFold(t *testing.T) {
	vs := quickSaves("thomas", time.Minute)
	vs[1].Actor = "anna"
	vs = withCurrent(vs, "x", t0.Add(time.Hour))
	if got := buildHistory(vs, nil, time.UTC, true); len(got) != 4 {
		t.Fatalf("want 4 entries, got %d: %+v", len(got), got)
	}
}

// Why: a revert or an import is a decision on its own and must be findable;
// folding it into neighbouring saves would hide it.
func TestHistoryRevertAndImportNeverFold(t *testing.T) {
	for name, mark := range map[string]func(*store.PolicyRuleVersion){
		"revert": func(v *store.PolicyRuleVersion) { v.RevertOf = 2 },
		"import": func(v *store.PolicyRuleVersion) { v.Note = importNote },
	} {
		t.Run(name, func(t *testing.T) {
			vs := quickSaves("thomas", time.Minute)
			mark(&vs[1]) // the middle one
			vs = withCurrent(vs, "x", t0.Add(time.Hour))
			if got := buildHistory(vs, nil, time.UTC, true); len(got) != 4 {
				t.Fatalf("want 4 entries, got %d", len(got))
			}
		})
	}
}

// Why: the current version is what is live now; it must never disappear into a
// fold, even when it is a quick save of the same actor.
func TestHistoryCurrentVersionAlwaysAlone(t *testing.T) {
	vs := quickSaves("thomas", time.Minute)
	got := buildHistory(vs, nil, time.UTC, true)
	if len(got) != 2 || !eqIDs(ids(got[0]), 4) || !eqIDs(ids(got[1]), 3, 2) {
		t.Fatalf("entries %+v", got)
	}
	// A later page has no current version: its newest entry may fold.
	if got := buildHistory(vs, nil, time.UTC, false); len(got) != 1 || len(got[0].Versions) != 3 {
		t.Fatalf("later page: %+v", got)
	}
}

// Why: a fold's summary is the net effect (before the first save -> after the
// last), not the sum of steps: B edited twice is still one changed rule.
func TestHistoryFoldSummaryIsNet(t *testing.T) {
	a, b, c := spec("ra00000000", "A", "1"), spec("rb00000000", "B", "1"), spec("rc00000000", "C", "1")
	a2, b2, b3 := a, b, b
	a2.Want, b2.Want, b3.Want = "2", "2", "3"
	prev := ver(1, "x", t0.Add(-time.Hour), a, b, c)
	vs := []store.PolicyRuleVersion{
		ver(4, "t", t0.Add(2*time.Minute), a2, b3), // C removed
		ver(3, "t", t0.Add(time.Minute), a2, b2, c),
		ver(2, "t", t0, a, b2, c),
	}
	got := buildHistory(vs, &prev, time.UTC, false)
	if len(got) != 1 || got[0].Summary != "2 rules changed, 1 removed" {
		t.Fatalf("%q (%d entries)", got[0].Summary, len(got))
	}
	if len(got[0].Changes) != 3 {
		t.Fatalf("net changes %+v", got[0].Changes)
	}
}

// Why: the summary is the reason the list is readable; each kind has fixed text.
func TestHistorySummaryTexts(t *testing.T) {
	a := RuleSpec{ID: "ra00000000", Name: "Local timezone", Command: "Timezone", Want: "99"}
	b := RuleSpec{ID: "rb00000000", Command: "OtaUrl", Families: []string{"ESP8266"}, Want: "u1"}
	cases := []struct {
		name       string
		old, new   []RuleSpec
		wantPrefix string
	}{
		{"added", nil, []RuleSpec{a}, "Added Local timezone"},
		{"removed", []RuleSpec{a}, nil, "Removed Local timezone"},
		{"changed expected", []RuleSpec{b}, []RuleSpec{{ID: b.ID, Command: "OtaUrl", Families: b.Families, Want: "u2"}}, "Changed OtaUrl · expected value"},
		{"changed two fields", []RuleSpec{b}, []RuleSpec{{ID: b.ID, Command: "OtaUrl", Families: []string{"ESP32"}, Want: "u2"}}, "Changed OtaUrl · expected value, scope"},
		{"many", nil, []RuleSpec{a, b, spec("rc00000000", "C", "1")}, "3 rules added"},
		{"none", []RuleSpec{a}, []RuleSpec{a}, "No rule changes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prev := ver(1, "x", t0.Add(-time.Hour), c.old...)
			got := buildHistory([]store.PolicyRuleVersion{ver(2, "x", t0, c.new...)}, &prev, time.UTC, false)
			if got[0].Summary != c.wantPrefix {
				t.Fatalf("got %q want %q", got[0].Summary, c.wantPrefix)
			}
		})
	}
}

// Why: with a page cut at N versions, the oldest of the page must still say
// what it changed; History fetches one extra version to diff against.
func TestRulesHistoryOldestOfPageDiffsAgainstTheVersionBefore(t *testing.T) {
	r, st, _ := newRules("", nil)
	a := spec("ra00000000", "A", "1")
	for i, w := range []string{"1", "2", "3", "4"} {
		a.Want = w
		b, _ := json.Marshal([]RuleSpec{a})
		st.vs = append(st.vs, store.PolicyRuleVersion{ID: int64(i + 1), Rules: string(b), Actor: "a" + w, At: t0.Add(time.Duration(i) * time.Hour)})
	}
	p, err := r.History(context.Background(), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 2 || p.NextBefore != 3 {
		t.Fatalf("page %+v", p)
	}
	if s := p.Entries[1].Summary; !strings.HasPrefix(s, "Changed A") {
		t.Fatalf("oldest of the page must diff against v2, got %q", s)
	}
	p2, err := r.History(context.Background(), p.NextBefore, 2)
	if err != nil || len(p2.Entries) != 2 || p2.NextBefore != 0 {
		t.Fatalf("second page %+v %v", p2, err)
	}
	if s := p2.Entries[1].Summary; s != "Added A" {
		t.Fatalf("the very first version adds everything, got %q", s)
	}
}

// Why: versions stored before 0.19 carry no ids; their summaries must still
// name the rule (matched by name) and not report every rule as new.
func TestHistoryLegacyVersionsWithoutIDsSummariseByName(t *testing.T) {
	old := store.PolicyRuleVersion{ID: 1, At: t0.Add(-time.Hour), Rules: `[{"name":"Tz","command":"Timezone","want":"99"},{"name":"Led","command":"LedPower","want":"1"}]`}
	cur := store.PolicyRuleVersion{ID: 2, At: t0, Rules: `[{"name":"Tz","command":"Timezone","want":"1"},{"name":"Led","command":"LedPower","want":"1"}]`}
	got := buildHistory([]store.PolicyRuleVersion{cur}, &old, time.UTC, true)
	if got[0].Summary != "Changed Tz · expected value" {
		t.Fatalf("%q", got[0].Summary)
	}
}

// Why: a rule's own history must survive a rename; the name changes, the id
// does not, so the page of the rule keeps showing the versions before it.
func TestRuleHistoryFollowsRenamedRuleByID(t *testing.T) {
	a := spec("rabcdefghij", "Old name", "1")
	a2 := a
	a2.Name = "New name"
	a3 := a2
	a3.Want = "3"
	other := spec("rzzzzzzzzzz", "Other", "1")
	other2 := other
	other2.Want = "2"
	vs := []store.PolicyRuleVersion{
		ver(4, "t", t0.Add(3*time.Hour), a3, other2),
		ver(3, "t", t0.Add(2*time.Hour), a2, other2), // rename only
		ver(2, "t", t0.Add(time.Hour), a, other2),    // touches Other only
		ver(1, "t", t0, a, other),
	}
	got := RuleHistory(vs, "rabcdefghij", "New name")
	if len(got) != 3 || !eqIDs(ids(got[0]), 4) || !eqIDs(ids(got[1]), 3) || !eqIDs(ids(got[2]), 1) {
		var all [][]int64
		for _, e := range got {
			all = append(all, ids(e))
		}
		t.Fatalf("entries %v", all)
	}
	if got[1].Changes[0].Fields[0] != "name" {
		t.Fatalf("rename must be a name change: %+v", got[1].Changes)
	}
	if len(got[0].Changes) != 1 || got[0].Changes[0].RuleID != "rabcdefghij" {
		t.Fatalf("only this rule's changes: %+v", got[0].Changes)
	}
}

// Why: legacy position ids name a slot, not a rule; a reorder in an old
// version must not attribute another rule's change to this one.
func TestRuleHistoryLegacyRulesMatchByName(t *testing.T) {
	v1 := store.PolicyRuleVersion{ID: 1, At: t0, Rules: `[{"name":"Tz","command":"Timezone","want":"1"},{"name":"Led","command":"LedPower","want":"1"}]`}
	v2 := store.PolicyRuleVersion{ID: 2, At: t0.Add(time.Hour), Rules: `[{"name":"Led","command":"LedPower","want":"0"},{"name":"Tz","command":"Timezone","want":"1"}]`}
	got := RuleHistory([]store.PolicyRuleVersion{v2, v1}, "r1", "Tz")
	if len(got) != 1 || got[0].Versions[0].ID != 1 {
		t.Fatalf("only the creation of Tz touches it: %+v", got)
	}
}

// Why: days are shown in the server's zone; a save at 23:59 and one at 00:01
// local time are different days although they are minutes apart in UTC.
func TestHistoryDayGroupingAtLocalMidnight(t *testing.T) {
	loc := time.FixedZone("CEST", 2*3600)
	late := time.Date(2026, 10, 28, 21, 59, 0, 0, time.UTC) // 23:59 local
	early := late.Add(2 * time.Minute)                      // 00:01 local next day
	vs := []store.PolicyRuleVersion{
		ver(3, "x", early.Add(time.Hour), spec("ra00000000", "A", "3")),
		ver(2, "y", early, spec("ra00000000", "A", "2")),
		ver(1, "z", late, spec("ra00000000", "A", "1")),
	}
	got := buildHistory(vs, nil, loc, true)
	if !got[1].Day.Equal(time.Date(2026, 10, 29, 0, 0, 0, 0, loc)) || !got[2].Day.Equal(time.Date(2026, 10, 28, 0, 0, 0, 0, loc)) {
		t.Fatalf("days %v %v", got[1].Day, got[2].Day)
	}
	if got[1].Day.Equal(got[2].Day) {
		t.Fatal("must be different days")
	}
}

// Why: rule order decides which rule wins, so a save that only reorders must
// not read as "no changes"; ids (or names for legacy versions) tell reorder
// from replace.
func TestHistoryReorderOnlySaveSaysReordered(t *testing.T) {
	a, b := spec("ra00000000", "A", "1"), spec("rb00000000", "B", "1")
	prev := ver(1, "x", t0.Add(-time.Hour), a, b)
	got := buildHistory([]store.PolicyRuleVersion{ver(2, "x", t0, b, a)}, &prev, time.UTC, true)
	if got[0].Summary != "Reordered rules" || !got[0].Reordered || len(got[0].Changes) != 0 {
		t.Fatalf("%+v", got[0])
	}
	// legacy versions without ids: matched by name
	old := store.PolicyRuleVersion{ID: 1, At: t0.Add(-time.Hour), Rules: `[{"name":"Tz","command":"Timezone","want":"1"},{"name":"Led","command":"LedPower","want":"1"}]`}
	cur := store.PolicyRuleVersion{ID: 2, At: t0, Rules: `[{"name":"Led","command":"LedPower","want":"1"},{"name":"Tz","command":"Timezone","want":"1"}]`}
	if got := buildHistory([]store.PolicyRuleVersion{cur}, &old, time.UTC, true); got[0].Summary != "Reordered rules" {
		t.Fatalf("legacy: %q", got[0].Summary)
	}
	// an identical save stays "No rule changes"
	same := ver(2, "x", t0, a, b)
	if got := buildHistory([]store.PolicyRuleVersion{same}, &prev, time.UTC, true); got[0].Summary != "No rule changes" || got[0].Reordered {
		t.Fatalf("same: %+v", got[0])
	}
}

// Why: a fold that nets to a pure reorder says so; the intermediate saves do
// not matter, only before-first against after-last.
func TestHistoryFoldNettingToReorderSaysReordered(t *testing.T) {
	a, b := spec("ra00000000", "A", "1"), spec("rb00000000", "B", "1")
	prev := ver(1, "x", t0.Add(-time.Hour), a, b)
	vs := []store.PolicyRuleVersion{
		ver(3, "t", t0.Add(time.Minute), b, a),
		ver(2, "t", t0, b, a),
	}
	got := buildHistory(vs, &prev, time.UTC, false)
	if len(got) != 1 || !got[0].Folded || got[0].Summary != "Reordered rules" {
		t.Fatalf("%+v", got)
	}
}

// Why: the bare name is what people know a rule by; the history must not show
// the "Name (Command)" label the diff view uses.
func TestChangeNameIsNameOrCommand(t *testing.T) {
	named := RuleSpec{ID: "ra00000000", Name: "Local timezone", Command: "Timezone", Want: "1"}
	unnamed := RuleSpec{ID: "rb00000000", Command: "OtaUrl", Want: "u"}
	for _, c := range DiffSpecs(nil, []RuleSpec{named, unnamed}) {
		want := map[string]string{"Local timezone (Timezone)": "Local timezone", "OtaUrl": "OtaUrl"}[c.Rule]
		if c.Name != want {
			t.Fatalf("%+v want name %q", c, want)
		}
	}
}

// Why: "1 rules changed" reads as a bug; counts of one are singular, and the
// mixed case keeps the kinds apart.
func TestSummarizeSingularAndPlural(t *testing.T) {
	ch := func(kind string) RuleChange { return RuleChange{Kind: kind, Name: kind} }
	cases := map[string][]RuleChange{
		"1 rule changed, 1 removed": {ch("changed"), ch("removed")},
		"2 rules changed, 1 added":  {ch("changed"), ch("changed"), ch("added")},
		"2 rules added":             {ch("added"), ch("added")},
		"2 rules removed":           {ch("removed"), ch("removed")},
		"1 rule changed, 2 removed": {ch("changed"), ch("removed"), ch("removed")},
		"1 added, 1 removed":        {ch("added"), ch("removed")},
	}
	for want, c := range cases {
		if got := Summarize(c); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

// Why: a limit of 0 or less from a caller must give the default page, not a
// slice-bounds panic on the cursor.
func TestRulesHistoryNonPositiveLimitUsesDefault(t *testing.T) {
	r, st, _ := newRules("", nil)
	st.vs = append(st.vs, ver(1, "a", t0, spec("ra00000000", "A", "1")))
	for _, l := range []int{0, -3} {
		p, err := r.History(context.Background(), 0, l)
		if err != nil || len(p.Entries) != 1 {
			t.Fatalf("limit %d: %+v %v", l, p, err)
		}
	}
}

// Why: a stored row that cannot be decoded must be named as such; its
// successor must be diffed against the last readable version, not against an
// empty list ("N rules added" would be a lie).
func TestHistoryUnreadableVersion(t *testing.T) {
	a, b := spec("ra00000000", "A", "1"), spec("rb00000000", "B", "1")
	bad := store.PolicyRuleVersion{ID: 2, Actor: "x", At: t0, Rules: "not json"}
	vs := []store.PolicyRuleVersion{ver(3, "y", t0.Add(time.Hour), a, b), bad}
	prev := ver(1, "z", t0.Add(-time.Hour), a)
	got := buildHistory(vs, &prev, time.UTC, true)
	if got[1].Summary != "Unreadable version" || got[1].Changes != nil {
		t.Fatalf("bad entry %+v", got[1])
	}
	if got[0].Summary != "Added B" {
		t.Fatalf("successor must diff against v1, got %q", got[0].Summary)
	}
	// nothing readable below: say so instead of inventing additions
	badPrev := store.PolicyRuleVersion{ID: 1, Rules: "{"}
	got = buildHistory(vs[:1], &badPrev, time.UTC, true)
	if got[0].Changes != nil || !strings.HasPrefix(got[0].Summary, "Changes unknown") {
		t.Fatalf("%+v", got[0])
	}
}
