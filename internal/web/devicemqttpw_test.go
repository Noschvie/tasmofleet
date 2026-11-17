package web

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// fakeMQTTPW is store.SetMQTTPassword on the fake inventory: it records the calls and, like the
// store, drops the own password in the other modes and clears the refused mark.
type fakeMQTTPW struct {
	mu    sync.Mutex
	inv   *fakeInv
	calls []mqttCall
}

type mqttCall struct {
	id    model.DeviceID
	mode  model.MQTTPasswordMode
	plain string
}

func (f *fakeMQTTPW) SetMQTTPassword(_ context.Context, id model.DeviceID, mode model.MQTTPasswordMode, plain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, mqttCall{id, mode, plain})
	f.inv.mu.Lock()
	defer f.inv.mu.Unlock()
	d := f.inv.devs[id]
	d.MQTTPasswordMode, d.MQTTPassword, d.MQTTPasswordBadAt = mode, "", time.Time{}
	if mode == model.MQTTPasswordOwn {
		d.MQTTPassword = plain
	}
	f.inv.devs[id] = d
	return nil
}

func (f *fakeMQTTPW) got() []mqttCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mqttCall(nil), f.calls...)
}

const storedOwnPW = "stored-own-pw-4711"

func mqttEnv(t *testing.T, d model.Device) (*env, *fakeMQTTPW) {
	t.Helper()
	pw := &fakeMQTTPW{}
	e := setupWith(t, func(deps *Deps) {
		inv := deps.Inventory.(*fakeInv)
		inv.devs[d.ID] = d
		pw.inv = inv
		deps.MQTTPasswords = pw
	})
	return e, pw
}

func ownDevice() model.Device {
	return model.Device{ID: 7, Name: "porch", Address: "10.0.0.7", MQTTPasswordMode: model.MQTTPasswordOwn, MQTTPassword: storedOwnPW}
}

func editPost(extra url.Values) url.Values {
	v := url.Values{"csrf": {"tok"}, "name": {"porch"}, "address": {"10.0.0.7"}}
	for k, vals := range extra {
		v[k] = vals
	}
	return v
}

// Why: the password is write-only. It must not be in the HTML of the form, not on a first view,
// not after a rejected save, and not after typing a new one.
func TestEditFormNeverRendersTheMQTTPassword(t *testing.T) {
	d := ownDevice()
	d.MQTTPasswordBadAt = time.Now()
	e, _ := mqttEnv(t, d)
	_, body := e.do(t, "GET", "/devices/7/edit", "admin", nil)
	for _, want := range []string{`name="mqtt_password_mode"`, `value="own" selected`, `placeholder="Unchanged"`, "The device refused the stored MQTT password", `name="retry_mqtt_password"`} {
		if !strings.Contains(body, want) {
			t.Errorf("form lacks %q", want)
		}
	}
	if strings.Contains(body, storedOwnPW) {
		t.Fatal("the stored MQTT password is in the HTML")
	}
	for name, form := range map[string]url.Values{
		"bad port":     {"port": {"abc"}, "mqtt_password_mode": {"own"}, "mqtt_password": {"typed-pw-9876"}},
		"bad password": {"mqtt_password_mode": {"own"}, "mqtt_password": {`typed"pw-9876`}},
	} {
		res, body := e.do(t, "POST", "/devices/7", "admin", editPost(form))
		if res.StatusCode != 422 {
			t.Fatalf("%s: status %d", name, res.StatusCode)
		}
		if strings.Contains(body, "pw-9876") || strings.Contains(body, storedOwnPW) {
			t.Fatalf("%s: a rejected save echoes an MQTT password", name)
		}
	}
}

// Why: choosing "Own password" and typing one stores it; the audit entry says what happened
// without the value.
func TestEditFormStoresAnOwnPasswordAndAuditsWithoutIt(t *testing.T) {
	e, pw := mqttEnv(t, model.Device{ID: 7, Name: "porch", Address: "10.0.0.7"})
	res, _ := e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"mqtt_password_mode": {"own"}, "mqtt_password": {"new-pw-1"}}))
	if res.StatusCode != 303 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if c := pw.got(); len(c) != 1 || c[0].mode != model.MQTTPasswordOwn || c[0].plain != "new-pw-1" {
		t.Fatalf("calls %+v", c)
	}
	var found bool
	for _, ev := range e.aud.events {
		if ev.Action == "device.mqtt_password" {
			found = true
			if strings.Contains(ev.Detail, "new-pw-1") || !strings.Contains(ev.Detail, `"mode":"own"`) {
				t.Fatalf("audit detail %s", ev.Detail)
			}
		}
	}
	if !found {
		t.Fatal("no audit entry")
	}
}

// Why: blank means "keep" (like the web password), so editing notes must not wipe or rewrite
// the stored password; with nothing to change no write happens at all.
func TestBlankKeepsTheStoredOwnPassword(t *testing.T) {
	e, pw := mqttEnv(t, ownDevice())
	res, _ := e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"notes": {"changed"}, "mqtt_password_mode": {"own"}, "mqtt_password": {""}}))
	if res.StatusCode != 303 || len(pw.got()) != 0 {
		t.Fatalf("status %d, calls %+v", res.StatusCode, pw.got())
	}
	if got := e.inv.devs[7]; got.MQTTPassword != storedOwnPW || got.Notes != "changed" {
		t.Fatalf("stored %+v", got)
	}
}

// Why: the refused mark is cleared only when the section was touched or "Retry the stored
// password" is ticked; an unrelated save must not re-seal, audit or clear it.
func TestRefusedMarkIsClearedOnlyByATouchedSection(t *testing.T) {
	d := ownDevice()
	d.MQTTPasswordBadAt = time.Now()
	e, pw := mqttEnv(t, d)
	e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"notes": {"x"}, "mqtt_password_mode": {"own"}}))
	if len(pw.got()) != 0 || e.inv.devs[7].MQTTPasswordBadAt.IsZero() {
		t.Fatalf("an unrelated save touched the MQTT password: %+v", pw.got())
	}
	for _, ev := range e.aud.events {
		if ev.Action == "device.mqtt_password" {
			t.Fatal("an unrelated save was audited as an MQTT password change")
		}
	}
	e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"mqtt_password_mode": {"own"}, "retry_mqtt_password": {"1"}}))
	c := pw.got()
	if len(c) != 1 || c[0].plain != storedOwnPW {
		t.Fatalf("calls %+v", c)
	}
	if got := e.inv.devs[7]; !got.MQTTPasswordBadAt.IsZero() || got.MQTTPassword != storedOwnPW {
		t.Fatalf("stored %+v", got)
	}
}

// Why: when the page is shown again (unverified move, another field rejected) a typed password
// is gone, so the person is told to type it again, and the chosen mode stays selected.
func TestRerenderKeepsTheModeAndAsksForThePasswordAgain(t *testing.T) {
	e, pw := mqttEnv(t, model.Device{ID: 7, Name: "porch", Address: "10.0.0.7", HTTPPassword: "web-pw"})
	e.inv.unproven = true
	res, body := e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"address": {"10.0.0.99"}, "mqtt_password_mode": {"own"}, "mqtt_password": {"typed-409-pw"}}))
	if res.StatusCode != 409 || len(pw.got()) != 0 {
		t.Fatalf("status %d, calls %+v", res.StatusCode, pw.got())
	}
	if !strings.Contains(body, `value="own" selected`) || !strings.Contains(body, "Enter the MQTT password again") || strings.Contains(body, "typed-409-pw") {
		t.Fatal("409 re-render lost the mode, the hint, or leaks the password")
	}
	res, body = e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"port": {"abc"}, "mqtt_password_mode": {"broker"}}))
	if res.StatusCode != 422 || !strings.Contains(body, `value="broker" selected`) {
		t.Fatalf("status %d: mode lost on a rejected field", res.StatusCode)
	}
}

// Why: "Own password" with no password to use is a mistake to name, not a silent "not set"; and
// the rest of the form must not be saved half-way.
func TestOwnModeWithoutAnyPasswordIsRejectedAndSavesNothing(t *testing.T) {
	e, pw := mqttEnv(t, model.Device{ID: 7, Name: "porch", Address: "10.0.0.7"})
	res, body := e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"name": {"renamed"}, "mqtt_password_mode": {"own"}}))
	if res.StatusCode != 422 || !strings.Contains(body, "Enter the device") {
		t.Fatalf("status %d", res.StatusCode)
	}
	if len(pw.got()) != 0 || e.inv.devs[7].Name != "porch" {
		t.Fatalf("calls %+v, name %q", pw.got(), e.inv.devs[7].Name)
	}
}

// Why: a quote or backslash cannot be sent in a FileDownload command, so it is refused where
// the person types it, not at the first backup.
func TestOwnPasswordWithQuoteIsRejected(t *testing.T) {
	e, pw := mqttEnv(t, model.Device{ID: 7, Name: "porch", Address: "10.0.0.7"})
	for _, bad := range []string{`a"b`, `a\b`, "a\nb"} {
		res, _ := e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"mqtt_password_mode": {"own"}, "mqtt_password": {bad}}))
		if res.StatusCode != 422 {
			t.Errorf("%q: status %d", bad, res.StatusCode)
		}
	}
	if len(pw.got()) != 0 {
		t.Fatalf("calls %+v", pw.got())
	}
}

// Why (D6): leaving "Own password" drops the stored secret so none stays at rest; "same as the
// broker" stores no value at all.
func TestSwitchingToBrokerOrNotSetDropsTheOwnPassword(t *testing.T) {
	for _, mode := range []string{"broker", ""} {
		e, pw := mqttEnv(t, ownDevice())
		res, _ := e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"mqtt_password_mode": {mode}, "mqtt_password": {"ignored"}}))
		c := pw.got()
		if res.StatusCode != 303 || len(c) != 1 || c[0].plain != "" || string(c[0].mode) != mode {
			t.Fatalf("mode %q: status %d, calls %+v", mode, res.StatusCode, c)
		}
		if got := e.inv.devs[7]; got.MQTTPassword != "" || string(got.MQTTPasswordMode) != mode {
			t.Fatalf("mode %q: stored %+v", mode, got)
		}
	}
}

// Why: other posts to the device edit route (the old form, scripts) carry no mode field and must
// leave the MQTT password alone; an unknown mode is refused.
func TestPostWithoutModeLeavesTheMQTTPasswordAlone(t *testing.T) {
	e, pw := mqttEnv(t, ownDevice())
	e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"notes": {"x"}}))
	if len(pw.got()) != 0 || e.inv.devs[7].MQTTPassword != storedOwnPW {
		t.Fatalf("calls %+v", pw.got())
	}
	res, _ := e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"mqtt_password_mode": {"surprise"}}))
	if res.StatusCode != 422 || len(pw.got()) != 0 {
		t.Fatalf("status %d, calls %+v", res.StatusCode, pw.got())
	}
}

// Why: only admins edit devices; an operator must reach neither the form nor the save.
func TestOperatorCannotEditTheMQTTPassword(t *testing.T) {
	e, pw := mqttEnv(t, ownDevice())
	if res, _ := e.do(t, "GET", "/devices/7/edit", "operator", nil); res.StatusCode != 403 {
		t.Fatalf("form: %d", res.StatusCode)
	}
	if res, _ := e.do(t, "POST", "/devices/7", "operator", editPost(url.Values{"mqtt_password_mode": {"broker"}})); res.StatusCode != 403 || len(pw.got()) != 0 {
		t.Fatalf("save: %d, calls %+v", res.StatusCode, pw.got())
	}
}

// Why: without the store behind it (tests, partial wiring) the form has no section rather than a
// field that silently does nothing.
func TestNoMQTTSectionWithoutTheStore(t *testing.T) {
	e := setupWith(t, nil)
	_, body := e.do(t, "GET", "/devices/1/edit", "admin", nil)
	if strings.Contains(body, "mqtt_password") {
		t.Fatal("section rendered without MQTTPasswords")
	}
}

// selectedMode is the value a browser posts for the edit form's mqtt_password_mode: the selected option,
// or the first one when none is selected.
func selectedMode(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `name="mqtt_password_mode"`)
	if i < 0 {
		t.Fatal("no MQTT password source in the form")
	}
	sel := body[i:]
	sel = sel[:strings.Index(sel, "</select>")]
	var values []string
	for _, opt := range strings.Split(sel, "<option")[1:] {
		v := opt[strings.Index(opt, `value="`)+7:]
		v = v[:strings.Index(v, `"`)]
		if strings.Contains(opt[:strings.Index(opt, ">")], "selected") {
			return v
		}
		values = append(values, v)
	}
	if len(values) == 0 {
		t.Fatal("no options")
	}
	return values[0]
}

// Why (#609): TasmoFleet learns the "no password" mode from a device's backup. The edit form must show it,
// so that saving an unrelated field (the notes) posts the mode back unchanged; before, the form had no such
// choice, the browser posted its first option ("Not set"), and the save silently undid the learned mode and
// blocked every transfer over MQTT.
func TestAnUnrelatedSaveKeepsTheNoPasswordMode(t *testing.T) {
	d := model.Device{ID: 7, Name: "porch", Address: "10.0.0.7", MQTTPasswordMode: model.MQTTPasswordEmpty}
	e, pw := mqttEnv(t, d)
	_, body := e.do(t, "GET", "/devices/7/edit", "admin", nil)
	mode := selectedMode(t, body)
	if mode != string(model.MQTTPasswordEmpty) {
		t.Fatalf("the form shows mode %q for a device without a password", mode)
	}
	res, _ := e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"mqtt_password_mode": {mode}, "notes": {"x"}}))
	if res.StatusCode != 303 || len(pw.got()) != 0 || e.inv.devs[7].MQTTPasswordMode != model.MQTTPasswordEmpty {
		t.Fatalf("status %d, calls %+v, mode %q", res.StatusCode, pw.got(), e.inv.devs[7].MQTTPasswordMode)
	}
	// Choosing it stores the mode without a password, like broker mode.
	e, pw = mqttEnv(t, ownDevice())
	res, _ = e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"mqtt_password_mode": {"empty"}, "mqtt_password": {"ignored"}}))
	if c := pw.got(); res.StatusCode != 303 || len(c) != 1 || c[0].mode != model.MQTTPasswordEmpty || c[0].plain != "" {
		t.Fatalf("status %d, calls %+v", res.StatusCode, c)
	}
}

// shownMode is the hidden mode the form showed, which the browser posts back.
func shownMode(t *testing.T, body string) string {
	t.Helper()
	const at = `name="mqtt_password_mode_shown" value="`
	i := strings.Index(body, at)
	if i < 0 {
		t.Fatal("the form does not carry the mode it showed")
	}
	v := body[i+len(at):]
	return v[:strings.Index(v, `"`)]
}

// Why (#609): the learner stores a device's password from a backup without a person. An edit form opened
// before that still shows the old mode, and saving an unrelated field posts it back. That is no choice of
// the person: it must not replace the learned password with "Not set" (dated now, so newer than the backup)
// nor be audited as the admin's change. A select the person did change still counts, whatever the learner
// stored meanwhile.
func TestAStaleFormKeepsALearnedPassword(t *testing.T) {
	e, pw := mqttEnv(t, model.Device{ID: 7, Name: "porch", Address: "10.0.0.7"})
	_, body := e.do(t, "GET", "/devices/7/edit", "admin", nil)
	mode, shown := selectedMode(t, body), shownMode(t, body)
	if mode != "" || shown != "" {
		t.Fatalf("the form shows %q (hidden %q) for a device without a password", mode, shown)
	}
	e.inv.mu.Lock() // the learner stores the backup's password after the form was opened
	d := e.inv.devs[7]
	d.MQTTPasswordMode, d.MQTTPassword = model.MQTTPasswordOwn, "learned-pw-1"
	e.inv.devs[7] = d
	e.inv.mu.Unlock()
	res, _ := e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"mqtt_password_mode": {mode}, "mqtt_password_mode_shown": {shown}, "notes": {"x"}}))
	if d := e.inv.devs[7]; res.StatusCode != 303 || len(pw.got()) != 0 || d.MQTTPasswordMode != model.MQTTPasswordOwn || d.MQTTPassword != "learned-pw-1" {
		t.Fatalf("status %d, calls %+v, stored %q/%q", res.StatusCode, pw.got(), d.MQTTPasswordMode, d.MQTTPassword)
	}
	res, _ = e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"mqtt_password_mode": {"broker"}, "mqtt_password_mode_shown": {shown}}))
	if c := pw.got(); res.StatusCode != 303 || len(c) != 1 || c[0].mode != model.MQTTPasswordBroker {
		t.Fatalf("a changed select was not stored: status %d, calls %+v", res.StatusCode, c)
	}
	// A form that showed "Own password" (blank field) posted after the learner stored broker mode asks for
	// nothing: no error for a section the person left alone, and broker mode stays.
	res, _ = e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"mqtt_password_mode": {"own"}, "mqtt_password_mode_shown": {"own"}, "notes": {"y"}}))
	if d := e.inv.devs[7]; res.StatusCode != 303 || len(pw.got()) != 1 || d.MQTTPasswordMode != model.MQTTPasswordBroker {
		t.Fatalf("status %d, calls %+v, stored %q", res.StatusCode, pw.got(), d.MQTTPasswordMode)
	}
}
