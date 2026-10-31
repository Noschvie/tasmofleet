package web

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// fleetOf sets the inventory to devices with these firmware versions.
func fleetOf(e *env, devs ...model.Device) {
	for id := range e.inv.devs {
		delete(e.inv.devs, id)
	}
	for i, d := range devs {
		d.ID = model.DeviceID(i + 1)
		if d.Address == "" {
			d.Address = "10.0.0." + string(rune('1'+i))
		}
		e.inv.devs[d.ID] = d
	}
}

func esp8266(name, version, variant string) model.Device {
	return model.Device{Name: name, Identity: model.Identity{Family: model.ChipESP8266, Version: version, Variant: variant}}
}

// Why: the suggestions are the catalogue, not a list somebody keeps: the editor must offer a
// command that the old hand-written list never had, with its index range and our description.
func TestCommandSuggestionsComeFromTheCatalogue(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	fleetOf(e, esp8266("a", "16.0.0", "tasmota"), esp8266("b", "13.4.0", "tasmota"))
	r, body := e.do(t, "GET", "/policy/rules/command-suggest?command=SetOpt", "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("%d", r.StatusCode)
	}
	for _, w := range []string{`data-name="SetOption"`, "0..177", "Switch one firmware behaviour on or off", "Starts with", "commands in Tasmota 13.4.0, 16.0.0"} {
		if !strings.Contains(body, w) {
			t.Errorf("suggestions lack %q:\n%s", w, body)
		}
	}
	_, body = e.do(t, "GET", "/policy/rules/command-suggest?command=Mqtt", "admin", nil)
	if !strings.Contains(body, `data-name="MqttHost"`) || strings.Contains(body, `data-name="MqttPassword"`) {
		t.Errorf("MqttHost is offered, a secret is not:\n%s", body)
	}
	if r, _ := e.do(t, "GET", "/policy/rules/command-suggest?command=x&other=1", "admin", nil); r.StatusCode != 400 {
		t.Errorf("an unknown parameter: %d", r.StatusCode)
	}
}

// Why: a command with an index range is typed as name plus number; switching between the two
// forms replaces the field, staying in one form must not (it would drop the focus).
func TestValueControlSwitchesBetweenPlainAndIndexedField(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	get := func(q string) string {
		r, body := e.do(t, "GET", "/policy/rules/value-control?"+q, "admin", nil)
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d\n%s", q, r.StatusCode, body)
		}
		return body
	}
	body := get("command=SetOption19")
	if !strings.Contains(body, `id="command-field"`) || !strings.Contains(body, `data-cmd-base="SetOption"`) || !strings.Contains(body, `0 to 177`) || !strings.Contains(body, "on / off") {
		t.Errorf("plain to indexed:\n%s", body)
	}
	body = get("base=SetOption&index=20")
	if strings.Contains(body, `id="command-field"`) || !strings.Contains(body, "Sends") {
		t.Errorf("indexed stays indexed: the field must not be replaced:\n%s", body)
	}
	body = get("edit=1&base=SetOption&index=19")
	if !strings.Contains(body, `id="command-field"`) || !strings.Contains(body, `value="SetOption19"`) || strings.Contains(body, "data-cmd-base") {
		t.Errorf("Change gives the plain field back:\n%s", body)
	}
	if r, _ := e.do(t, "GET", "/policy/rules/value-control?base=x%20y", "admin", nil); r.StatusCode != 400 {
		t.Errorf("a base that is not a name: %d", r.StatusCode)
	}
}

// Why: the value field follows the catalogue's kind and range for commands no curated setting covers.
func TestValueControlFollowsTheCatalogueKind(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	body := func(cmd string) string {
		_, b := e.do(t, "GET", "/policy/rules/value-control?command="+cmd, "admin", nil)
		return b
	}
	if b := body("Latitude"); !strings.Contains(b, `step="any"`) || !strings.Contains(b, "Decimal number") || !strings.Contains(b, ">number<") {
		t.Errorf("Latitude takes decimals:\n%s", b)
	}
	if b := body("BlinkTime"); !strings.Contains(b, `min="2"`) || !strings.Contains(b, `max="3600"`) || !strings.Contains(b, `step="1"`) {
		t.Errorf("BlinkTime has a range:\n%s", b)
	}
	if b := body("LogHost"); !strings.Contains(b, ">text<") || !strings.Contains(b, "No range is known") {
		t.Errorf("MqttHost is text:\n%s", b)
	}
	if b := body("Fade"); !strings.Contains(b, "on / off") || !strings.Contains(b, `value="1"`) {
		t.Errorf("Fade is a state:\n%s", b)
	}
	if b := body("MqttHost"); !strings.Contains(b, "Tasmota docs") || !strings.Contains(b, "tasmota.github.io/docs/Commands/#mqtthost") || !strings.Contains(b, "Host name or IP address of the MQTT broker.") {
		t.Errorf("help with link:\n%s", b)
	}
}

// Why: the approved design: version and feature checks warn and the rule can still be saved;
// only a command that exists in no Tasmota version of the fleet is refused. An index the source
// puts out of range is a warning too (the range is read heuristically).
func TestCatalogueChecksWarnButOnlyAnUnknownCommandIsRefused(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	fleetOf(e, esp8266("new1", "16.0.0", "tasmota"), esp8266("old1", "13.4.0", "tasmota"), esp8266("old2", "13.4.0", "tasmota"))
	_, b := e.do(t, "GET", "/policy/rules/value-control?command=JsonPP", "admin", nil)
	for _, w := range []string{"JsonPP is not a command in Tasmota 13.4.0 (2 devices)", "tone-warn", "command exists", "not a command in Tasmota 13.4.0", "old1 and old2 show as", "applies to the other 1 device"} {
		if !strings.Contains(b, w) {
			t.Errorf("version warning lacks %q:\n%s", w, b)
		}
	}
	// saving works for it
	saveRule(t, e, url.Values{"command": {"JsonPP"}, "want": {"1"}, "name": {"jsonpp"}})
	if len(ruleIDs(t, mem)) != 1 {
		t.Fatal("a version warning must not block saving")
	}
	_, b = e.do(t, "GET", "/policy/rules/value-control?command=SetOption200", "admin", nil)
	if !strings.Contains(b, "outside 0 to 177") || !strings.Contains(b, "tone-warn") {
		t.Errorf("index warning:\n%s", b)
	}
	saveRule(t, e, url.Values{"command": {"SetOption200"}, "want": {"1"}, "name": {"far"}, "version": {strconv.Itoa(len(mem.vs))}})
	if len(ruleIDs(t, mem)) != 2 {
		t.Fatal("an index out of range must not block saving")
	}
	// a command that no version of the fleet has is refused, with the versions in the sentence
	r, body := e.do(t, "POST", "/policy/rules/save", "admin", ruleForm(url.Values{"command": {"NoSuchCmdAnywhere"}, "want": {"1"}, "name": {"x"}, "version": {strconv.Itoa(len(mem.vs))}}))
	if r.StatusCode != 422 || !strings.Contains(body, "NoSuchCmdAnywhere is not a command in any Tasmota version in your fleet (16.0.0, 13.4.0)") {
		t.Errorf("unknown command: %d\n%s", r.StatusCode, body)
	}
	if len(ruleIDs(t, mem)) != 2 {
		t.Fatal("nothing may be saved for it")
	}
	_, b = e.do(t, "GET", "/policy/rules/value-control?command=NoSuchCmdAnywhere", "admin", nil)
	if !strings.Contains(b, "tone-danger") || !strings.Contains(b, "cannot be saved") {
		t.Errorf("the refusal shows live:\n%s", b)
	}
}

// Why: "needs feature X" is only said for a build that certainly lacks it; the rest of the fleet is not blamed.
func TestFeatureCheckNamesOnlyDevicesWhoseBuildLacksIt(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	fleetOf(e, esp8266("plain", "16.0.0", "tasmota"), esp8266("mini", "16.0.0", "minimal"), esp8266("custom", "16.0.0", "unknown"))
	_, b := e.do(t, "GET", "/policy/rules/value-control?command=Latitude", "admin", nil)
	if !strings.Contains(b, "Latitude needs the feature USE_TIMERS, USE_SUNRISE (1 device)") || !strings.Contains(b, "mini: needs USE_TIMERS, USE_SUNRISE") || strings.Contains(b, "custom") {
		t.Errorf("feature check:\n%s", b)
	}
}

// Why: a number value outside the range the device's handler accepts can never match a reading.
func TestCatalogueValueRangeIsCheckedOnSave(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	r, body := e.do(t, "POST", "/policy/rules/save", "admin", ruleForm(url.Values{"command": {"BlinkTime"}, "want": {"1"}, "name": {"x"}}))
	if r.StatusCode != 422 || !strings.Contains(body, "1 is below the minimum of 2.") {
		t.Fatalf("%d\n%s", r.StatusCode, body)
	}
	if r, _ := e.do(t, "POST", "/policy/rules/save", "admin", ruleForm(url.Values{"command": {"BlinkTime"}, "want": {"abc"}, "name": {"x"}})); r.StatusCode != 422 {
		t.Fatalf("not a number: %d", r.StatusCode)
	}
	saveRule(t, e, url.Values{"command": {"BlinkTime"}, "want": {"10"}, "name": {"x"}})
	if len(ruleIDs(t, mem)) != 1 {
		t.Fatal("an in-range value is saved")
	}
}

// Why: a device whose firmware cannot know the command shows that reason in the devices card,
// not the generic "no backup yet".
func TestNotJudgedNoteNamesTheMissingCommand(t *testing.T) {
	d := model.Device{Name: "old", Identity: model.Identity{Family: model.ChipESP8266, Version: "13.4.0", Variant: "tasmota"}}
	if got := notJudgedNote("JsonPP", d); got != "not a command in 13.4.0" {
		t.Errorf("%q", got)
	}
	if got := notJudgedNote("Sleep", d); got != "" {
		t.Errorf("a known command has no note: %q", got)
	}
	if got := notJudgedNote("JsonPP", model.Device{Name: "x"}); got != "" {
		t.Errorf("no version, no note: %q", got)
	}
}
