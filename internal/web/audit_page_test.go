package web

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why: the audit log grows without bound. A page shows 50 newest events, the
// action and result filters narrow it, paging links keep the filters, and a
// long JSON detail is expandable (pretty printed) instead of widening the table.
func TestAuditPagingAndFilters(t *testing.T) {
	e := setup(t)
	for i := 0; i < 120; i++ {
		res, act := "ok", "device.update"
		if i%3 == 0 {
			res, act = "refused: not allowed", "command.send"
		}
		e.aud.events = append(e.aud.events, model.AuditEvent{ID: int64(i), At: time.Date(2026, 10, 19, 10, 0, i%60, 0, time.UTC),
			Actor: "adam", Action: act, Target: fmt.Sprintf("device:%d", i), Result: res, Detail: `{"cmd":"Status 2","n":` + fmt.Sprint(i) + `}`})
	}
	_, body := e.do(t, "GET", "/audit?since=all", "admin", nil)
	if strings.Count(body, "<tr>") != 26 || !strings.Contains(body, "of 5, 120 events") || !strings.Contains(body, "page=2") {
		t.Fatalf("first page: rows %d %s", strings.Count(body, "<tr>"), body[strings.Index(body, "Pages"):])
	}
	_, body = e.do(t, "GET", "/audit?since=all&page=5", "admin", nil)
	if strings.Count(body, "<tr>") != 21 || strings.Contains(body, `rel="next"`) {
		t.Fatalf("last page: rows %d", strings.Count(body, "<tr>"))
	}
	// the page size is a URL parameter and the filters survive in every link
	_, body = e.do(t, "GET", "/audit?since=all&per=100&action=device.update", "admin", nil)
	if strings.Count(body, "<tr>") != 81 || !strings.Contains(body, "of 1, 80 events") {
		t.Fatalf("page size 100: rows %d", strings.Count(body, "<tr>"))
	}
	_, body = e.do(t, "GET", "/audit?since=all&action=device.update", "admin", nil)
	if !strings.Contains(body, "action=device.update&amp;dir=desc&amp;page=2") || !strings.Contains(body, "since=all&amp;sort=when") {
		t.Fatalf("a pager link lost the filter or the sort")
	}
	// Why: the sort applies to the whole result before paging, not to one page; the header links keep the filters.
	_, body = e.do(t, "GET", "/audit?since=all&sort=target&dir=desc", "admin", nil)
	first := strings.Index(body, "device:99")
	if first < 0 || strings.Index(body, "device:98") < first || !strings.Contains(body, `aria-sort="descending"`) || !strings.Contains(body, "since=all&amp;sort=actor&amp;dir=asc") {
		t.Fatalf("target sort desc: first at %d", first)
	}
	_, body = e.do(t, "GET", "/audit?since=all&result=error", "admin", nil)
	if !strings.Contains(body, "40 events") {
		t.Fatalf("result filter: %s", body[:min(len(body), 300)])
	}
	_, body = e.do(t, "GET", "/audit?since=all&action=device.update", "admin", nil)
	if !strings.Contains(body, "80 events") {
		t.Fatalf("action filter")
	}
	if !strings.Contains(body, "&#34;cmd&#34;: &#34;Status 2&#34;") {
		t.Fatalf("detail is not pretty printed in the expanded view")
	}
}

// Why (design v2, N14): the audit page filters by action family ("device.*"),
// a search box that also finds devices by name, and a "today" period. The
// family filter runs in the store, so it is not cut short by the fetch limit;
// the search finds a device's rows although the log stores "device:N".
func TestAuditFamilySearchAndToday(t *testing.T) {
	y, m, d := time.Now().Date()
	now := time.Date(y, m, d, 12, 0, 0, 0, time.Local) // midday: "today" is never across midnight
	e := setupWith(t, func(dp *Deps) { dp.Clock = noonClock{now} })
	add := func(at time.Time, actor, action, target string) {
		e.aud.events = append(e.aud.events, model.AuditEvent{At: at, Actor: actor, Action: action, Target: target, Result: "ok"})
	}
	add(now.Add(-time.Minute), "maren", "device.restart", "device:1")
	add(now.Add(-2*time.Minute), "jonas", "device.backup", "device:2")
	add(now.Add(-3*time.Minute), "jonas", "firmware.update", "device:2")
	add(now.Add(-3*24*time.Hour), "maren", "devices_old.x", "device:1")
	count := func(q string) string {
		t.Helper()
		_, body := e.do(t, "GET", "/audit?"+q, "admin", nil)
		i := strings.Index(body, " events") // the pager's "Page 1 of 1, 2 events" (the table's caption says "Audit events" before it)
		if at := strings.Index(body, "</table>"); at >= 0 {
			i = at + strings.Index(body[at:], " events")
			if i < at {
				i = -1
			}
		}
		if i < 0 {
			return "no count in " + body[:min(len(body), 200)]
		}
		j := strings.LastIndexAny(body[:i], " >") + 1
		return body[j:i]
	}
	cases := []struct{ q, want, why string }{
		{"since=all&action=device.*", "2", "a family keeps device.restart and device.backup, not devices_old.x"},
		{"since=all&action=device.restart", "1", "an exact action still works"},
		{"since=all&q=attic", "2", "device 2 is called attic: its rows are found by name"},
		{"since=all&q=MAREN", "2", "the search matches the actor, any case"},
		{"since=all&q=attic&action=firmware.*", "1", "search and family combine"},
		{"since=7d&q=maren", "2", "a 3-day-old row is inside 7 days"},
		{"since=today&action=device.*", "2", "today keeps this morning's rows"},
		{"since=today&q=maren", "1", "today drops the row of three days ago"},
	}
	for _, c := range cases {
		if got := count(c.q); got != c.want {
			t.Errorf("%s: %s events, want %s (%s)", c.q, got, c.want, c.why)
		}
	}
	_, body := e.do(t, "GET", "/audit?since=all&q=attic&action=device.*", "admin", nil)
	if !strings.Contains(body, "q=attic") {
		t.Error("the search does not survive in the pager and sort links")
	}
}

// Why: the action select offers families, one per prefix, and the "today"
// period starts at local midnight, not 24 hours back.
func TestAuditFamilyHelpers(t *testing.T) {
	got := auditFamilies([]string{"device.restart", "device.backup", "firmware.update", "login", "device.x.y"})
	if strings.Join(got, ",") != "device.*,firmware.*,login" {
		t.Errorf("families = %v", got)
	}
	for in, want := range map[string]string{"device.*": "device.", "device.restart": "", "*": "", ".*": "", "a.*.*": ""} {
		if p, ok := auditFamily(in); p != want || ok != (want != "") {
			t.Errorf("auditFamily(%q) = %q %v, want %q", in, p, ok, want)
		}
	}
	loc := time.FixedZone("x", 2*3600)
	now := time.Date(2026, 10, 22, 0, 30, 0, 0, loc)
	if got := auditSince(now, -1); !got.Equal(time.Date(2026, 10, 22, 0, 0, 0, 0, loc)) {
		t.Errorf("today = %v", got)
	}
	if got := auditSince(now, time.Hour); !got.Equal(now.Add(-time.Hour)) {
		t.Errorf("window = %v", got)
	}
	if !auditSince(now, 0).IsZero() {
		t.Error("all time must not limit")
	}
}

type noonClock struct{ t time.Time }

func (c noonClock) Now() time.Time                     { return c.t }
func (noonClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

// Why (#407): the arrow in a column header and the order of the rows must agree, in
// both directions, for every sortable column. The Action column shows a label ("Create user"),
// not the stored key ("account.create"), and people read the label, so the sort follows what is
// shown: the key order differs from the label order for these actions.
func TestAuditSortOrderMatchesArrowAndShownText(t *testing.T) {
	e := setup(t)
	base := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	// target marker t1..t6 is the row's identity; the other columns are given in the order that
	// each sort is expected to produce (marker order for an ascending sort).
	rows := []struct {
		marker, actor, action, result string
		at                            time.Time
	}{
		{"device:t1", "carol", "sysconfig.update", "ok", base.Add(5 * time.Minute)},
		{"device:t2", "adam", "policy.rules.save", "error: boom", base.Add(1 * time.Minute)},
		{"device:t3", "dora", "command.send", "error: aaa", base.Add(4 * time.Minute)},
		{"device:t4", "bob", "backup.create", "refused: no", base.Add(2 * time.Minute)},
		{"device:t5", "erin", "account.create", "refused: zz", base.Add(6 * time.Minute)},
		{"device:t6", "frank", "device.update", "error: zzz", base.Add(3 * time.Minute)},
	}
	for i, r := range rows {
		e.aud.events = append(e.aud.events, model.AuditEvent{ID: int64(i + 1), At: r.at, Actor: r.actor, Action: r.action, Target: r.marker, Result: r.result})
	}
	order := func(body string) string {
		type hit struct {
			at int
			m  string
		}
		var hits []hit
		for _, r := range rows {
			if i := strings.Index(body[strings.Index(body, "<tbody"):], r.marker); i >= 0 {
				hits = append(hits, hit{i, strings.TrimPrefix(r.marker, "device:")})
			}
		}
		sort.Slice(hits, func(a, b int) bool { return hits[a].at < hits[b].at })
		var out []string
		for _, h := range hits {
			out = append(out, h.m)
		}
		return strings.Join(out, " ")
	}
	cases := []struct {
		col, asc string // asc is the expected row order of an ascending sort
	}{
		{"when", "t2 t4 t6 t3 t1 t5"},   // oldest first
		{"actor", "t2 t4 t1 t3 t5 t6"},  // adam bob carol dora erin frank
		{"action", "t4 t3 t5 t6 t2 t1"}, // by label: Backup, Console command, Create user, Edit device, Save policy rules, System settings
		{"target", "t1 t2 t3 t4 t5 t6"}, // device:t1 ... device:t6
		{"result", "t1 t3 t2 t6 t4 t5"}, // ok first, then error: boom, refused: no
	}
	reverse := func(s string) string {
		f := strings.Fields(s)
		slices.Reverse(f)
		return strings.Join(f, " ")
	}
	for _, c := range cases {
		for _, dir := range []string{"asc", "desc"} {
			_, body := e.do(t, "GET", "/audit?since=all&sort="+c.col+"&dir="+dir, "admin", nil)
			want := c.asc
			arrow, aria := "is-asc", "ascending"
			if dir == "desc" {
				want, arrow, aria = reverse(c.asc), "is-desc", "descending"
			}
			if got := order(body); got != want {
				t.Errorf("sort=%s dir=%s: rows %q, want %q", c.col, dir, got, want)
			}
			if !strings.Contains(body, `aria-sort="`+aria+`"><a class="sortlink `+arrow+`"`) {
				t.Errorf("sort=%s dir=%s: the header does not show %s", c.col, dir, arrow)
			}
		}
	}
}

// Why (#407): the sort arrow must point the way people read it: up for ascending, down for
// descending. The header icon is drawn with two CSS triangles; "to bottom ..." gradients placed at
// the top make an upward point, so the ascending icon needs exactly those. Until #407 it was drawn
// the other way round, so an ascending column looked descending.
func TestSortIconAscendingPointsUp(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	rule := func(sel string) string {
		i := strings.Index(string(css), sel+" {")
		if i < 0 {
			t.Fatalf("no rule %s", sel)
		}
		return string(css)[i : i+strings.Index(string(css)[i:], "}")]
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if a := norm(rule(".sortlink.is-asc .sortmark")); !strings.Contains(a, "to bottom right, transparent 50%, var(--color-accent) 50%) top left") {
		t.Errorf("ascending icon does not point up: %s", a)
	}
	if d := norm(rule(".sortlink.is-desc .sortmark")); !strings.Contains(d, "to top right, transparent 50%, var(--color-accent) 50%) bottom left") {
		t.Errorf("descending icon does not point down: %s", d)
	}
}

// resultBadges returns, per Result cell of the audit table, the badge's class and word.
func resultBadges(t *testing.T, body string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, m := range regexp.MustCompile(`<td data-label="Result"[^>]*>(.*?)</td>`).FindAllStringSubmatch(body, -1) {
		b := regexp.MustCompile(`<span class="([^"]*)">([^<]*)</span>`).FindStringSubmatch(m[1])
		if b == nil {
			t.Fatalf("a Result cell without a badge: %s", m[1])
		}
		out = append(out, [2]string{b[1], b[2]})
	}
	return out
}

// Why (#1069): the Result column is the design system's badge (ui.Badge, class bdg) in the design's
// tone words, not the old glyph badge, and its colour comes from the same classification as the
// result filter and the word (auditStatus). Two classifiers had drifted apart: an empty result and
// "role changed" read as success but were drawn red, a refusal that only the detail names was red
// instead of amber. A badge that disagrees with its word sends the reader to the wrong conclusion.
func TestAuditResultIsTheDesignSystemBadge(t *testing.T) {
	e := setup(t)
	at := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	for i, r := range []struct{ result, detail string }{
		{"ok", `{}`},
		{"failed: timeout", `{}`},
		{"refused: not allowed", `{}`},
		{"role changed", `{}`},
		{"", `{}`},
		{"error", `{"refused":"policy"}`},
		{"forbidden", `{}`},
	} {
		e.aud.events = append(e.aud.events, model.AuditEvent{ID: int64(i), At: at.Add(-time.Duration(i) * time.Minute), Actor: "adam",
			Action: "device.update", Target: "device:1", Result: r.result, Detail: r.detail})
	}
	_, body := e.do(t, "GET", "/audit?since=all", "admin", nil)
	if strings.Contains(body, `class="badge`) {
		t.Errorf("the audit page still renders the pre-v2 badge")
	}
	// newest first: the events were added newest first, and "when" sorts descending by default
	want := [][2]string{
		{"bdg t-ok", "Succeeded"}, {"bdg t-danger", "Failed"}, {"bdg t-warn", "Refused"}, {"bdg t-ok", "Succeeded"},
		{"bdg t-ok", "Succeeded"}, {"bdg t-warn", "Refused"}, {"bdg t-warn", "Refused"},
	}
	got := resultBadges(t, body)
	if !slices.Equal(got, want) {
		t.Errorf("result badges (class, word):\n got %q\nwant %q", got, want)
	}
}

// Why (#1069): the pager is made of the design system's buttons. A page that cannot be reached is a
// disabled button, not a grey span that still looks like one you can press; the page size you are on
// is marked for assistive technology (aria-current="page") and styled as the selected one, and it is
// a link to the page you are on, not to page 1.
func TestAuditPagerUsesTheDesignSystemButtons(t *testing.T) {
	e := setup(t)
	for i := 0; i < 60; i++ {
		e.aud.events = append(e.aud.events, model.AuditEvent{ID: int64(i), At: time.Date(2026, 10, 19, 10, 0, i%60, 0, time.UTC),
			Actor: "adam", Action: "device.update", Target: "device:1", Result: "ok", Detail: `{}`})
	}
	nav := func(q string) string {
		t.Helper()
		_, body := e.do(t, "GET", "/audit?since=all"+q, "admin", nil)
		i := strings.Index(body, `<nav class="pager"`)
		if i < 0 {
			t.Fatalf("no pager on %q", q)
		}
		return body[i : i+strings.Index(body[i:], "</nav>")]
	}
	for _, old := range []string{`class="button`, "is-off", "is-current", `aria-current="true"`} {
		if strings.Contains(nav(""), old) {
			t.Errorf("the pager still has %s", old)
		}
	}
	first := nav("")
	if !strings.Contains(first, `<button type="button" class="btn btn-sm" disabled>Previous</button>`) {
		t.Errorf("page 1: Previous is not a disabled button: %s", first)
	}
	if !strings.Contains(first, `rel="next"`) || strings.Contains(first, `disabled>Next`) {
		t.Errorf("page 1: Next is not a link: %s", first)
	}
	last := nav("&page=3")
	if !strings.Contains(last, `<button type="button" class="btn btn-sm" disabled>Next</button>`) || !strings.Contains(last, `rel="prev"`) {
		t.Errorf("last page: Next is not a disabled button or Previous not a link: %s", last)
	}
	cur := regexp.MustCompile(`<a [^>]*aria-current="page"[^>]*>([^<]*)</a>`).FindAllStringSubmatch(nav("&page=2"), -1)
	if len(cur) != 1 || cur[0][1] != "25" || !strings.Contains(cur[0][0], "btn-current") || !strings.Contains(cur[0][0], "page=2") {
		t.Errorf("page 2: exactly the current size is marked, as a link to this page with btn-current: %q", cur)
	}
}

// Why (#1094): the events table has no heading of its own (the page title names the page, not the
// table), so it is named by a caption, and its header cells say they head a column. The caption must
// stay the first child of the table when the server rewrites the table for the user's saved columns:
// a <caption> after the <colgroup> is invalid and browsers drop the caption.
func TestAuditTableHasCaptionFirstAndColumnScope(t *testing.T) {
	now := time.Date(2030, 1, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, pref string }{
		{"defaults", ""},
		{"custom order and a hidden column", `{"o":["action","actor"],"h":["target"]}`},
		{"a pinned column and a width", `{"p":{"result":"r"},"w":{"actor":30}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakePrefs()
			e := setupWith(t, func(d *Deps) { d.Prefs = fp; d.Clock = noonClock{now} })
			e.aud.Record(t.Context(), model.AuditEvent{ID: 1, At: now.Add(-time.Hour), Actor: "alice", Action: "device.update", Target: "device:1", Result: "ok"})
			if tc.pref != "" {
				_ = fp.SetUserPref(t.Context(), "test:admin", "table.audit", tc.pref)
			}
			_, body := e.do(t, "GET", "/audit", "admin", nil)
			i := strings.Index(body, `<table`)
			if i < 0 {
				t.Fatal("no table on the audit page")
			}
			tbl := body[i : i+strings.Index(body[i:], "</table>")]
			after := tbl[strings.Index(tbl, ">")+1:]
			if !strings.HasPrefix(after, `<caption class="sr-only">Audit events</caption>`) {
				t.Errorf("the caption is not the first child of the table: %.200s", after)
			}
			head := tbl[strings.Index(tbl, "<thead"):strings.Index(tbl, "</thead>")]
			for _, th := range regexp.MustCompile(`<th\b[^>]*>`).FindAllString(head, -1) {
				if !strings.Contains(th, `scope="col"`) && !strings.Contains(th, `class="spare"`) {
					t.Errorf("header cell without scope=col: %s", th)
				}
			}
			if !strings.Contains(tbl, `data-table="audit"`) {
				t.Error("the audit table keeps its registry key: saved widths, order and pins live under it")
			}
		})
	}
}
