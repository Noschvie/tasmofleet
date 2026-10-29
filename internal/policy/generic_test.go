package policy

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/settings"
)

func rulesJSON(command, want string) []byte {
	b, _ := json.Marshal([]map[string]string{{"command": command, "want": want}})
	return b
}

// Why: a generic rule is sent to the devices unasked (Make compliant) and read every hour,
// so the editor and the file import must refuse, with the reason, everything that is unsafe:
// Status (Status 0 crashes ESP8266), secrets, acting commands, and anything the console
// would ask a confirmation for.
func TestParseRulesGenericCommands(t *testing.T) {
	for _, ok := range [][2]string{{"Sleep", "50"}, {"LedState", "3"}, {"WebLog", "2"}, {"Latitude", "52.5"}, {"OtaUrl", "http://ota.tasmota.com/tasmota/release/tasmota.bin.gz"}} {
		if _, err := ParseRules(rulesJSON(ok[0], ok[1]), settings.Queryable); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, c := range []struct{ command, want, why string }{
		{"Status0", "1", "Status commands are never sent automatically"},
		{"status", "1", "Status commands are never sent automatically"},
		{"StatusSNS", "1", "Status commands are never sent automatically"},
		{"MqttPassword", "x", "secret settings cannot be checked"}, // curated secret
		{"Password", "x", "secret settings cannot be checked"},
		{"Reset", "1", "acts when it is sent without a value"},
		{"Backlog", "x", "acts when it is sent without a value"},
		{"Template", "x", "needs a confirmation in the console and cannot be a policy rule"},
		{"Ap", "1", "needs a confirmation in the console and cannot be a policy rule"},
		{"hostname", "x", "write the setting as Hostname"},
		{"setoption19", "1", "write the setting as SetOption19"},
		{"SO19", "1", "write the full name"},
		{"Module", "1", "restarts, locks out or actuates the device"},
		{"Dimmer", "50", "restarts, locks out or actuates the device"},
		{"ShutterOpen1", "1", "acts when it is sent without a value"},
	} {
		_, err := ParseRules(rulesJSON(c.command, c.want), settings.Queryable)
		if err == nil || !strings.Contains(err.Error(), "cannot be checked") || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s %s: %v, want a refusal with %q", c.command, c.want, err, c.why)
		}
	}
	// The reason holds without the caller's own check too (the file importer passes none).
	if _, err := ParseRules(rulesJSON("Status0", "1"), nil); err == nil {
		t.Error("Status0 accepted when no allowed() is given")
	}
}

// Why: Make compliant sends "<Command> <want>"; for a generic rule that line is judged by the
// command policy, so nothing that needs a console confirmation, and no Status, can be sent.
func TestCommandTextGeneric(t *testing.T) {
	for _, c := range [][3]string{{"Sleep", "50", "Sleep 50"}, {"LedState", " 3 ", "LedState 3"}, {"OtaUrl", "http://ota.tasmota.com/tasmota32/release/tasmota32.bin", "OtaUrl http://ota.tasmota.com/tasmota32/release/tasmota32.bin"}} {
		if got, err := CommandText(c[0], c[1]); err != nil || got != c[2] {
			t.Errorf("CommandText(%q, %q) = %q, %v; want %q", c[0], c[1], got, err, c[2])
		}
	}
	for _, c := range [][2]string{
		{"Status0", "1"}, {"Status", "0"}, {"Reset", "1"}, {"Restart", "1"}, {"Module", "1"}, {"WebServer", "1"}, {"Wifi", "0"},
		{"Password", "x"}, {"MqttPassword", "x"}, {"Sleep", "5;Reset 1"}, {"Sleep", "5\n6"}, {"Sleep", ""}, {"hostname", "x"}, {"SO19", "1"},
	} {
		if got, err := CommandText(c[0], c[1]); err == nil || got != "" {
			t.Errorf("CommandText(%q, %q) = %q, %v; want a refusal", c[0], c[1], got, err)
		}
	}
}

// Why: Make compliant for a generic rule sends exactly one Backlog with the validated
// "<Command> <want>" and one SaveData that re-sends the current value (no restart is assumed),
// then reads the command back to verify it.
func TestFixerAppliesAndVerifiesAGenericRule(t *testing.T) {
	c, r := &fakeCmd{}, &fakeRead{vals: map[string]string{"SaveData": "0", "LedState": "3"}}
	f := newFixer(c, r)
	d := model.Device{ID: 1}
	if err := f.Apply(context.Background(), d, []string{"LedState 3"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Backlog LedState 3", "SaveData 0"}; !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %q, want %q", c.sent, want)
	}
	if _, err := f.Verify(context.Background(), d, []string{"LedState 3"}); err != nil {
		t.Fatal(err)
	}
	r.vals["LedState"] = "2"
	if _, err := f.Verify(context.Background(), d, []string{"LedState 3"}); err == nil || !strings.Contains(err.Error(), "LedState is 2, expected 3") {
		t.Fatalf("a wrong value must fail the verification: %v", err)
	}
}

// Why: the plan is stored and replayed; a line that is no longer allowed (or never was) must be
// stopped by the fixer itself before a device sees it, whatever put it there.
func TestFixerRefusesDeniedGenericLines(t *testing.T) {
	for _, line := range []string{"Status0 1", "Status 0", "Reset 1", "Module 1", "Password x", "Sleep 5;Reset 1", "Backlog Reset 1", "hostname x", "Sleep"} {
		c := &fakeCmd{}
		r := &fakeRead{vals: map[string]string{"SaveData": "0"}}
		if err := newFixer(c, r).Apply(context.Background(), model.Device{}, []string{line}); err == nil || len(c.sent) != 0 || len(r.calls) != 0 {
			t.Errorf("%q: err %v, sent %q, reads %v", line, err, c.sent, r.calls)
		}
	}
}

// Why: OtaUrl is a curated setting that asks a confirmation in the console. Its catalog field
// keeps its own validation, so a rule for it must stay readable and fixable: a TasmoAdmin-era
// URL is flagged and Make compliant sets the upstream one.
func TestOtaUrlRuleDetectsATasmoAdminUrlAndMakeCompliantSetsIt(t *testing.T) {
	devs := []model.Device{dev(1, "plug", model.ChipESP8266)}
	vals := map[model.DeviceID]map[string]string{1: {"OtaUrl": "http://192.0.2.10/tasmoadmin/firmware/tasmota.bin.gz"}}
	fs := Check(otaRules[:1], devs, vals)
	plans := PlanFix(fs, func(model.DeviceID, string) bool { return true })
	want := "OtaUrl http://ota.tasmota.com/tasmota/release/tasmota.bin.gz"
	if len(plans) != 1 || !reflect.DeepEqual(plans[0].Lines(), []string{want}) {
		t.Fatalf("plans %+v", plans)
	}
	c := &fakeCmd{}
	r := &fakeRead{vals: map[string]string{"SaveData": "0"}}
	if err := newFixer(c, r).Apply(context.Background(), devs[0], plans[0].Lines()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.sent, []string{"Backlog " + want, "SaveData 0"}) {
		t.Fatalf("sent %q", c.sent)
	}
}

// Why: a generic value is read live only (it has no place in a decoded backup), and a plan
// built from a finding never turns a denied or unknown rule into a command.
func TestPlanFixGenericRuleNeedsALiveValue(t *testing.T) {
	rule := Rule{Command: "LedState", Want: "3"}
	devs := []model.Device{dev(1, "a", model.ChipESP8266), dev(2, "b", model.ChipESP8266)}
	fs := Check([]Rule{rule}, devs, map[model.DeviceID]map[string]string{1: {"LedState": "1"}})
	plans := PlanFix(fs, func(id model.DeviceID, _ string) bool { return id == 1 })
	if len(plans) != 2 || !reflect.DeepEqual(plans[0].Lines(), []string{"LedState 3"}) || len(plans[1].Commands) != 0 || plans[1].Skipped[0].Reason != SkipUnknown {
		t.Fatalf("plans %+v", plans)
	}
	if _, ok := ValuesFromDecoded([]byte(`{"led_state":1}`))["LedState"]; ok {
		t.Error("a generic command must not be guessed from a backup")
	}
}

// Why: env mode (POLICY_RULES without ids) must name rules exactly like a
// stored pre-0.19 version does, so a link means the same rule in both.
func TestParseRulesGivesPositionIDsToRulesWithoutOne(t *testing.T) {
	rs, err := ParseRules([]byte(`[{"command":"SaveData","want":"0"},{"id":"rkeep","command":"Timezone","want":"99"},{"command":"Latitude","want":"52"}]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].ID != LegacyID(0) || rs[0].ID != "r1" || rs[1].ID != "rkeep" || rs[2].ID != "r3" {
		t.Fatalf("ids %q %q %q", rs[0].ID, rs[1].ID, rs[2].ID)
	}
}

// Why: two rules with one id would make a link ambiguous; refusing beats
// guessing which rule the policy meant.
func TestParseRulesRefusesBadIDs(t *testing.T) {
	for name, in := range map[string]string{
		"duplicate":         `[{"id":"rx","command":"SaveData","want":"0"},{"id":"rx","command":"Timezone","want":"99"}]`,
		"clash with legacy": `[{"command":"SaveData","want":"0"},{"id":"r1","command":"Timezone","want":"99"}]`,
		"bad format":        `[{"id":"X1","command":"SaveData","want":"0"}]`,
	} {
		if _, err := ParseRules([]byte(in), nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Why: a coordinate is judged against a four-decimal reading, so ParseRules reduces the rule's
// want to that form (Make compliant then sends it too); a want that is not a number is refused
// instead of being sent as some other number.
func TestParseRulesCanonicalisesCoordinates(t *testing.T) {
	rules, err := ParseRules([]byte(`[{"command":"Latitude","want":"52.520008"},{"command":"Sleep","want":"50"}]`), nil)
	if err != nil || rules[0].Want != "52.5200" || rules[1].Want != "50" {
		t.Fatalf("rules %+v, %v", rules, err)
	}
	if _, err := ParseRules([]byte(`[{"command":"Longitude","want":"13,40"}]`), nil); err == nil {
		t.Fatal("a comma decimal must be refused")
	}
}
