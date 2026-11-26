package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
)

// textOfID is the text of the one element with this id ("" when there is none or the id is repeated). The
// element must hold text only: its content ends at the first closing tag of its own name.
func textOfID(body, id string) string {
	attr := ` id="` + id + `"`
	if strings.Count(body, attr) != 1 {
		return ""
	}
	at := strings.Index(body, attr)
	open := strings.LastIndex(body[:at], "<")
	name := strings.Fields(strings.TrimPrefix(body[open:at], "<"))[0]
	rest := body[at:]
	rest = rest[strings.Index(rest, ">")+1:]
	end := strings.Index(rest, "</"+name+">")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(rest[:end], ""))
}

// powerButtons are the opening tags of the list's power switches.
func powerButtons(body string) []string {
	return regexp.MustCompile(`<button[^>]*class="pwr"[^>]*>`).FindAllString(body, -1)
}

// Why (#1074): a screen reader user who walks the Power column heard "Toggle relay 1, currently ON" sixty
// times: no name said which device a switch belongs to. A switch is named by its row (the device name in the
// Name cell, then "relay N"), says its state once (aria-checked, never in the name), and every id the name
// points to exists exactly once on the page, although the table row and the phone card share one DOM.
func TestListPowerSwitchIsNamedByItsRow(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), Power: map[string]string{"POWER1": "ON", "POWER2": "OFF"}}
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	var btns []string
	for _, b := range powerButtons(body) {
		if strings.Contains(b, `"dvn-1 `) { // the other device has no known relay: a disabled switch of its own
			btns = append(btns, b)
		}
	}
	if len(btns) != 4 { // two relays, in the table row and on the phone card
		t.Fatalf("want 4 power switches of device 1 (2 relays, row and card), got %d: %v", len(btns), btns)
	}
	for _, b := range btns {
		for _, want := range []string{`role="switch"`, `aria-checked="`, `aria-labelledby="dvn-1 rly-`} {
			if !strings.Contains(b, want) {
				t.Errorf("switch %s: missing %s", b, want)
			}
		}
		if strings.Contains(b, "aria-pressed") || strings.Contains(b, "aria-label=") || strings.Contains(b, "currently") {
			t.Errorf("switch %s: the state is aria-checked only and the name comes from its row", b)
		}
	}
	if !strings.Contains(btns[0], `aria-checked="true"`) || !strings.Contains(btns[1], `aria-checked="false"`) {
		t.Errorf("aria-checked must follow the relay state: %v", btns[:2])
	}
	if got := textOfID(body, "dvn-1"); got != "kitchen" {
		t.Errorf(`id dvn-1 must be the device name once, got %q`, got)
	}
	for n, want := range map[string]string{"1": "relay 1", "2": "relay 2"} {
		if got := textOfID(body, "rly-"+n); got != want {
			t.Errorf("id rly-%s: %q, want %q", n, got, want)
		}
	}
	for _, id := range regexp.MustCompile(` id="((?:dvn|rly)-\d+)"`).FindAllStringSubmatch(body, -1) {
		if strings.Count(body, ` id="`+id[1]+`"`) != 1 {
			t.Errorf("id %s is not unique on the page", id[1])
		}
	}
}

// Why (#1074): a device whose relays are not known yet still has its (disabled) switch; it is named by the
// row as well, with "power" for the device since there is no relay number to say.
func TestListPowerSwitchWithoutKnownRelaysIsNamedToo(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	btns := powerButtons(body)
	if len(btns) == 0 {
		t.Fatal("no power switch on the page")
	}
	for _, b := range btns {
		if !strings.Contains(b, `role="switch"`) || !strings.Contains(b, `aria-labelledby="dvn-`) || !strings.Contains(b, ` rly-0"`) {
			t.Errorf("switch of a device without known relays: %s", b)
		}
	}
	if got := textOfID(body, "rly-0"); got != "power" {
		t.Errorf("id rly-0: %q, want power", got)
	}
}

// Why (#1074): every health event re-renders the power cell for the live stream (about 90 events a second on a
// 60-device list). That fragment knows only the device id, so the name must come from ids the page holds, not
// from a device lookup per event: the fragment points at the same ids as the page, carries no device name and
// no id of its own (the row and the card both receive it, so an id would be repeated).
func TestPowerFragmentOfAnEventNamesTheSwitchThroughThePageIDs(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	body := readSSE(t, e, "power-1", func() {
		e.hub.Publish(events.Event{Topic: events.TopicHealth, Payload: model.Health{DeviceID: 1, State: model.HealthOnline, Power: map[string]string{"POWER1": "ON"}}})
	})
	frag := body[strings.Index(body, "event: power-1"):]
	frag = frag[:strings.Index(frag, "\n\n")]
	for _, want := range []string{`role="switch"`, `aria-checked="true"`, `aria-labelledby="dvn-1 rly-1"`} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment lacks %s: %s", want, frag)
		}
	}
	if strings.Contains(frag, "kitchen") || strings.Contains(frag, ` id=`) || strings.Contains(frag, "aria-pressed") {
		t.Errorf("the fragment must carry no device name, no id and no aria-pressed: %s", frag)
	}
}

// Why (#1074): the POST answer of a toggle is the same cell; it keeps the name of its row.
func TestPowerToggleAnswerKeepsTheRowName(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{state: "ON"})
	_, body := e.do(t, "POST", "/devices/1/power/1", "operator", map[string][]string{"action": {"TOGGLE"}}, "HX-Request", "true")
	if !strings.Contains(body, `aria-labelledby="dvn-1 rly-1"`) || !strings.Contains(body, `role="switch"`) {
		t.Errorf("answer: %s", body)
	}
}

// Why (#1074): role="menu" and "menuitem" promise arrow-key navigation and roving focus; menu.js has only
// Escape and click-outside. A screen reader then switches to its forms mode and the keys do nothing. A row menu
// is a plain disclosure with links and buttons, so no template may claim the role.
func TestNoTemplateClaimsAMenuRole(t *testing.T) {
	var found []string
	for _, dir := range []string{".", "ui"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.templ"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			src, err := os.ReadFile(f) // #nosec G304 -- the repository's own templates
			if err != nil {
				t.Fatal(err)
			}
			for i, l := range strings.Split(string(src), "\n") {
				if strings.Contains(l, `role="menu"`) || strings.Contains(l, `role="menuitem"`) {
					found = append(found, f+" line "+strconv.Itoa(i+1))
				}
			}
		}
	}
	if len(found) > 0 {
		t.Errorf("menu roles without a menu keyboard model: %v", found)
	}
}

// Why (#1074): 60 rows each had a trigger named "Actions"; the device name sat in the title only, which a
// screen reader does not read as the name. The trigger names its row, in the table row and on the card.
func TestDeviceRowMenuTriggerNamesTheDevice(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	for _, name := range []string{"kitchen", "attic"} {
		if n := strings.Count(body, `<summary class="btn btn-ghost btn-icon icon" aria-label="Actions for `+name+`"`); n != 2 {
			t.Errorf("device %s: %d triggers named for it, want 2 (row and card)", name, n)
		}
	}
	if strings.Contains(body, `aria-label="Actions"`) {
		t.Error("a trigger named just Actions")
	}
}

// Why (#1074): the users list named every trigger "User actions".
func TestUserRowMenuTriggerNamesTheUser(t *testing.T) {
	body := renderUsersPage(t, usersFixture())
	for _, who := range []string{"ops", "Sam"} {
		if !strings.Contains(menuTriggerOf(t, body, who), `aria-label="Actions for `+who+`"`) {
			t.Errorf("the menu of %s: %s", who, menuTriggerOf(t, body, who))
		}
	}
	if strings.Contains(body, "User actions") {
		t.Error("a trigger named User actions")
	}
}

// menuTriggerOf is the summary of the "⋯" menu of the row that names who.
func menuTriggerOf(t *testing.T, body, who string) string {
	t.Helper()
	m := menuOf(t, body, who)
	return m[:strings.Index(m, "</summary>")]
}

// Why (#1074): "Add" and "Ignore" five times on the dashboard: a screen reader says nothing of what is added.
// The buttons keep their visible verb and name the device behind it ("Add Shed Light"), which starts with
// the visible text as label-in-name asks.
func TestAttentionButtonsNameTheirDevice(t *testing.T) {
	e, _ := dashFleet(t, nil)
	_, body := e.do(t, "GET", "/", "admin", nil)
	att := between(body, `<h2>Needs attention`, `</section>`)
	for _, want := range []string{`aria-label="Add Shed Light"`, `aria-label="Ignore Shed Light"`, `aria-label="Apply kitchen"`, `aria-label="Ignore kitchen"`} {
		if !strings.Contains(att, want) {
			t.Errorf("needs attention lacks %s in %s", want, att)
		}
	}
}

// Why (#1074): "Details" on every audit row: 25 buttons with one name. The summary says what it opens and
// when (the visible word stays "Details"), and says which row it controls; the time is a <time> so time.js
// writes it in the reader's zone like the When column.
func TestAuditDetailsNameTheirEventAndControlTheirRow(t *testing.T) {
	e := setup(t)
	for i := 0; i < 3; i++ {
		e.aud.events = append(e.aud.events, model.AuditEvent{ID: int64(i), At: time.Date(2026, 10, 19, 10, 0, i, 0, time.UTC),
			Actor: "adam", Action: "command.send", Target: "device:1", Result: "ok", Detail: `{"cmd":"Status 2"}`})
	}
	_, body := e.do(t, "GET", "/audit?since=all", "admin", nil)
	sums := regexp.MustCompile(`<summary class="btn btn-sm"[^>]*>.*?</summary>`).FindAllString(body, -1)
	if len(sums) != 3 {
		t.Fatalf("want 3 Details summaries, got %d in %s", len(sums), body)
	}
	for _, s := range sums {
		m := regexp.MustCompile(`aria-controls="([^"]+)"`).FindStringSubmatch(s)
		if m == nil || strings.Count(body, `<tr class="detail-row" id="`+m[1]+`"`) != 1 {
			t.Errorf("the summary must control the detail row that holds its content: %s", s)
		}
		if !strings.Contains(s, `>Details<span class="sr-only"> of `) || !strings.Contains(s, ` (kitchen) at <time `) {
			t.Errorf("the summary names its event and time after the visible word: %s", s)
		}
		if strings.Contains(s, "aria-expanded") {
			t.Errorf("details reports its own state; a static aria-expanded goes stale: %s", s)
		}
	}
}

// Why (#1074): "Use as reference" about twenty times on the templates page: the link list was unusable.
func TestUseAsReferenceNamesTheDevice(t *testing.T) {
	f := gtFixture()
	f.ov.Groups[0].Reference, f.ov.Groups[0].NeedsReference = nil, true
	for i := range f.ov.Groups[0].Devices {
		f.ov.Groups[0].Devices[i].Plan = nil
	}
	e := gtEnv(t, f)
	_, body := e.do(t, "GET", "/policy/templates", "admin", nil)
	for _, name := range []string{"tv-plug", "router-plug", "balcony-plug"} {
		if !strings.Contains(body, `>Use as reference<span class="sr-only"> `+name+`</span></a>`) {
			t.Errorf("Use as reference for %s lacks its hidden device name", name)
		}
	}
}

// Why (#1074): the change note had only a placeholder, which is gone as soon as someone types, and password
// managers filled it. It has a visible label. The inputs that are not credentials say autocomplete=off.
func TestRuleEditChangeNoteHasAVisibleLabelAndNoAutofill(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	_, body := e.do(t, "GET", "/policy/rules/new?command=SaveData", "admin", nil)
	if !strings.Contains(body, `Note for History (optional)`) {
		t.Error("no visible label for the change note")
	}
	for _, name := range []string{"name", "note", "change_note"} {
		re := regexp.MustCompile(`<(?:input|textarea)[^>]*\bname="` + name + `"[^>]*>`)
		tag := re.FindString(body)
		if tag == "" || !strings.Contains(tag, `autocomplete="off"`) {
			t.Errorf("%s: %q lacks autocomplete=off", name, tag)
		}
	}
	if m := regexp.MustCompile(`<input[^>]*\bname="want"[^>]*>`).FindString(body); m != "" && !strings.Contains(m, `autocomplete="off"`) {
		t.Errorf("the expected value input lacks autocomplete=off: %s", m)
	}
}

// Why (#1074): the first letter of a username typed on a phone was capitalised and spell-checked, a classic
// cause of "Wrong username or password" on this very form. "Keep me signed in" was ticked on a page that is used
// on shared screens: it is the person's choice, unticked until they make it, and kept after a failed attempt.
func TestSignInFieldsAndRememberDefault(t *testing.T) {
	p := NewPages()
	render := func(d auth.LoginData) string {
		w := httptest.NewRecorder()
		p.Login(w, httptest.NewRequest(http.MethodGet, "/login", nil), 200, d)
		return w.Body.String()
	}
	remember := func(body string) string {
		return regexp.MustCompile(`<input[^>]*name="remember"[^>]*>`).FindString(body)
	}
	first := render(auth.LoginData{Local: true})
	if r := remember(first); r == "" || strings.Contains(r, "checked") {
		t.Errorf("the first view must not tick Keep me signed in: %q", r)
	}
	user := regexp.MustCompile(`<input[^>]*name="username"[^>]*>`).FindString(first)
	if !strings.Contains(user, `autocapitalize="off"`) || !strings.Contains(user, `spellcheck="false"`) {
		t.Errorf("the username field: %s", user)
	}
	failed := render(auth.LoginData{Local: true, Error: "Wrong username or password.", Username: "maren", Remember: true})
	if r := remember(failed); !strings.Contains(r, "checked") {
		t.Errorf("a failed attempt keeps the ticked choice: %q", r)
	}
	failedOff := render(auth.LoginData{Local: true, Error: "Wrong username or password.", Username: "maren"})
	if r := remember(failedOff); strings.Contains(r, "checked") {
		t.Errorf("a failed attempt with the box unticked must not tick it: %q", r)
	}
}

// Why (#1074): every deviating device of a group has a "Pin table" disclosure with the same words; its
// summary says which device it belongs to.
func TestPinTableSummaryNamesItsDevice(t *testing.T) {
	e := gtEnv(t, gtFixture())
	_, body := e.do(t, "GET", "/policy/templates", "admin", nil)
	if !strings.Contains(body, `<span class="sr-only"> (balcony-plug)</span></summary>`) {
		t.Error("the pin table of balcony-plug does not name its device")
	}
}
