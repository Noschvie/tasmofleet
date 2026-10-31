package web

import (
	"html"
	"net/url"
	"strings"
	"testing"
	"time"
)

// seedTimes sets the save time and actor of the stored versions, 1-based by version id. The
// test store keeps no clock, and the fold rule depends on both.
func seedTimes(mem *rulesMem, ats []time.Time, actors []string) {
	for i := range mem.vs {
		mem.vs[i].At, mem.vs[i].Actor = ats[i], actors[i]
	}
}

// historyEnv has four versions of one rule: v1..v3 are quick saves of one actor, v4 (current) is another actor's.
func historyEnv(t *testing.T) (*env, *rulesMem) {
	t.Helper()
	y, m, d := time.Now().Date()
	return historyEnvAt(t, time.Date(y, m, d, 12, 0, 0, 0, time.Local))
}

// historyEnvAt is historyEnv with the page's clock stopped at now. The versions are seeded from the
// start of now's day, never from now minus an hour, so "Today" holds at any time of day.
func historyEnvAt(t *testing.T, now time.Time) (*env, *rulesMem) {
	t.Helper()
	e, mem, _ := rulesEnvDeps(t, func(dp *Deps) { dp.Clock = noonClock{now} })
	saveRule(t, e, nil)
	id := ruleIDs(t, mem)[0]
	for i, want := range []string{"1", "0", "1"} {
		saveRule(t, e, url.Values{"id": {id}, "version": {vstr(int64(i + 1))}, "want": {want}})
	}
	base := time.Date(now.Year(), now.Month(), now.Day(), 0, 1, 0, 0, now.Location())
	seedTimes(mem, []time.Time{base, base.Add(30 * time.Second), base.Add(60 * time.Second), base.Add(30 * time.Minute)},
		[]string{"admin", "admin", "admin", "other"})
	return e, mem
}

// Why: three quick saves read as noise; they must be one expandable entry, while the current
// version stays its own line so it is never hidden inside a fold.
func TestPolicyHistoryFoldsQuickSavesAndKeepsTheCurrentVersionAlone(t *testing.T) {
	e, _ := historyEnv(t)
	r, body := e.do(t, "GET", "/policy/history", "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	for _, w := range []string{"3 quick saves", "v1–3", "<details", "?v=2#diff", "Current", "Today · "} {
		if !strings.Contains(body, w) {
			t.Errorf("history lacks %q", w)
		}
	}
	if strings.Count(body, `<details class="pol-hfold`) != 1 {
		t.Errorf("only the quick saves fold, got %d folds", strings.Count(body, `<details class="pol-hfold`))
	}
	if !strings.Contains(body, `id="diff"`) || !strings.Contains(body, "Changes in v4") {
		t.Error("the newest version is picked by default")
	}
}

// Why (#340): the suite failed between 00:00 and 01:00 UTC because the versions were seeded an hour
// before the real clock and so landed on yesterday. "Today" is decided by the page's clock; just
// after midnight, at noon and just before the next midnight the saves of this day still read "Today",
// and the day before reads without it.
func TestPolicyHistoryTodayDoesNotDependOnTheTimeOfDay(t *testing.T) {
	y, m, d := time.Now().Date()
	for _, hm := range [][2]int{{0, 35}, {12, 0}, {23, 59}} {
		now := time.Date(y, m, d, hm[0], hm[1], 0, 0, time.Local)
		e, mem := historyEnvAt(t, now)
		_, body := e.do(t, "GET", "/policy/history", "admin", nil)
		if !strings.Contains(body, "Today · ") {
			t.Errorf("at %02d:%02d the saves of this day lack %q", hm[0], hm[1], "Today · ")
		}
		for i := range mem.vs {
			mem.vs[i].At = mem.vs[i].At.Add(-24 * time.Hour)
		}
		_, body = e.do(t, "GET", "/policy/history", "admin", nil)
		if strings.Contains(body, "Today · ") {
			t.Errorf("at %02d:%02d yesterday's saves read as Today", hm[0], hm[1])
		}
	}
}

// Why: a link to a version that does not exist must say so instead of showing the newest one.
func TestPolicyHistoryUnknownVersionIs404(t *testing.T) {
	e, _ := historyEnv(t)
	for _, q := range []string{"?v=99", "?v=abc", "?v=-1", "?before=x"} {
		if r, _ := e.do(t, "GET", "/policy/history"+q, "admin", nil); r.StatusCode != 404 {
			t.Errorf("%s: status %d, want 404", q, r.StatusCode)
		}
	}
}

// Why: Revert changes the rules, so only admins get it; operators may read the history. The dialog lists
// what the revert does (current -> the target), the opposite of the diff on the card.
func TestPolicyHistoryRevertIsAdminOnlyAndListsWhatItChanges(t *testing.T) {
	e, _ := historyEnv(t)
	_, op := e.do(t, "GET", "/policy/history?v=4", "operator", nil)
	if strings.Contains(op, "Revert to") || strings.Contains(op, "dlg-revert") {
		t.Error("an operator sees a Revert control")
	}
	_, body := e.do(t, "GET", "/policy/history?v=4", "admin", nil)
	if !strings.Contains(body, "Revert to v3…") {
		t.Fatal("no Revert to v3 button")
	}
	i := strings.Index(body, `id="dlg-revert"`)
	dlg := html.UnescapeString(body[i:])
	dlg = dlg[:strings.Index(dlg, "</dialog>")]
	if !strings.Contains(dlg, `expected "1" -> "0"`) || !strings.Contains(dlg, "/policy/rules/revert/3") {
		t.Fatalf("revert dialog must list current (want 1) -> v3 (want 0):\n%s", dlg)
	}
	if !strings.Contains(html.UnescapeString(body[:i]), `expected "0" -> "1"`) {
		t.Error("the card must show what v4 changed (v3 -> v4)")
	}
}

// Why: the first version has nothing to revert to.
func TestPolicyHistoryFirstVersionHasNoRevert(t *testing.T) {
	e, _ := historyEnv(t)
	_, body := e.do(t, "GET", "/policy/history?v=1", "admin", nil)
	if strings.Contains(body, "Revert to") || !strings.Contains(body, "Changes in v1") {
		t.Error("v1 shows its changes but no revert")
	}
}

func TestPolicyRevertLandsOnTheHistoryWithItsFlash(t *testing.T) {
	e, mem := historyEnv(t)
	r, _ := e.do(t, "POST", "/policy/rules/revert/1", "admin", url.Values{"csrf": {"tok"}})
	if r.StatusCode != 303 || !strings.HasSuffix(r.Header.Get("Location"), "/policy/history?ok=reverted") || len(mem.vs) != 5 {
		t.Fatalf("revert: %d %q versions=%d", r.StatusCode, r.Header.Get("Location"), len(mem.vs))
	}
	if _, body := e.do(t, "GET", "/policy/history?ok=reverted", "admin", nil); !strings.Contains(body, "Reverted: ") {
		t.Error("no flash on the history")
	}
}

// Why: with 50 versions a page, the older ones must stay reachable, and a link to a version on a
// later page (the rule page's) must find it without a cursor.
func TestPolicyHistoryPagesAndFindsVersionsOnLaterPages(t *testing.T) {
	e, mem := historyEnv(t)
	first := mem.vs[0]
	for len(mem.vs) < 60 {
		v := first
		v.ID = int64(len(mem.vs) + 1)
		mem.vs = append(mem.vs, v)
	}
	base := time.Now().Add(-90 * 24 * time.Hour)
	for i := range mem.vs { // a day apart: nothing folds
		mem.vs[i].At, mem.vs[i].Actor = base.Add(time.Duration(i)*24*time.Hour), "admin"
	}
	_, p1 := e.do(t, "GET", "/policy/history", "admin", nil)
	if !strings.Contains(p1, "Older versions") || strings.Contains(p1, "?v=5#diff") || !strings.Contains(p1, "?v=60#diff") {
		t.Fatal("first page: 60..11 with a link to older ones")
	}
	r, p2 := e.do(t, "GET", "/policy/history?v=5", "admin", nil)
	if r.StatusCode != 200 || !strings.Contains(p2, "Changes in v5") || !strings.Contains(p2, "Newest versions") || strings.Contains(p2, "Older versions") {
		t.Fatalf("v5 is on the second page: %d", r.StatusCode)
	}
}

func TestPolicyHistoryEmptyState(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	r, body := e.do(t, "GET", "/policy/history", "admin", nil)
	if r.StatusCode != 200 || !strings.Contains(body, "No versions yet") {
		t.Fatalf("empty: %d", r.StatusCode)
	}
}

// Why: a rule's page lists only the versions that touched it, not every save of the list.
func TestRulePageHistoryListsOnlyItsOwnVersions(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	saveRule(t, e, nil) // v1: rule A
	a := ruleIDs(t, mem)[0]
	saveRule(t, e, url.Values{"version": {"1"}, "command": {"SetOption0"}, "name": {"Rule B"}}) // v2: rule B added
	saveRule(t, e, url.Values{"id": {a}, "version": {"2"}, "want": {"1"}})                      // v3: rule A edited
	_, body := e.do(t, "GET", "/policy/rules/"+a, "admin", nil)
	i := strings.Index(body, `id="rule-history"`)
	if i < 0 {
		t.Fatal("no history card")
	}
	card := body[i:]
	card = card[:strings.Index(card, "</details>")]
	if !strings.Contains(card, "?v=1#diff") || !strings.Contains(card, "?v=3#diff") || strings.Contains(card, "?v=2#diff") {
		t.Errorf("rule A history must list v1 and v3 only:\n%s", card)
	}
	if !strings.Contains(card, "All policy history") {
		t.Error("no link to the whole history")
	}
}

// Why: picking a version inside a fold must show that version's own change, not the fold's net
// effect; the first version of the history diffs against an empty base.
func TestPolicyHistoryPickInsideAFoldShowsOnlyThatVersion(t *testing.T) {
	e, _ := historyEnv(t)
	_, v2 := e.do(t, "GET", "/policy/history?v=2", "admin", nil)
	card := html.UnescapeString(v2[strings.Index(v2, `id="diff"`):])
	if !strings.Contains(card, "v1 → v2") || !strings.Contains(card, "Changed: ") || strings.Contains(card, "Added: ") {
		t.Errorf("v2 changed the rule, it did not add it:\n%s", card)
	}
	_, v1 := e.do(t, "GET", "/policy/history?v=1", "admin", nil)
	card = html.UnescapeString(v1[strings.Index(v1, `id="diff"`):])
	if !strings.Contains(card, "Added: ") || !strings.Contains(card, "first version") {
		t.Errorf("v1 added the rule:\n%s", card)
	}
}

// Why: reverting to what is current changes nothing; the answer belongs on the History tab, with its own words.
func TestPolicyRevertToTheCurrentVersionSaysNothingChanged(t *testing.T) {
	e, mem := historyEnv(t)
	r, _ := e.do(t, "POST", "/policy/rules/revert/4", "admin", url.Values{"csrf": {"tok"}})
	if r.StatusCode != 303 || !strings.HasSuffix(r.Header.Get("Location"), "/policy/history?ok=nochange") || len(mem.vs) != 4 {
		t.Fatalf("revert to current: %d %q versions=%d", r.StatusCode, r.Header.Get("Location"), len(mem.vs))
	}
	if _, body := e.do(t, "GET", "/policy/history?ok=nochange", "admin", nil); !strings.Contains(body, "Nothing changed") {
		t.Error("no flash")
	}
}

// Why: a fold inside one minute reads "10:00", not "10:00–10:00".
func TestPolicyHistoryFoldWithinOneMinuteShowsOneTime(t *testing.T) {
	e, mem := historyEnv(t)
	base := time.Date(2026, 10, 1, 10, 0, 5, 0, time.Local)
	for i := range mem.vs[:3] {
		mem.vs[i].At = base.Add(time.Duration(i*10) * time.Second)
	}
	_, body := e.do(t, "GET", "/policy/history", "admin", nil)
	i := strings.Index(body, `<details class="pol-hfold`)
	sum := body[i : i+strings.Index(body[i:], "</summary>")]
	if strings.Contains(sum, "–<time") {
		t.Errorf("one minute, one time:\n%s", sum)
	}
}

// Why: Run now on the History tab must stay on it (the other tabs do the same), and the tab
// row of every page names the same tabs.
func TestPolicyHistoryRunNowStaysOnTheTab(t *testing.T) {
	e, _ := historyEnv(t)
	r, body := e.do(t, "POST", "/policy/live", "admin", url.Values{"csrf": {"tok"}, "back": {"history"}})
	if r.StatusCode != 200 || !strings.Contains(body, `id="history"`) || !strings.Contains(body, `name="back" value="history"`) {
		t.Fatalf("run now on the history tab: %d", r.StatusCode)
	}
	_, over := e.do(t, "GET", "/policy", "admin", nil)
	for _, w := range []string{">Rules", ">Deviating devices", ">History"} {
		if !strings.Contains(over, w) {
			t.Errorf("overview tabs lack %q", w)
		}
	}
}
