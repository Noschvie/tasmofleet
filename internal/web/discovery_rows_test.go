package web

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/discovery"
	"github.com/hellwand/tasmofleet/internal/model"
)

// The Discovered box of the Add page after the UI audit (#1081, #905): Show ignored is a real checkbox in its own
// GET form, address changes and conflicts are rows of the one table, one verb pair ("Ignore" / "Show again") is used
// everywhere, and what a button or a cell says is visible text, not only a tooltip.

var (
	reShowIgnForm   = regexp.MustCompile(`(?s)<form class="add-showign"[^>]*>.*?</form>`)
	reNewdevForm    = regexp.MustCompile(`(?s)<form class="add-newdev"[^>]*>.*?</form>`)
	reTitleAttr     = regexp.MustCompile(`\stitle="[^"]*"`)
	reSelectAll     = regexp.MustCompile(`<input type="checkbox" data-select-all[^>]*>`)
	reOldVerbButton = regexp.MustCompile(`>\s*(Dismiss|Restore|Un-ignore)\s*</button>`)
)

// discTable is the markup of the Discovered table (the desktop view), "" when the page has none.
func discTable(body string) string {
	return between(body, `<div class="add-table add-disc-table">`, `</table>`)
}

// rowWith is the whole table row (<tr> to </tr>) that holds marker, "" when no row does.
func rowWith(table, marker string) string {
	i := strings.Index(table, marker)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(table[:i], "<tr")
	end := strings.Index(table[i:], "</tr>")
	if start < 0 || end < 0 {
		return ""
	}
	return table[start : i+end]
}

// moveFixture is setupDiscovery plus a second address change, of a device with a stored web password,
// and a new device with an MQTT topic.
func moveFixture(t *testing.T) (*env, *fakeDiscovery) {
	t.Helper()
	e, fd := setupDiscovery(t)
	d1 := e.inv.devs[1] // kitchen has a stored web password; a move needs its MAC
	d1.MAC = "aa:bb:cc:dd:ee:04"
	e.inv.devs[1] = d1
	now := time.Now()
	fd.props = append(fd.props,
		discovery.Proposal{ID: 4, Version: "v4", Kind: discovery.KindAddressChange, Via: "mqtt", ExistingID: 1, SeenAt: now, Device: model.Device{Name: "kitchen", Address: "10.0.0.61", MAC: "aa:bb:cc:dd:ee:04"}},
		discovery.Proposal{ID: 5, Version: "v5", Kind: discovery.KindNew, Via: "mqtt", SeenAt: now, Device: model.Device{Name: "Porch plug", Address: "10.0.0.62", MAC: "aa:bb:cc:dd:ee:05", Topic: "porch_plug"}},
	)
	return e, fd
}

// Why (A3-003, #1081): "Show ignored" was a link with role=checkbox and a drawn 16 px box: a screen reader
// announced a checkbox that Space could not toggle, and the box was not a control a browser knows. It is a real
// checkbox in a GET form of its own: the form must not be the POST form (the CSRF token and the ticked ids
// would travel in the URL), and without JavaScript an Apply button submits it.
func TestShowIgnoredIsARealCheckboxInItsOwnGetForm(t *testing.T) {
	e, fd := setupDiscovery(t)
	fd.ignored = []discovery.Proposal{{ID: 9, Version: "v9", Kind: discovery.KindNew, SeenAt: time.Now(), Device: model.Device{Name: "hidden-lamp", Address: "10.0.0.60", MAC: "aa:bb:cc:dd:ee:09"}}}
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	if regexp.MustCompile(`<a[^>]*role="checkbox"`).MatchString(body) {
		t.Error("an <a role=checkbox> is on the page: a link drawn as a checkbox")
	}
	form := reShowIgnForm.FindString(body)
	if form == "" {
		t.Fatal("no form of Show ignored")
	}
	for _, want := range []string{`method="get"`, `data-autosubmit`, `<input type="checkbox" name="ignored"`, "Show ignored (1)", `data-autosubmit-hide`, ">Apply</button>"} {
		if !strings.Contains(form, want) {
			t.Errorf("the Show ignored form lacks %q: %s", want, form)
		}
	}
	if strings.Contains(form, "csrf") || strings.Contains(form, " checked") {
		t.Errorf("the form carries a CSRF token or is ticked while ignored devices are hidden: %s", form)
	}
	if post := reNewdevForm.FindString(body); strings.Contains(post, `name="ignored"`) && strings.Contains(post, `type="checkbox" name="ignored"`) {
		t.Error("the checkbox sits inside the POST form")
	}
	if strings.Contains(body, "hidden-lamp") {
		t.Error("ignored devices are shown without the switch")
	}

	// ticked: the browser sends ignored=on; the list shows them and the checkbox is ticked
	_, body = e.do(t, "GET", "/devices/new?ignored=on", "admin", nil)
	if !strings.Contains(reShowIgnForm.FindString(body), `<input type="checkbox" name="ignored" checked`) || !strings.Contains(body, "hidden-lamp") {
		t.Error("ignored=on (what a ticked checkbox sends) does not show the ignored devices")
	}
}

// Why: a checkbox without a value sends "on", and older links and the POST forms carry "1": either one
// switches the list on, and an action that returns to the list keeps it on whichever was sent.
func TestIgnoredSwitchAcceptsWhatACheckboxSends(t *testing.T) {
	e, fd := setupDiscovery(t)
	fd.ignored = []discovery.Proposal{{ID: 9, Version: "v9", Kind: discovery.KindNew, SeenAt: time.Now(), Device: model.Device{Name: "hidden-lamp", Address: "10.0.0.60", MAC: "aa:bb:cc:dd:ee:09"}}}
	for _, q := range []string{"ignored=on", "ignored=1"} {
		if _, body := e.do(t, "GET", "/devices/new?"+q, "admin", nil); !strings.Contains(body, "hidden-lamp") {
			t.Errorf("?%s: the ignored device is not listed", q)
		}
	}
	for _, v := range []string{"on", "1"} {
		r, _ := e.do(t, "POST", "/discovery/proposals/1/dismiss", "admin", url.Values{"version": {"v1"}, "ignored": {v}})
		if loc := r.Header.Get("Location"); r.StatusCode != 303 || !strings.Contains(loc, "ignored=1") {
			t.Errorf("Ignore with ignored=%s returns to %q, want the list with the switch on", v, loc)
		}
	}
	if _, body := e.do(t, "GET", "/devices/new?ignored=", "admin", nil); strings.Contains(body, "hidden-lamp") {
		t.Error("an empty ignored= switches the list on")
	}
}

// Why (A3-005, A3-006): the page said Ignore for a new device, Dismiss for an address change, Restore for
// both and Un-ignore in the scan results, and the notices said Dismissed. and Restored. One action has one
// name: "Ignore" and "Show again" (a person who ignored a device finds the button that undoes it by the same words).
func TestOneVerbPairEverywhere(t *testing.T) {
	now := time.Now()
	fd := &fakeDiscovery{
		props: []discovery.Proposal{
			{ID: 2, Version: "v2", Kind: discovery.KindAddressChange, Via: "scan", ExistingID: 2, SeenAt: now, Device: model.Device{Name: "kitchen", Address: "10.0.0.51", MAC: "aa:bb:cc:dd:ee:02"}},
			{ID: 3, Version: "v3", Kind: discovery.KindConflict, Via: "mqtt", ExistingID: 2, SeenAt: now, Device: model.Device{Name: "x", Address: "10.0.0.52", MAC: "aa:bb:cc:dd:ee:03"}},
			{ID: 4, Version: "v4", Kind: discovery.KindNew, Via: "scan", SeenAt: now, Device: model.Device{Name: "fresh", Address: "10.0.0.50", MAC: "aa:bb:cc:dd:ee:01"}},
		},
		ignored: []discovery.Proposal{
			{ID: 5, Version: "v5", Kind: discovery.KindNew, Via: "scan", SeenAt: now, Device: model.Device{Name: "old", Address: "10.0.0.53", MAC: "aa:bb:cc:dd:ee:05"}},
			{ID: 6, Version: "v6", Kind: discovery.KindAddressChange, Via: "scan", ExistingID: 2, SeenAt: now, Device: model.Device{Name: "kitchen", Address: "10.0.0.54", MAC: "aa:bb:cc:dd:ee:06"}},
			{ID: 7, Version: "v7", Kind: discovery.KindConflict, Via: "mqtt", ExistingID: 2, SeenAt: now, Device: model.Device{Name: "y", Address: "10.0.0.55", MAC: "aa:bb:cc:dd:ee:07"}},
		},
		scan: discovery.ScanStatus{ID: "abc123", State: discovery.ScanDone, Total: 2, Done: 2, StartedAt: now, FinishedAt: now,
			Hits: []discovery.Hit{
				{Address: "10.0.0.50", Port: 80, MAC: "aa:bb:cc:dd:ee:01", Name: "fresh", Outcome: "new proposal"},
				{Address: "10.0.0.53", Port: 80, MAC: "aa:bb:cc:dd:ee:05", Name: "old", Outcome: "new proposal"},
			}},
	}
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	_, add := e.do(t, "GET", "/devices/new?ignored=1", "admin", nil)
	_, scan := e.do(t, "GET", "/discovery/scan", "admin", nil)
	for name, page := range map[string]string{"Add page": add, "scan results": scan} {
		if m := reOldVerbButton.FindString(page); m != "" {
			t.Errorf("%s: a button still says %q", name, m)
		}
	}
	// every ignored row of every kind offers Show again, with the version it was shown (table and phone card each)
	for _, id := range []string{"5", "6", "7"} {
		want := `formaction="/tf/discovery/proposals/` + id + `/undismiss" name="version" value="v` + id + `">Show again</button>`
		if n := strings.Count(add, want); n != 2 {
			t.Errorf("Add page: ignored proposal %s has %d Show again buttons, want 2 (table and card)", id, n)
		}
	}
	if n := strings.Count(scan, `/undismiss" name="version" value="v5">Show again</button>`); n != 2 {
		t.Errorf("scan results: %d Show again buttons for the ignored hit, want 2", n)
	}
	// Ignore on an address change and on a conflict, not Dismiss
	for _, id := range []string{"2", "3"} {
		want := `formaction="/tf/discovery/proposals/` + id + `/dismiss" name="version" value="v` + id + `">Ignore</button>`
		if n := strings.Count(add, want); n != 2 {
			t.Errorf("Add page: proposal %s has %d Ignore buttons, want 2", id, n)
		}
	}
	// the notices
	for ok, want := range map[string]string{"dismissed": "Ignored.", "undismissed": "Shown again."} {
		_, body := e.do(t, "GET", "/devices/new?ok="+ok, "admin", nil)
		if !strings.Contains(body, want) || strings.Contains(body, "Dismissed.") || strings.Contains(body, "Restored.") {
			t.Errorf("ok=%s: the notice is not %q", ok, want)
		}
	}
}

// Why: the dashboard offers the same action for an address change, and the audit log names the same
// actions. A person who reads "Dismiss" there and "Ignore" here wonders whether they differ.
func TestDashboardAndAuditUseTheSameVerbs(t *testing.T) {
	if got := auditLabel("discovery.undismiss"); got != "Show discovered device again" {
		t.Errorf("audit label of undismiss = %q", got)
	}
	if got := auditLabel("discovery.dismiss"); got != "Ignore discovered device" {
		t.Errorf("audit label of dismiss = %q", got)
	}
	e, _ := dashFleet(t, nil)
	_, body := e.do(t, "GET", "/", "admin", nil)
	att := between(body, `<h2>Needs attention`, `</section>`)
	if strings.Contains(att, ">Dismiss<") {
		t.Error("Needs attention still says Dismiss")
	}
}

// Why (A3-005): an address change was a card in a second component with its own buttons, and its explanation
// ("The new address must answer with the device's MAC...") was repeated in every card. It is a row of the one
// table with a badge, the sentence stands once above the table, and a row keeps only what is its own
// exception (here: a stored web password).
func TestAddressChangesAreRowsOfTheTable(t *testing.T) {
	e, _ := moveFixture(t)
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	if strings.Contains(body, `class="add-moves"`) {
		t.Error("an Address changed or Conflicts card is still rendered")
	}
	table := discTable(body)
	if table == "" {
		t.Fatal("no Discovered table")
	}
	rows := strings.Split(table, "<tr")[1:]
	var moves []string
	for _, r := range rows {
		if strings.Contains(r, ">New address<") {
			moves = append(moves, r)
		}
	}
	if len(moves) != 2 {
		t.Fatalf("%d rows carry the New address badge, want 2", len(moves))
	}
	for i, want := range []struct{ id, version, oldAddr, newAddr string }{{"2", "v2", "10.0.0.2", "10.0.0.51"}, {"4", "v4", "10.0.0.1", "10.0.0.61"}} {
		r := moves[i]
		for _, s := range []string{want.newAddr, "was " + want.oldAddr,
			`formaction="/tf/discovery/proposals/` + want.id + `/accept" name="version" value="` + want.version + `">Apply</button>`,
			`formaction="/tf/discovery/proposals/` + want.id + `/dismiss" name="version" value="` + want.version + `">Ignore</button>`} {
			if !strings.Contains(r, s) {
				t.Errorf("row of proposal %s lacks %q: %s", want.id, s, r)
			}
		}
		if strings.Contains(r, `name="id"`) {
			t.Errorf("row of proposal %s has a selection box: Ignore selected would hide it unseen", want.id)
		}
	}
	// the conflict is a row too, and it cannot be applied
	if c := rowWith(table, ">Conflict<"); c == "" || strings.Contains(c, ">Apply</button>") {
		t.Errorf("the conflict is no row, or offers Apply: %q", c)
	}
	if n := strings.Count(body, "must answer with the device’s MAC"); n != 1 {
		t.Errorf("the MAC sentence appears %d times, want once above the table", n)
	}
	if strings.Index(body, "must answer with the device’s MAC") > strings.Index(body, `<div class="add-table add-disc-table">`) {
		t.Error("the sentence is below the table")
	}
	// the exception belongs to its row: once in the table and once in the phone card of that row, not on the others
	if n := strings.Count(table, "It has a stored web password"); n != 1 {
		t.Errorf("the exception of the device with a stored password appears %d times in the table, want once, in its row", n)
	}
	if n := strings.Count(body, "It has a stored web password"); n != 2 {
		t.Errorf("the exception appears %d times on the page, want 2 (table row and phone card)", n)
	}
	// 2 new devices + 2 address changes + 1 conflict
	if !strings.Contains(body, "Waiting for a decision (5)") {
		t.Error("the heading does not count what the table holds: new devices, address changes and conflicts")
	}
}

// Why: the table holds known devices too, so a heading "New devices (N)" counted only some of its rows and read
// "(0)" above a table with a row in it. With nothing new, the heading still counts the open address change and
// conflict, and the select-all (which ticks only new devices) is not offered: it would tick nothing, and Enter on
// it would submit the form with the first row's button.
func TestHeadingCountsWhatTheTableHoldsAndSelectAllNeedsANewDevice(t *testing.T) {
	e, fd := setupDiscovery(t)
	fd.props = fd.props[1:] // the address change (2) and the conflict (3): no new device
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	if !strings.Contains(body, "Waiting for a decision (2)") {
		t.Error("the heading does not count the open address change and conflict")
	}
	if strings.Contains(body, "New devices (") {
		t.Error("the old heading, which counts only new devices, is back")
	}
	table := discTable(body)
	if table == "" {
		t.Fatal("no Discovered table")
	}
	if reSelectAll.MatchString(table) {
		t.Error("the select-all is offered although no row has a box")
	}
}

// Why (A3-048): the name and the MQTT topic were in a tooltip, which touch and keyboard users never see (the second
// Add with its "with a password" tooltip is gone, #1110). They are text on the page; the select-all box was hidden
// until a script revealed it, so a table without the script (or before it ran) had no way to tick every row.
func TestDiscoveredTableSaysItsThingsInVisibleText(t *testing.T) {
	e, _ := moveFixture(t)
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	visible := reTitleAttr.ReplaceAllString(body, "")
	for _, want := range []string{"porch_plug", "Porch plug", "10.0.0.62"} {
		if !strings.Contains(visible, want) {
			t.Errorf("%q is only in a tooltip, or missing", want)
		}
	}
	boxes := reSelectAll.FindAllString(body, -1)
	if len(boxes) == 0 {
		t.Fatal("no select-all box")
	}
	for _, b := range boxes {
		if strings.Contains(b, "hidden") {
			t.Errorf("the select-all is hidden until a script runs: %s", b)
		}
	}
}

// Why: the scan result's select-all is revealed by the same script; the result table is the page's one
// way to tick several new devices.
func TestScanTableSelectAllIsVisible(t *testing.T) {
	e, _ := setupDiscovery(t)
	_, body := e.do(t, "GET", "/discovery/scan", "admin", nil)
	boxes := reSelectAll.FindAllString(body, -1)
	if len(boxes) == 0 {
		t.Fatal("no select-all box in the scan results")
	}
	for _, b := range boxes {
		if strings.Contains(b, "hidden") {
			t.Errorf("the select-all of the scan table is hidden: %s", b)
		}
	}
}

// Why (rubric a): the Apply button of an address change is now in the form of the new devices, which carries
// use_default_password when a fleet default password is set. The default password is for new devices: an address
// change must keep the stored password and never hand the fleet password to the new address. Real inventory,
// real discovery manager: a fake could not show what the stored device ends up with.
func TestApplyAnAddressChangeFromTheSharedFormKeepsTheStoredPassword(t *testing.T) {
	w := newWhyEnvWith(t, func(d *Deps) { d.DefaultDevicePassword = func() string { return "fleet-fake-pw" } })
	id, err := w.inv.Create(t.Context(), "t", model.Device{Name: "lamp", Address: "10.0.0.60", MAC: "02:00:5e:00:00:07", Topic: "lamp", HTTPPassword: "stored-pw"})
	if err != nil {
		t.Fatal(err)
	}
	w.net.macs["10.0.0.61"] = "02:00:5e:00:00:07"
	w.mgr.HandleMQTT(w.discoveryConfig("02:00:5e:00:00:07", "10.0.0.61", "lamp", "Lamp"))
	var p discovery.Proposal
	waitFor(t, "the address change to be proposed", func() bool {
		props, _ := w.mgr.Proposals(t.Context())
		for _, x := range props {
			if x.Kind == discovery.KindAddressChange && x.ExistingID == id {
				p = x
				return true
			}
		}
		return false
	})
	_, body := w.do(t, "GET", "/devices/new", "admin", nil)
	if !strings.Contains(reNewdevForm.FindString(body), `name="use_default_password" value="1"`) || !strings.Contains(body, ">New address<") {
		t.Fatal("premise: the address change shares the form that carries use_default_password")
	}
	// a device with a stored password asks for the explicit confirmation (the password goes to the new address)
	apply := "/discovery/proposals/" + strconv.FormatInt(p.ID, 10) + "/accept"
	_, ask := w.do(t, "POST", apply, "admin", url.Values{"version": {p.Version}, "use_default_password": {"1"}})
	r, out := w.do(t, "POST", apply, "admin", url.Values{"version": {p.Version}, "use_default_password": {"1"}, "token": {confirmToken(t, ask)}})
	if r.StatusCode != 303 {
		t.Fatalf("Apply: status %d: %s", r.StatusCode, out)
	}
	d, err := w.inv.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Address != "10.0.0.61" {
		t.Errorf("the device did not move: %s", d.Address)
	}
	if d.HTTPPassword != "stored-pw" {
		t.Errorf("the stored web password changed to %q: the fleet default must not follow an address change", d.HTTPPassword)
	}
}
