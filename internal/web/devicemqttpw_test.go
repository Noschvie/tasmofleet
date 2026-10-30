package web

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
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
	for _, want := range []string{`name="mqtt_password_mode"`, `value="own" selected`, `placeholder="Unchanged"`, "The device refused the stored MQTT password"} {
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

// Why: saving clears the refused mark (D6): a person who fixed the device retries without
// retyping, and the stored password survives the blank field.
func TestSavingClearsTheRefusedMarkAndKeepsThePassword(t *testing.T) {
	d := ownDevice()
	d.MQTTPasswordBadAt = time.Now()
	e, pw := mqttEnv(t, d)
	e.do(t, "POST", "/devices/7", "admin", editPost(url.Values{"mqtt_password_mode": {"own"}}))
	c := pw.got()
	if len(c) != 1 || c[0].plain != storedOwnPW {
		t.Fatalf("calls %+v", c)
	}
	if got := e.inv.devs[7]; !got.MQTTPasswordBadAt.IsZero() || got.MQTTPassword != storedOwnPW {
		t.Fatalf("stored %+v", got)
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
