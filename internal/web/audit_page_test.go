package web

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
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
		i := strings.Index(body, " events")
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
