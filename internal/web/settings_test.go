package web

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

type fakeSettings struct {
	mu      sync.Mutex
	values  map[string]string
	readErr error
	applyIn []map[string]string
	applyEr error
	saved   []string
}

func (f *fakeSettings) Read(context.Context, model.DeviceID) (map[string]string, error) {
	return f.values, f.readErr
}
func (f *fakeSettings) Apply(_ context.Context, _ string, _ model.DeviceID, ch map[string]string) ([]settings.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyIn = append(f.applyIn, ch)
	if f.applyEr != nil {
		return nil, f.applyEr
	}
	var out []settings.Result
	for k, v := range ch {
		cmd := k + " " + v
		if k == "MqttPassword" {
			cmd = k + " ***"
		}
		out = append(out, settings.Result{Key: k, Command: cmd})
	}
	return out, nil
}

func (f *fakeSettings) Save(_ context.Context, _ string, _ model.DeviceID, v string) (settings.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, v)
	return settings.Result{Key: "SaveData", Command: "SaveData " + v}, nil
}

func withSettings(t *testing.T, fs *fakeSettings) *env {
	return setupWith(t, func(d *Deps) { d.Settings = fs })
}

func baseForm(extra ...string) url.Values {
	f := url.Values{"csrf": {"tok"}, "TelePeriod": {"300"}, "orig.TelePeriod": {"300"}}
	for i := 0; i+1 < len(extra); i += 2 {
		f.Set(extra[i], extra[i+1])
	}
	return f
}

// Why: settings change MQTT credentials and flash; only admins may open or submit the form.
func TestSettingsRoles(t *testing.T) {
	e := withSettings(t, &fakeSettings{values: map[string]string{"TelePeriod": "300"}})
	for _, c := range []struct {
		method, role string
		want         int
	}{{"GET", "", 401}, {"GET", "operator", 403}, {"GET", "admin", 200}, {"POST", "operator", 403}} {
		resp, _ := e.do(t, c.method, "/devices/1/settings", c.role, url.Values{"csrf": {"tok"}})
		if resp.StatusCode != c.want {
			t.Errorf("%s as %q: %d, want %d", c.method, c.role, resp.StatusCode, c.want)
		}
	}
	e2 := setup(t)
	if resp, _ := e2.do(t, "GET", "/devices/1/settings", "admin", nil); resp.StatusCode != 404 {
		t.Errorf("settings must be absent without a service, got %d", resp.StatusCode)
	}
}

// Why: the form shows SetOption names (plan rule 6), warns about flash writes
// and never pre-fills the MQTT password.
func TestSettingsFormContent(t *testing.T) {
	fs := &fakeSettings{values: map[string]string{"TelePeriod": "300", "SetOption0": "0", "MqttUser": "tasmota", "MqttPassword": "must-not-appear"}}
	e := withSettings(t, fs)
	resp, body := e.do(t, "GET", "/devices/1/settings", "admin", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{">SetOption0 (save_state)</label>", "Restore relay state after power loss", "flash", `name="orig.TelePeriod" value="300"`, "saved to the device once", `type="password"`, `name="csrf"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `name="confirm"`) || strings.Contains(body, `name="save"`) {
		t.Fatal("the flash checkboxes are gone: applying always saves once")
	}
	if strings.Contains(body, "must-not-appear") || strings.Contains(body, `name="orig.MqttPassword"`) {
		t.Fatal("the secret field leaked into the form")
	}
}

// Why: a form built from a failed read would make every field look changed.
func TestSettingsReadFailureShowsNoForm(t *testing.T) {
	e := withSettings(t, &fakeSettings{readErr: tasmota.ErrUnreachable})
	resp, body := e.do(t, "GET", "/devices/1/settings", "admin", nil)
	if resp.StatusCode != 502 || !strings.Contains(body, "unreachable") || strings.Contains(body, "<form class=\"stack\"") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

// Why: a crafted POST must not reach the device layer with a key outside the list.
func TestSettingsUnknownKeyRejected(t *testing.T) {
	fs := &fakeSettings{}
	e := withSettings(t, fs)
	for _, k := range []string{"Reset", "Backlog", "orig.Reset", "OtaUrl"} {
		resp, _ := e.do(t, "POST", "/devices/1/settings", "admin", baseForm("TelePeriod", "60", k, "1"))
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d", k, resp.StatusCode)
		}
	}
	if len(fs.applyIn) != 0 {
		t.Fatalf("applied %v", fs.applyIn)
	}
}

// Why: only edited fields may become commands (flash wear).
func TestSettingsApplySendsOnlyChanges(t *testing.T) {
	fs := &fakeSettings{}
	e := withSettings(t, fs)
	if resp, _ := e.do(t, "POST", "/devices/1/settings", "admin", baseForm()); resp.StatusCode != 200 || len(fs.applyIn) != 0 {
		t.Fatalf("without changes: %d %v", resp.StatusCode, fs.applyIn)
	}
	resp, body := e.do(t, "POST", "/devices/1/settings", "admin", baseForm("TelePeriod", "60", "MqttPassword", "distinct-fake-pw-77"))
	if resp.StatusCode != 200 || len(fs.applyIn) != 1 {
		t.Fatalf("status %d applied %v", resp.StatusCode, fs.applyIn)
	}
	got := fs.applyIn[0]
	if len(got) != 2 || got["TelePeriod"] != "60" || got["MqttPassword"] != "distinct-fake-pw-77" {
		t.Fatalf("changes %v", got)
	}
	if strings.Contains(body, "distinct-fake-pw-77") || !strings.Contains(body, "MqttPassword ***") {
		t.Fatalf("secret leaked or result missing: %s", body)
	}
}

func TestSettingsValidationErrorsRerender(t *testing.T) {
	fs := &fakeSettings{applyEr: &settings.ValidationError{Fields: map[string]string{"TelePeriod": "0 (off) or 10 to 3600 seconds"}}}
	e := withSettings(t, fs)
	resp, body := e.do(t, "POST", "/devices/1/settings", "admin", baseForm("TelePeriod", "5"))
	if resp.StatusCode != 422 || !strings.Contains(body, "10 to 3600") || !strings.Contains(body, "Nothing was sent") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `value="5"`) {
		t.Error("the typed value must survive the re-render")
	}
}

// Why: "Nothing changed." is a neutral outcome, not an error, and the form that
// comes back must still match the device. It used to render in the red error box
// and lose which fields the device did not report: the unreported selects then
// showed their first option, so the sticky bar counted changes nobody made and
// the next Apply would have sent them.
func TestSettingsNothingChangedIsNeutralAndKeepsTheDeviceState(t *testing.T) {
	fs := &fakeSettings{values: map[string]string{"TelePeriod": "300", "Hostname": "lamp-1"}}
	e := withSettings(t, fs)
	// the browser posts what the form showed: unreported fields as "" with orig ""
	form := baseForm("SetOption3", "", "orig.SetOption3", "", "PowerOnState", "", "orig.PowerOnState", "")
	resp, body := e.do(t, "POST", "/devices/1/settings", "admin", form)
	if resp.StatusCode != 200 || len(fs.applyIn) != 0 {
		t.Fatalf("status %d applied %v", resp.StatusCode, fs.applyIn)
	}
	if !strings.Contains(body, `role="status"><span class="banner-dot"></span><div class="banner-text">Nothing changed.`) || strings.Contains(body, `role="alert"><span class="banner-dot"></span><div class="banner-text">Nothing changed`) {
		t.Errorf("Nothing changed must be the neutral notice: %s", body)
	}
	for _, name := range []string{"SetOption3", "PowerOnState"} {
		i := strings.Index(body, `name="`+name+`"`)
		if i < 0 || !strings.Contains(body[i:i+200], `<option value="" selected>(unchanged`) {
			t.Errorf("%s lost its not-reported state, so the counter would count it as changed", name)
		}
	}
	if !strings.Contains(body, `name="Hostname" value="lamp-1"`) || !strings.Contains(body, `name="orig.Hostname" value="lamp-1"`) {
		t.Error("fields show the values the device reports")
	}
}

// ---- policy ----

type fakePolicy struct {
	rep  PolicyReport
	live []bool
}

func (f *fakePolicy) Report(_ context.Context, _ string, live bool) (PolicyReport, error) {
	f.live = append(f.live, live)
	return f.rep, nil
}

// Why: the view must tell drift, unknown and ok apart; "unknown" must never
// render as compliant, and the age of a value is part of the verdict.
func TestPolicyPage(t *testing.T) {
	rule := policy.Rule{Command: "SetOption0", Want: "0", Note: "flash wear", Families: []model.ChipFamily{model.ChipESP8266}}
	fp := &fakePolicy{rep: PolicyReport{
		Rules:   []policy.Rule{rule},
		Devices: []model.Device{{ID: 1, Name: "kitchen"}, {ID: 2, Name: "attic"}, {ID: 3, Name: "cellar"}},
		Findings: []policy.Finding{
			{DeviceID: 1, Rule: rule, Have: "1"},
			{DeviceID: 3, Rule: rule, Unknown: true},
		},
		Provenance: map[model.DeviceID]map[string]ValueSource{
			1: {"setoption0": {Kind: "live", At: time.Now().Add(-5 * time.Minute)}},
			2: {"setoption0": {Kind: "live", At: time.Now().Add(-2 * time.Hour)}},
			3: {"saveData": {Kind: "live", At: time.Now().Add(-time.Minute)}},
		},
	}}
	e := setupWith(t, func(d *Deps) { d.Policy = fp })
	resp, body := e.do(t, "GET", "/policy", "operator", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{"SetOption0 = 0", "Chip family: ESP8266", "flash wear", "1 rule · 1 device deviates", "1 → <b>0</b>", "unknown → <b>0</b>", "checked 5m ago (live)", "last run 1m ago"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "attic") {
		t.Error("the compliant attic is not flagged")
	}
	if _, _ = e.do(t, "POST", "/policy/live", "operator", url.Values{"csrf": {"tok"}}); len(fp.live) != 2 || fp.live[0] || !fp.live[1] {
		t.Errorf("live flags %v", fp.live)
	}
	if resp, _ := e.do(t, "GET", "/policy", "", nil); resp.StatusCode != 401 {
		t.Errorf("anonymous: %d", resp.StatusCode)
	}
	if resp, _ := setup(t).do(t, "GET", "/policy", "operator", nil); resp.StatusCode != 404 {
		t.Errorf("view must be hidden without rules, got %d", resp.StatusCode)
	}
}

// Why: an unreported select would otherwise post its first option (SetOption3
// 0 turns MQTT off) and the diff against the empty original would apply it.
func TestSettingsUnreportedSelectIsUnchanged(t *testing.T) {
	fs := &fakeSettings{values: map[string]string{"TelePeriod": "300"}}
	e := withSettings(t, fs)
	_, body := e.do(t, "GET", "/devices/1/settings", "admin", nil)
	i := strings.Index(body, `name="SetOption3"`)
	if i < 0 || !strings.Contains(body[i:i+200], `<option value="" selected>(unchanged`) {
		t.Fatalf("SetOption3 lacks the unchanged option: %s", body[i:i+200])
	}
	// the browser now posts "" and orig "": no change, so nothing is applied
	form := baseForm("SetOption3", "", "orig.SetOption3", "", "PowerOnState", "", "orig.PowerOnState", "")
	if resp, _ := e.do(t, "POST", "/devices/1/settings", "admin", form); resp.StatusCode != 200 || len(fs.applyIn) != 0 {
		t.Fatalf("status %d applied %v", resp.StatusCode, fs.applyIn)
	}
}

// Why: with SaveData 0 nothing persists a change, so every applied batch ends
// with exactly one SaveData write (no checkbox, no choice); it is skipped only
// when a restart or SaveData itself saves anyway, or nothing was applied.
func TestSettingsAlwaysSavesOnce(t *testing.T) {
	fs := &fakeSettings{}
	e := withSettings(t, fs)
	f := baseForm("TelePeriod", "60", "orig.SaveData", "0", "SaveData", "0")
	_, body := e.do(t, "POST", "/devices/1/settings", "admin", f)
	if len(fs.saved) != 1 || fs.saved[0] != "0" || !strings.Contains(body, "Written to flash with one SaveData 0") {
		t.Fatalf("saved %v body %s", fs.saved, body)
	}
	fs.saved = nil
	e.do(t, "POST", "/devices/1/settings", "admin", baseForm("Hostname", "h-2", "orig.Hostname", "h-1", "orig.SaveData", "0", "SaveData", "0"))
	if len(fs.saved) != 0 {
		t.Fatal("a restarting change saves by itself")
	}
	fs.saved = nil
	fs.values = map[string]string{"SaveData": "5"}
	e.do(t, "POST", "/devices/1/settings", "admin", baseForm("TelePeriod", "60")) // form without SaveData: asked from the device
	if len(fs.saved) != 1 || fs.saved[0] != "5" {
		t.Fatalf("saved %v, want one SaveData 5 read from the device", fs.saved)
	}
	fs.saved = nil
	fs.values = map[string]string{}
	e.do(t, "POST", "/devices/1/settings", "admin", baseForm("TelePeriod", "60"))
	if len(fs.saved) != 0 {
		t.Fatal("must not guess the SaveData value")
	}
}

// Why: backups are stale; when one stands in for a missing live value the page
// must say so with its date, and a setting that cannot be read without side
// effects must say that instead of looking like missing data.
func TestPolicyPageLabelsBackupAndNotReadable(t *testing.T) {
	r1 := policy.Rule{Command: "SaveData", Want: "0"}
	r2 := policy.Rule{Command: "Reset", Want: "0"}
	taken := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fp := &fakePolicy{rep: PolicyReport{
		Rules: []policy.Rule{r1, r2}, Devices: []model.Device{{ID: 1, Name: "kitchen"}},
		Findings:   []policy.Finding{{DeviceID: 1, Rule: r1, Have: "5"}, {DeviceID: 1, Rule: r2, RuleIndex: 1, Unknown: true}},
		Provenance: map[model.DeviceID]map[string]ValueSource{1: {"savedata": {Kind: "backup", At: taken}}},
		NotLive:    map[string]bool{"reset": true},
	}}
	e := setupWith(t, func(d *Deps) { d.Policy = fp })
	_, body := e.do(t, "GET", "/policy", "operator", nil)
	if !strings.Contains(body, "from backup of 2026-10-01") {
		t.Errorf("the backup value must name its date:\n%s", body)
	}
	// The second rule (index 1) is the one with the value that cannot be read live.
	if _, body = e.do(t, "GET", "/policy?edit=1", "operator", nil); !strings.Contains(body, "not readable live") {
		t.Error("an unreadable setting says so")
	}
}
