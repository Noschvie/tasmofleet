package web

import (
	"context"
	"errors"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/transport"
)

var pwNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// Why (#609, "the device page says which transfers the password enables"): one row says whether the device takes
// the password (a word), where it came from and when (a line), and what it is for. Before, the row repeated the
// stored mode ("Same as the broker's", "Set") and never said whether the device accepted it, so a wrong password
// looked as good as a right one until a transfer failed. The seven states and their precedence are one function,
// which the card on System, MQTT uses to count, so the page and the card cannot disagree.
func TestPasswordRowNamesStateSourceAndWhatItEnables(t *testing.T) {
	tested, taken, bad := pwNow.Add(-time.Hour), pwNow.Add(-9*time.Hour), pwNow.Add(-time.Minute)
	for _, tc := range []struct {
		name    string
		in      pwInput
		learnOn bool
		word    string
		tone    string
		has     []string
		not     []string
	}{
		{"another broker", pwInput{Mode: model.MQTTPasswordBroker}, true, "Not needed", "neutral", []string{"does not report to TasmoFleet's broker"}, []string{"Lets TasmoFleet"}},
		{"unusable", pwInput{Ours: true, Mode: model.MQTTPasswordBroker, Unusable: "TasmoFleet has no broker password configured"}, true, "Unusable", "warn", []string{"no broker password"}, nil},
		{"refused", pwInput{Ours: true, Mode: model.MQTTPasswordOwn, BadAt: bad, OKAt: tested}, true, "Refused", "danger", []string{"refused it", "backups and restores over MQTT are off", "No password"}, []string{"accepted"}},
		{"tested", pwInput{Ours: true, Mode: model.MQTTPasswordBroker, OKAt: tested}, true, "Works", "ok", []string{"Same as TasmoFleet's broker password", "Tested", "the device accepted it", "back up, restore, read files and upload firmware over MQTT"}, nil},
		{"from a backup", pwInput{Ours: true, Mode: model.MQTTPasswordOwn, Source: model.MQTTPasswordFromBackup, SetAt: taken}, true, "Works", "ok", []string{"own password, stored encrypted", "taken from the backup of"}, []string{"Tested"}},
		{"typed, never accepted", pwInput{Ours: true, Mode: model.MQTTPasswordOwn, SetAt: taken}, true, "Not tested", "neutral", []string{"Set", "has not accepted it yet", "press Test"}, []string{"accepted it."}},
		{"no password", pwInput{Ours: true, Mode: model.MQTTPasswordEmpty, SetAt: taken}, true, "Not tested", "neutral", []string{"No password (TasmoFleet sends an empty one)"}, []string{"The device has no MQTT password"}},
		// The mode is TasmoFleet's setting, not a fact about the device, and a refusal under it must not advise the mode already set.
		{"refused with no password", pwInput{Ours: true, Mode: model.MQTTPasswordEmpty, BadAt: bad}, true, "Refused", "danger", []string{"refused it", "Enter the device's own password."}, []string{"choose No password", "The device has no MQTT password"}},
		{"unset, backups on", pwInput{Ours: true}, true, "Not known", "warn", []string{"takes the password from a backup", "Needed to back up, restore"}, []string{"Lets TasmoFleet"}},
		{"unset, backups off", pwInput{Ours: true}, false, "Not set", "neutral", []string{"turned off", "Needed to back up, restore"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mqttFacts(pwNow, tc.in.Mode)
			f.MQTT.Ours = tc.in.Ours
			f.Password = transport.Password{Mode: tc.in.Mode, BadAt: tc.in.BadAt, Unusable: tc.in.Unusable}
			d := model.Device{MQTTPasswordSource: tc.in.Source, MQTTPasswordSetAt: tc.in.SetAt, MQTTPasswordOKAt: tc.in.OKAt, MQTTPassword: "never-shown-pw"}
			r := passwordRow(d, f, pwView{LearnOff: !tc.learnOn})
			text := r.Sub + r.StampLead + r.StampTail + " " + r.Enables
			if r.Value != tc.word || string(r.Tone) != tc.tone {
				t.Fatalf("row %q/%q, want %q/%q", r.Value, r.Tone, tc.word, tc.tone)
			}
			for _, w := range tc.has {
				if !strings.Contains(text, w) {
					t.Errorf("the line lacks %q: %q", w, text)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(text, w) {
					t.Errorf("the line holds %q: %q", w, text)
				}
			}
			if strings.Contains(text, "never-shown-pw") {
				t.Fatal("the line holds the password")
			}
		})
	}
}

// fakePWTester is the router's password test: it answers err and records which device was tested.
type fakePWTester struct {
	mu    sync.Mutex
	err   error
	calls []model.DeviceID
}

func (f *fakePWTester) TestMQTTPassword(_ context.Context, id model.DeviceID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	return f.err
}

// pwEnv is an env with three devices on the broker (one typed and tested, one refused, one unset) and one on another broker.
func pwEnv(t *testing.T, tester *fakePWTester, fleet *fakePWFleet) (*env, *fakeAudit) {
	t.Helper()
	facts := fakeFacts{f: map[model.DeviceID]transport.Facts{}}
	aud := &fakeAudit{}
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) {
		inv := d.Inventory.(*fakeInv)
		inv.devs[11] = model.Device{ID: 11, Name: "porch", Address: "10.0.0.11", MQTTPasswordMode: model.MQTTPasswordOwn, MQTTPassword: "typed-secret-4711", MQTTPasswordSetAt: pwNow.Add(-2 * time.Hour), MQTTPasswordOKAt: pwNow.Add(-time.Hour)}
		inv.devs[12] = model.Device{ID: 12, Name: "garage", Address: "10.0.0.12"}
		inv.devs[13] = model.Device{ID: 13, Name: "shed", Address: "10.0.0.13"}
		inv.devs[14] = model.Device{ID: 14, Name: "pool", Address: "10.0.0.14", MQTTPasswordMode: model.MQTTPasswordBroker, MQTTPasswordSource: model.MQTTPasswordFromBackup, MQTTPasswordSetAt: pwNow.Add(-9 * time.Hour)}
		inv.devs[15] = model.Device{ID: 15, Name: "zigbee", Address: "10.0.0.15"}
		inv.devs[16] = model.Device{ID: 16, Name: "wrong", Address: "10.0.0.16", MQTTPasswordMode: model.MQTTPasswordOwn, MQTTPassword: "typed-secret-4716", MQTTPasswordSetAt: pwNow.Add(-5 * time.Hour), MQTTPasswordBadAt: pwNow.Add(-time.Minute)}
		for id, dev := range inv.devs {
			if id < 11 {
				continue
			}
			f := mqttFacts(pwNow, dev.MQTTPasswordMode)
			f.Password.BadAt = dev.MQTTPasswordBadAt
			f.MQTT.Ours = id != 15
			f.MQTT.Topic = strings.ToLower(dev.Name)
			facts.f[id] = f
		}
		fleet.inv = inv
		d.Transport = facts
		d.PasswordTest = tester
		d.PasswordFleet = fleet
		d.MQTTPasswords = &fakeMQTTPW{inv: inv}
		d.Recorder = aud
	})
	return e, aud
}

// fakePWFleet is the fleet service on the fake inventory: the reasons it was given, and a bulk set that, like the
// store, writes only devices that are still not set.
type fakePWFleet struct {
	mu      sync.Mutex
	inv     *fakeInv
	gaps    map[model.DeviceID]PasswordGap
	calls   [][]model.DeviceID
	tested  bool
	testing int
	before  func() // runs at the start of UseBroker: a password that arrives between the confirmation and the write
}

func (f *fakePWFleet) Gaps(_ context.Context, ids []model.DeviceID) map[model.DeviceID]PasswordGap {
	out := map[model.DeviceID]PasswordGap{}
	for _, id := range ids {
		out[id] = f.gaps[id]
	}
	return out
}

func (f *fakePWFleet) UseBroker(_ context.Context, _ string, ids []model.DeviceID, test bool) ([]model.DeviceID, error) {
	if f.before != nil {
		f.before()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ids)
	f.tested = test
	var applied []model.DeviceID
	f.inv.mu.Lock()
	defer f.inv.mu.Unlock()
	for _, id := range ids {
		if d := f.inv.devs[id]; d.MQTTPasswordMode == model.MQTTPasswordNone {
			d.MQTTPasswordMode = model.MQTTPasswordBroker
			f.inv.devs[id] = d
			applied = append(applied, id)
		}
	}
	return applied, nil
}

func (f *fakePWFleet) Testing() int { return f.testing }

// Why (#609): Test is the only way to learn that a typed password is right before a backup needs it. It is an
// operator's action like Back up (it changes nothing on the device); the result goes to the audit as a word and
// never as the password, a refusal is shown as such, and a test that could not run says why instead of "refused"
// (which would send the person to change a password that was never checked).
func TestPasswordTestRouteRecordsResultWithoutTheValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		outcome string
		want    string // in the redirect, or in the page of a test that did not run
	}{
		{"accepted", nil, "accepted", "ok=pw-accepted"},
		{"refused", mqttcmd.ErrPasswordRefused, "refused", "ok=pw-refused"},
		{"blocked", &transport.PasswordTestBlockedError{Reason: "the device is not online on MQTT"}, "blocked", "The device is not online on MQTT."},
		{"no answer", errors.New("mqttcmd: timeout with typed-secret-4711 in it"), "error", "Nothing says the password is wrong"},
		{"busy", context.DeadlineExceeded, "error", "busy with another transfer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tester := &fakePWTester{err: tc.err}
			e, aud := pwEnv(t, tester, &fakePWFleet{})
			res, body := e.do(t, "POST", "/devices/11/mqtt-password/test", "operator", url.Values{"csrf": {"tok"}})
			if !slices.Equal(tester.calls, []model.DeviceID{11}) {
				t.Fatalf("tested %v", tester.calls)
			}
			seen := body + res.Header.Get("Location")
			if !strings.Contains(seen, tc.want) {
				t.Fatalf("status %d, location %q, body lacks %q:\n%s", res.StatusCode, res.Header.Get("Location"), tc.want, body)
			}
			var found bool
			for _, ev := range aud.events {
				if ev.Action == "device.mqtt_password_test" {
					found = true
					if ev.Target != "device:11" || ev.Actor != "test:operator" || !strings.Contains(ev.Detail, `"result":"`+tc.outcome+`"`) {
						t.Errorf("audit %+v", ev)
					}
				}
				if strings.Contains(ev.Detail+ev.Target+ev.Result, "typed-secret-4711") {
					t.Errorf("the audit holds the password: %+v", ev)
				}
			}
			if !found {
				t.Fatal("no audit entry for the test")
			}
			if strings.Contains(body, "typed-secret-4711") {
				t.Fatal("the answer holds the password")
			}
		})
	}
	e, _ := pwEnv(t, &fakePWTester{}, &fakePWFleet{})
	if res, _ := e.do(t, "POST", "/devices/11/mqtt-password/test", "", url.Values{"csrf": {"tok"}}); res.StatusCode != 401 {
		t.Errorf("without a session: %d", res.StatusCode)
	}
}

var sumRe = regexp.MustCompile(`<div class="sys-sum-label">([^<]*)</div>\s*<div class="sys-sum-value">([^<]*)</div>`)

func sumsOf(page string) map[string]string {
	out := map[string]string{}
	for _, m := range sumRe.FindAllStringSubmatch(page, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// Why (#609, "ask only for the devices that have none"): the card counts the fleet by the same words the device
// pages use, and lists the devices TasmoFleet has no password for, each with the reason (so it is no riddle) and a
// Set… link to enter it. The button that gives them the broker password needs a broker password; without one it is
// disabled and says why. No value of any password appears on the page.
func TestMQTTCardListsDevicesWithoutUsableBackupWithReason(t *testing.T) {
	fleet := &fakePWFleet{gaps: map[model.DeviceID]PasswordGap{12: GapNoBackup, 13: GapDmpOnly}}
	e, _ := pwEnv(t, &fakePWTester{}, fleet)
	_, page := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	sums := sumsOf(page)
	for label, want := range map[string]string{"On this broker": "5", "From backups": "1", "Entered": "2", "Not known": "2"} {
		if sums[label] != want {
			t.Errorf("%s = %q, want %q (sums %v)", label, sums[label], want, sums)
		}
	}
	for _, want := range []string{"Not known (2)", "garage", "shed", mqttOnlyGap, GapDmpOnly.Text(), `href="/tf/devices/12/edit"`, `href="/tf/devices/13/edit"`,
		"Use the broker password for 2 devices…", `data-dialog="dlg-pwbroker"`, `id="dlg-pwbroker"`, "1 refused"} {
		if !strings.Contains(page, want) {
			t.Errorf("the card lacks %q", want)
		}
	}
	for _, not := range []string{"typed-secret-4711", "typed-secret-4716", "fake-stored-pw", "zigbee", "read after the next backup"} {
		if strings.Contains(page, not) {
			t.Errorf("the page holds %q", not)
		}
	}
	dlg := page[strings.Index(page, `id="dlg-pwbroker"`):]
	dlg = dlg[:strings.Index(dlg, "</dialog>")]
	if strings.Count(dlg, `name="id"`) != 2 || strings.Count(dlg, "checked") < 3 || !strings.Contains(dlg, `name="test" value="1" checked`) {
		t.Errorf("the dialog does not list both devices ticked with the test box on:\n%s", dlg)
	}
	if strings.Index(page, `id="dlg-pwbroker"`) < strings.Index(page, "</form>") {
		t.Error("the dialog sits inside the settings form: its fields would be posted with a settings save")
	}
	fleet.testing = 3
	if _, page = e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil); !strings.Contains(page, "Testing 3 devices") {
		t.Error("a busy test run is not shown")
	}
}

// Why (#609, acceptance "several devices in one step"): the bulk action cuts the posted devices to those the card
// lists now. A password that a backup taught, or a person typed, between the dialog and the confirm is a newer fact
// and must not be overwritten; the audit names what was written and what was left out, and never a password. A
// post without the confirmation, or for a device that is not on the list, changes nothing.
func TestBulkBrokerPasswordSkipsDevicesLearnedMeanwhile(t *testing.T) {
	fleet := &fakePWFleet{gaps: map[model.DeviceID]PasswordGap{}}
	e, aud := pwEnv(t, &fakePWTester{}, fleet)
	fleet.before = func() { // the learner takes 13's password after the page was checked, before the write
		fleet.inv.mu.Lock()
		d := fleet.inv.devs[13]
		d.MQTTPasswordMode = model.MQTTPasswordOwn
		fleet.inv.devs[13] = d
		fleet.inv.mu.Unlock()
	}
	form := url.Values{"csrf": {"tok"}, "confirm": {"1"}, "test": {"1"}, "id": {"12", "13", "14", "99"}}
	if res, _ := e.do(t, "POST", "/settings/system/mqtt-passwords/broker", "operator", form); res.StatusCode != 403 {
		t.Fatalf("an operator may not give the broker password to devices: %d", res.StatusCode)
	}
	if res, _ := e.do(t, "POST", "/settings/system/mqtt-passwords/broker", "admin", url.Values{"csrf": {"tok"}, "id": {"12"}}); res.StatusCode != 422 || len(fleet.calls) != 0 {
		t.Fatalf("without confirm=1: %d, calls %v", res.StatusCode, fleet.calls)
	}
	res, _ := e.do(t, "POST", "/settings/system/mqtt-passwords/broker", "admin", form)
	loc := res.Header.Get("Location")
	if res.StatusCode != 303 || !strings.Contains(loc, "ok=pw-bulk") || !strings.Contains(loc, "n=1") || !strings.Contains(loc, "s=3") {
		t.Fatalf("status %d, location %q", res.StatusCode, loc)
	}
	if len(fleet.calls) != 1 || !slices.Equal(fleet.calls[0], []model.DeviceID{12, 13}) || !fleet.tested {
		t.Fatalf("the fleet got %v (tested %v): only the devices the card lists now (12 and 13), with the test box", fleet.calls, fleet.tested)
	}
	var n int
	for _, ev := range aud.events {
		if ev.Action != "device.mqtt_password_bulk" {
			continue
		}
		n++
		for _, want := range []string{`"mode":"broker"`, `"applied":[12]`, `"skipped":[`, `"test":true`} {
			if !strings.Contains(ev.Detail, want) {
				t.Errorf("audit %q lacks %q", ev.Detail, want)
			}
		}
		if strings.Contains(ev.Detail, "typed-secret") || strings.Contains(ev.Detail, "fake-stored-pw") {
			t.Errorf("audit holds a password: %q", ev.Detail)
		}
	}
	if n != 1 {
		t.Fatalf("%d audit entries for one bulk action, want 1", n)
	}
	if d := fleet.inv.devs[14]; d.MQTTPasswordMode != model.MQTTPasswordBroker || d.MQTTPasswordSource != model.MQTTPasswordFromBackup {
		t.Errorf("a device that was never on the list changed: %+v", d)
	}
}

// Why (#609, T18): the confirmation page for browsers without scripts is the twin of the dialog and starts tests; a
// test of an accepted password puts a chunk of the device's settings on the broker, so the page carries the broker
// ACL notice like every page that starts such a transfer, and shows the same ticked list and the test box.
func TestBulkBrokerPasswordConfirmPageNeedsNoScriptsAndCarriesTheACLNotice(t *testing.T) {
	fleet := &fakePWFleet{}
	e, _ := pwEnv(t, &fakePWTester{}, fleet)
	res, page := e.do(t, "GET", "/settings/system/mqtt-passwords/broker", "admin", nil)
	if res.StatusCode != 200 || strings.Count(page, `name="id"`) != 2 || !strings.Contains(page, `name="confirm" value="1"`) || !strings.Contains(page, `name="test" value="1" checked`) {
		t.Fatalf("status %d:\n%s", res.StatusCode, page)
	}
	if n := countACL(t, page); n != 1 {
		t.Errorf("%d ACL notices, want 1", n)
	}
	// The test publishes TasmoFleet's own login where a device's own account may read it: the page says so in its body and
	// in the notice, and no longer says that nothing but the test is sent.
	text := html.UnescapeString(page)
	if strings.Count(text, pwBrokerTestWarning) != 2 || strings.Contains(text, "nothing is sent to a device except the test") {
		t.Errorf("the page must carry the warning in its body and in the ACL notice (%d times):\n%s", strings.Count(text, pwBrokerTestWarning), text)
	}
	_, tab := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if !strings.Contains(html.UnescapeString(tab), pwBrokerTestWarning) || strings.Contains(html.UnescapeString(tab), "nothing is sent to a device except the test") {
		t.Error("the dialog on System, MQTT does not carry the warning")
	}
	if _, one := e.do(t, "GET", "/settings/system/mqtt-passwords/broker?id=13", "admin", nil); strings.Count(one, `name="id"`) != 1 || !strings.Contains(one, `value="13"`) {
		t.Errorf("?id=13 does not limit the page to that device")
	}
	if len(fleet.calls) != 0 {
		t.Fatal("opening the confirmation changed something")
	}
}

// Why (#609, B3): the device page offers what the state calls for, to whom it may: an operator tests, an admin also
// changes (or enters, or gives the broker password to a device with none); Test is disabled with the reason when the
// strict rule of transfers says no, and not offered where the device needs no password at all.
func TestDevicePageOffersTestAndChangeByRole(t *testing.T) {
	e, _ := pwEnv(t, &fakePWTester{}, &fakePWFleet{gaps: map[model.DeviceID]PasswordGap{12: GapNoBackup}})
	_, op := e.do(t, "GET", "/devices/11", "operator", nil)
	if !strings.Contains(op, `action="/tf/devices/11/mqtt-password/test"`) || strings.Contains(op, "Change…") {
		t.Errorf("operator: Test yes, Change… no")
	}
	_, adm := e.do(t, "GET", "/devices/11", "admin", nil)
	if !strings.Contains(adm, `href="/tf/devices/11/edit"`) || !strings.Contains(adm, "Change…") {
		t.Errorf("admin: Change… missing")
	}
	_, unk := e.do(t, "GET", "/devices/12", "admin", nil)
	for _, want := range []string{"Not known", mqttOnlyGap, "Enter…", "Use the broker password…", "/settings/system/mqtt-passwords/broker?id=12"} {
		if !strings.Contains(unk, want) {
			t.Errorf("unset device: page lacks %q", want)
		}
	}
	_, gone := e.do(t, "GET", "/devices/15", "admin", nil)
	if !strings.Contains(gone, "Not needed") || strings.Contains(gone, "mqtt-password/test") {
		t.Errorf("a device on another broker: Not needed, and no Test")
	}
	for _, page := range []string{op, adm, unk, gone} {
		if strings.Contains(page, "typed-secret") || strings.Contains(page, "fake-stored-pw") {
			t.Fatal("a password reached the device page")
		}
	}
}

// mqttOnlyGap is what the card and the device row say of a device without a backup whose HTTP fails (the facts of pwEnv).
const mqttOnlyGap = "TasmoFleet reaches it only over MQTT, so no backup can bring its password: enter it, or use the broker password."

// Why (#609, mockup B1/B3): the main group the card is for is the MQTT-only devices. For them "It is read after the next
// backup" promises what cannot happen: the backup over MQTT needs the password first and HTTP fails. The reason must
// say that, and only for such devices; a device that HTTP reaches still gets the learner's sentence.
func TestAGapOfAnMQTTOnlyDeviceSaysNoBackupCanBringThePassword(t *testing.T) {
	failing := mqttFacts(pwNow, model.MQTTPasswordNone)
	working := failing
	working.HTTP = transport.HTTPState{State: transport.HTTPOK}
	if got := gapText(GapNoBackup, failing); got != mqttOnlyGap {
		t.Errorf("HTTP failing: %q", got)
	}
	if got := gapText(GapNoBackup, working); got != GapNoBackup.Text() || !strings.Contains(got, "next backup") {
		t.Errorf("HTTP working: %q", got)
	}
	if got := gapText(GapDmpOnly, failing); got != GapDmpOnly.Text() {
		t.Errorf("another reason must stay as the learner said it: %q", got)
	}
}

// Why (#609, T18): the Test button publishes the FileDownload start with the password and, when the device accepts it,
// one chunk of the settings dump through the broker. The header's notice follows the backup path, the button a looser
// rule (a refusal does not block it), so a refused device whose backup goes over HTTP or nowhere showed the button
// and no notice. The notice must stand by the button exactly where the header's does not, so every page that can
// start the exposure carries one, and none carries two.
func TestDeviceTestButtonCarriesTheACLNoticeWhenTheBackupIsNotOverMQTT(t *testing.T) {
	e, _ := pwEnv(t, &fakePWTester{}, &fakePWFleet{gaps: map[model.DeviceID]PasswordGap{}})
	_, refused := e.do(t, "GET", "/devices/16", "operator", nil) // refused: the backup is not over MQTT, Test again is pressable
	if !strings.Contains(refused, `action="/tf/devices/16/mqtt-password/test"`) || !strings.Contains(refused, "Test again") {
		t.Fatal("the refused device does not offer Test again")
	}
	if n := countACL(t, refused); n != 1 || !strings.Contains(html.UnescapeString(refused), aclTextPasswordTest) {
		t.Errorf("refused device: %d ACL notices, want 1 that names the password test", n)
	}
	_, works := e.do(t, "GET", "/devices/11", "operator", nil) // backup over MQTT: the header's notice covers the button
	if n := countACL(t, works); n != 1 || strings.Contains(html.UnescapeString(works), aclTextPasswordTest) {
		t.Errorf("device whose backup goes over MQTT: %d ACL notices, want only the Back up one", n)
	}
	_, unk := e.do(t, "GET", "/devices/12", "admin", nil) // a Test that is disabled puts nothing on the broker
	if strings.Contains(html.UnescapeString(unk), aclTextPasswordTest) {
		t.Error("a disabled Test carries the notice")
	}
}
