package web

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
)

// statusBadges is every status badge of a page: the class string and the state it carries.
var statusBadgeRe = regexp.MustCompile(`<span class="([^"]*)" data-state="([a-z_]+)"`)

// Why (#1097, DS-015): Online or Offline was drawn three ways (a badge with a dot in the list, a 22 px pill in the
// header, a bare dot on the dashboard), so one state looked like three different things. The same state must read the
// same on every page that shows a device's connection, with the one class set of ui.StatusBadge, because the list's
// script (data-state) and the phone card's :has() rule are written against it.
func TestConnectionStatusIsOneBadge(t *testing.T) {
	e, _ := dashFleet(t, nil)
	// device 3 is offline (seen), 5 refuses the login, 6 is online
	cases := []struct {
		id    string
		state string
		class string
		attn  bool // the dashboard's attention list shows the device
	}{
		{"3", "offline", "bdg dv-st t-danger", true},
		{"5", "auth_failed", "bdg dv-st t-danger", true},
		{"6", "online", "bdg dv-st t-ok", false},
	}
	_, list := e.do(t, "GET", "/devices", "admin", nil)
	_, dash := e.do(t, "GET", "/", "admin", nil)
	attention := between(dash, `<h2>Needs attention`, `</section>`)
	for _, c := range cases {
		want := `<span class="` + c.class + `" data-state="` + c.state + `"`
		if n := strings.Count(list, want); n < 2 { // the table row and the phone card
			t.Errorf("device list: %s badge appears %d times, want the row and the card (%s)", c.state, n, want)
		}
		_, head := e.do(t, "GET", "/devices/"+c.id, "admin", nil)
		if !strings.Contains(between(head, `id="devhd"`, `devhd-meta`), want) {
			t.Errorf("device header of %s lacks %s", c.id, want)
		}
		if got := strings.Contains(attention, want); got != c.attn {
			t.Errorf("attention list shows the %s badge = %v, want %v", c.state, got, c.attn)
		}
	}
	// one look: whatever the page, a state has exactly one class set
	for name, page := range map[string]string{"list": list, "attention": attention} {
		seen := map[string]string{}
		for _, m := range statusBadgeRe.FindAllStringSubmatch(page, -1) {
			if prev, ok := seen[m[2]]; ok && prev != m[1] {
				t.Errorf("%s: state %s drawn as %q and %q", name, m[2], prev, m[1])
			}
			seen[m[2]] = m[1]
		}
	}
	// the old hand-written looks are gone: the header pill and the attention list's bare dot for a status
	_, head := e.do(t, "GET", "/devices/3", "admin", nil)
	if strings.Contains(head, "devhd-state") || strings.Contains(head, "devhd-dot") {
		t.Error("the header still writes its own status pill")
	}
	if i := strings.Index(attention, ">hallway<"); i < 0 || strings.Contains(attention[:i][strings.LastIndex(attention[:i], `<a class="att-row`):], "sdot-dot") {
		t.Error("a device in the attention list for its connection still shows the bare dot besides the badge")
	}
}

// Why (#1097): a policy deviation, a weak signal and the broker notice are no connection state, so they must not
// pretend to be one: they keep the dot of their tone and carry no status badge.
func TestAttentionItemsThatAreNoStatusKeepTheDot(t *testing.T) {
	e, _ := dashFleet(t, nil)
	_, dash := e.do(t, "GET", "/", "admin", nil)
	attention := between(dash, `<h2>Needs attention`, `</section>`)
	i := strings.Index(attention, "TelePeriod 60")
	if i < 0 {
		t.Fatalf("no deviation item in %s", attention)
	}
	row := attention[strings.LastIndex(attention[:i], `<a class="att-row`):i]
	if !strings.Contains(row, `class="sdot-dot t-warn"`) || strings.Contains(row, "data-state") {
		t.Errorf("a deviation item is a dot in the warn tone, no status: %s", row)
	}
}

// Why (#1097, A5-042): a paused live check in grey reads as "fine", although the numbers beside it are no longer
// being refreshed. Paused is the warn tone with the word, and the deviating count says from when its values are.
func TestPausedLiveCheckIsWarnAndMarksTheCount(t *testing.T) {
	st := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	aud := &fakeAudit{}
	lp, err := sysconfig.NewLivePause(t.Context(), st, aud)
	if err != nil {
		t.Fatal(err)
	}
	read := time.Now().Add(-90 * time.Minute)
	e := setupWith(t, func(d *Deps) { d.Policy = shownPolicy{read: read}; d.PolicyLivePause = lp; d.Recorder = aud })
	_, body := e.do(t, "GET", "/policy", "admin", nil)
	if !strings.Contains(body, `<span class="pol-sum-v t-ok">Running</span>`) || strings.Contains(body, "values from") {
		t.Fatalf("a running check is green and marks nothing:\n%s", between(body, `class="pol-sum"`, `class="pol-line-acts"`))
	}
	r, _ := e.do(t, "POST", "/policy/live/pause", "admin", url.Values{"csrf": {"tok"}})
	_, body = e.do(t, "GET", r.Header.Get("Location")[len("/tf"):], "admin", nil)
	sum := between(body, `class="pol-sum"`, `class="pol-line-acts"`)
	if !strings.Contains(sum, `<span class="pol-sum-v t-warn">Paused</span>`) || strings.Contains(sum, "t-neutral") {
		t.Errorf("the paused word is not warn:\n%s", sum)
	}
	count := between(sum, `data-sum`, `</p>`)
	if !regexp.MustCompile(`deviating</span>\s+\(values from <time`).MatchString(count) || strings.Contains(sum, "deviations shown are from") {
		t.Errorf("the deviating count does not say where its values are from, once:\n%s", count)
	}
}

// shownPolicy is a report with one live reading, so a paused page knows when its numbers are from.
type shownPolicy struct {
	noEval
	read time.Time
}

func (p shownPolicy) Report(context.Context, string, bool) (PolicyReport, error) {
	return PolicyReport{
		Rules:      []policy.Rule{{ID: "r1", Command: "SaveData", Want: "0"}},
		Devices:    []model.Device{{ID: 1, Name: "kitchen"}},
		Provenance: map[model.DeviceID]map[string]ValueSource{1: {"savedata": {Kind: "live", At: p.read}}},
	}, nil
}
