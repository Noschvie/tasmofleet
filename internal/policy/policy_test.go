package policy

import (
	"os"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func dev(id model.DeviceID, name string, fam model.ChipFamily, tags ...string) model.Device {
	return model.Device{ID: id, Name: name, Tags: tags, Identity: model.Identity{Family: fam}}
}

// flashWear mirrors deploy/unraid/policy.example.json: the flash-wear policy with
// the ESP32 exception for SetOption0.
var flashWear = []Rule{
	{Command: "PowerOnState", Want: "0"},
	{Command: "SetOption0", Want: "0", Families: []model.ChipFamily{model.ChipESP8266}},
	{Command: "SetOption0", Want: "1", Families: []model.ChipFamily{model.ChipESP32}},
	{Command: "SaveData", Want: "0"},
}

func find(fs []Finding, id model.DeviceID, cmd string) (Finding, bool) {
	for _, f := range fs {
		if f.DeviceID == id && f.Rule.Command == cmd {
			return f, true
		}
	}
	return Finding{}, false
}

// Why: the point of the ESP32 exception is that the same value (SetOption0 1)
// is drift on a plug and compliance on a QuinLED. A swapped exception would
// either spam false alarms or hide real flash wear.
func TestESP32ExceptionIsPerFamily(t *testing.T) {
	devs := []model.Device{dev(1, "plug", model.ChipESP8266), dev(2, "quinled", model.ChipESP32)}
	vals := map[model.DeviceID]map[string]string{
		1: {"PowerOnState": "0", "SetOption0": "1", "SaveData": "0"},
		2: {"PowerOnState": "0", "SetOption0": "1", "SaveData": "0"},
	}
	fs := Check(flashWear, devs, vals)
	if len(fs) != 1 {
		t.Fatalf("findings %+v", fs)
	}
	f, ok := find(fs, 1, "SetOption0")
	if !ok || f.Have != "1" || f.Rule.Want != "0" || f.Unknown {
		t.Fatalf("plug with SetOption0 1 must drift: %+v", fs)
	}
	vals[2]["SetOption0"] = "0"
	fs = Check(flashWear, devs, vals)
	if f, ok := find(fs, 2, "SetOption0"); !ok || f.Have != "0" || f.Rule.Want != "1" {
		t.Fatalf("ESP32 with SetOption0 0 must drift against its exception: %+v", fs)
	}
}

// Why: "no value" must never read as "compliant" (a failed backup decode
// would otherwise show the whole fleet green).
func TestMissingValueIsUnknownNotOK(t *testing.T) {
	devs := []model.Device{dev(1, "plug", model.ChipESP8266)}
	fs := Check(flashWear, devs, map[model.DeviceID]map[string]string{1: {"PowerOnState": "0"}})
	f, ok := find(fs, 1, "SaveData")
	if !ok || !f.Unknown || f.Have != "" {
		t.Fatalf("SaveData: %+v", fs)
	}
	if _, ok := find(fs, 1, "PowerOnState"); ok {
		t.Fatal("PowerOnState is compliant")
	}
	// a device without any values at all
	fs = Check(flashWear, devs, nil)
	if len(fs) != 3 { // PowerOnState, SetOption0 (esp8266 rule), SaveData
		t.Fatalf("findings %+v", fs)
	}
	for _, f := range fs {
		if !f.Unknown {
			t.Errorf("%+v should be unknown", f)
		}
	}
}

// Why: a family-limited rule cannot be judged for a device whose chip is not
// identified; saying nothing would hide the exception question. The first
// family rule claims the setting (precedence), so a lower rule never judges a
// device the exception might cover: one unknown, not one per rule.
func TestUnknownFamilyIsUnknownForFamilyRules(t *testing.T) {
	devs := []model.Device{dev(1, "new", model.ChipUnknown)}
	vals := map[model.DeviceID]map[string]string{1: {"PowerOnState": "0", "SetOption0": "0", "SaveData": "0"}}
	fs := Check(flashWear, devs, vals)
	if len(fs) != 1 || fs[0].RuleIndex != 1 {
		t.Fatalf("findings %+v", fs)
	}
	for _, f := range fs {
		if !f.Unknown || f.Rule.Command != "SetOption0" {
			t.Errorf("unexpected %+v", f)
		}
	}
}

// Why: per-device exceptions (by name or tag) are how an operator accepts a
// known deviation without disabling the rule for the fleet.
func TestExceptByNameOrTag(t *testing.T) {
	r := []Rule{{Command: "SaveData", Want: "0", Except: []string{"boiler", "lamps"}}}
	devs := []model.Device{
		dev(1, "boiler", model.ChipESP8266),
		dev(2, "lamp-1", model.ChipESP8266, "lamps"),
		dev(3, "other", model.ChipESP8266, "kitchen"),
	}
	vals := map[model.DeviceID]map[string]string{1: {"SaveData": "5"}, 2: {"SaveData": "5"}, 3: {"SaveData": "5"}}
	fs := Check(r, devs, vals)
	if len(fs) != 1 || fs[0].DeviceID != 3 {
		t.Fatalf("findings %+v", fs)
	}
}

// Why: the device reports ON/OFF where the policy says 1/0, and command names
// differ in case between backups and live replies.
func TestComparisonNormalises(t *testing.T) {
	r := []Rule{{Command: "SetOption0", Want: "0"}, {Command: "Timezone", Want: "99"}}
	devs := []model.Device{dev(1, "a", model.ChipESP8266)}
	ok := Check(r, devs, map[model.DeviceID]map[string]string{1: {"setoption0": "OFF", "TIMEZONE": " 99 "}})
	if len(ok) != 0 {
		t.Fatalf("findings %+v", ok)
	}
	bad := Check(r, devs, map[model.DeviceID]map[string]string{1: {"SetOption0": "ON", "Timezone": "+01:00"}})
	if len(bad) != 2 {
		t.Fatalf("findings %+v", bad)
	}
}

func TestFindingsKeepDeviceThenRuleOrder(t *testing.T) {
	r := []Rule{{Command: "A", Want: "1"}, {Command: "B", Want: "1"}}
	devs := []model.Device{dev(2, "x", model.ChipESP8266), dev(1, "y", model.ChipESP8266)}
	fs := Check(r, devs, nil)
	got := []string{}
	for _, f := range fs {
		got = append(got, f.Rule.Command+string(rune('0'+f.DeviceID)))
	}
	if strings.Join(got, ",") != "A2,B2,A1,B1" {
		t.Fatalf("order %v", got)
	}
}

// ---- ParseRules ----

func TestParseRules(t *testing.T) {
	good := `[{"command":"SetOption0","want":"1","note":"x","families":["esp32","esp32c3"],"except":["a"]}]`
	rs, err := ParseRules([]byte(good), nil)
	if err != nil || len(rs) != 1 || rs[0].Families[1] != model.ChipESP32C3 || rs[0].Except[0] != "a" || rs[0].Note != "x" {
		t.Fatalf("rules %+v err %v", rs, err)
	}
	bad := map[string]string{
		"not an array":     `{"command":"x"}`,
		"unknown key":      `[{"command":"SaveData","want":"0","extra":"1"}]`,
		"empty want":       `[{"command":"SaveData","want":""}]`,
		"missing want":     `[{"command":"SaveData"}]`,
		"bad command":      `[{"command":"Save Data","want":"0"}]`,
		"injection":        `[{"command":"Reset","want":"0;Reset 1"}]`,
		"newline in want":  `[{"command":"SaveData","want":"0\n1"}]`,
		"unknown family":   `[{"command":"SaveData","want":"0","families":["z80"]}]`,
		"empty exception":  `[{"command":"SaveData","want":"0","except":[""]}]`,
		"trailing garbage": `[] []`,
		"long note":        `[{"command":"SaveData","want":"0","note":"` + strings.Repeat("x", 301) + `"}]`,
	}
	for name, in := range bad {
		if _, err := ParseRules([]byte(in), nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	many := "[" + strings.Repeat(`{"command":"SaveData","want":"0"},`, MaxRules) + `{"command":"SaveData","want":"0"}]`
	if _, err := ParseRules([]byte(many), nil); err == nil {
		t.Error("more than MaxRules accepted")
	}
	exact := "[" + strings.Repeat(`{"command":"SaveData","want":"0"},`, MaxRules-1) + `{"command":"SaveData","want":"0"}]`
	if rs, err := ParseRules([]byte(exact), nil); err != nil || len(rs) != MaxRules {
		t.Errorf("exactly MaxRules must be accepted: %v", err)
	}
	// Why: a rule may only name commands the settings layer can query safely.
	only := func(c string) bool { return c == "SaveData" }
	if _, err := ParseRules([]byte(`[{"command":"Restart","want":"1"}]`), only); err == nil {
		t.Error("non-queryable command accepted")
	}
	if _, err := ParseRules([]byte(`[{"command":"SaveData","want":"0"}]`), only); err != nil {
		t.Errorf("queryable command refused: %v", err)
	}
}

// ---- decoded backups ----

// Why: the mapping must follow what decode-config really writes. The fixture
// is the unedited output of decode-config v16.0.0 for the synthetic dump
// (internal/backup/testdata/synthetic), so a renamed key breaks this test
// instead of silently turning a rule into "unknown" (it did once: save_data).
func TestValuesFromDecodedRealOutput(t *testing.T) {
	raw, err := os.ReadFile("testdata/decoded-16.0.0.json")
	if err != nil {
		t.Fatal(err)
	}
	got := ValuesFromDecoded(raw)
	want := map[string]string{
		"PowerOnState": "3", "SaveData": "0", "Timezone": "+00:00", "TelePeriod": "300", "Hostname": "synthetic-plug",
		"Topic": "synthetic_plug", "FullTopic": "", "MqttHost": "mqtt.example.invalid", "MqttPort": "1883", "MqttUser": "synthetic-user",
		"SetOption0": "0", "SetOption3": "0", "SetOption4": "0", "SetOption19": "0",
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %q (present %v), want %q", k, g, ok, v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected extra values: %v", got)
	}
	for k, v := range got {
		if strings.Contains(strings.ToLower(k), "pass") || strings.Contains(v, "synthetic-mqtt-pass") || strings.Contains(v, "wifi-pass") {
			t.Fatalf("a secret was extracted: %s", k)
		}
	}
}

func TestValuesFromDecodedTimezone(t *testing.T) {
	cases := map[string]string{
		`{"timezone":99}`:                       "99",
		`{"timezone":1,"timezone_minutes":0}`:   "+01:00",
		`{"timezone":-5,"timezone_minutes":30}`: "-05:30",
		`{"timezone":14,"timezone_minutes":0}`:  "+14:00",
		`{"timezone":0}`:                        "+00:00",
	}
	for in, want := range cases {
		if got := ValuesFromDecoded([]byte(in))["Timezone"]; got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}
	if _, ok := ValuesFromDecoded([]byte(`{"timezone":"x"}`))["Timezone"]; ok {
		t.Error("a non-numeric timezone must stay unknown")
	}
	if _, ok := ValuesFromDecoded([]byte(`{"timezone":1,"timezone_minutes":"x"}`))["Timezone"]; ok {
		t.Error("non-numeric minutes must stay unknown")
	}
}

func TestValuesFromDecodedOddInput(t *testing.T) {
	if ValuesFromDecoded([]byte("not json")) != nil {
		t.Error("garbage must give no values")
	}
	if got := ValuesFromDecoded([]byte(`{"flag":"x","poweronstate":[1],"save_data":{"a":1}}`)); len(got) != 0 {
		t.Errorf("odd types must be left out: %v", got)
	}
	if got := ValuesFromDecoded([]byte(`{"flag":{"save_state":true}}`)); got["SetOption0"] != "1" {
		t.Errorf("bool flag: %v", got)
	}
}

// Why: "ESP32 exception" must cover every ESP32 chip, including ones added
// later, or their SetOption0 would be skipped and look fine.
func TestParseRulesESP32Wildcard(t *testing.T) {
	rs, err := ParseRules([]byte(`[{"command":"SetOption0","want":"1","families":["esp32*"]}]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs[0].Families) != len(knownFamilies)-1 {
		t.Fatalf("families %v", rs[0].Families)
	}
	for _, f := range knownFamilies {
		if f.IsESP32() != hasFamily(rs[0].Families, f) {
			t.Errorf("%s: wildcard coverage wrong", f)
		}
	}
}
