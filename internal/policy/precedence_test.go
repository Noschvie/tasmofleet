package policy

import (
	"reflect"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: a disabled rule is a rule the admin parked ("paused while testing");
// it must not flag anything nor shadow a lower rule, or disabling it would
// silently keep part of its effect.
func TestDisabledRuleIsNeverEvaluated(t *testing.T) {
	rules := []Rule{
		{Command: "TelePeriod", Want: "60", Disabled: true},
		{Command: "TelePeriod", Want: "300"},
	}
	devs := []model.Device{dev(1, "a", model.ChipESP8266)}
	fs := Check(rules, devs, map[model.DeviceID]map[string]string{1: {"TelePeriod": "60"}})
	if len(fs) != 1 || fs[0].RuleIndex != 1 || fs[0].Rule.Want != "300" || fs[0].Have != "60" {
		t.Fatalf("the enabled lower rule must judge the device: %+v", fs)
	}
	// alone, a disabled rule produces nothing, not even "unknown"
	if fs := Check(rules[:1], devs, nil); len(fs) != 0 {
		t.Fatalf("disabled rule produced %+v", fs)
	}
}

// Why: the list order is the precedence (design v2, N3). A specific rule placed
// above a general one wins for the devices it covers; without precedence the
// same device would be flagged for both values and could never be compliant.
func TestHigherRuleShadowsLowerForSharedDevices(t *testing.T) {
	rules := []Rule{
		{Command: "PowerOnState", Want: "3", Tags: []string{"plug"}},
		{Command: "poweronstate", Want: "0"}, // case differs: still the same setting
	}
	devs := []model.Device{dev(1, "plug", model.ChipESP8266, "plug"), dev(2, "lamp", model.ChipESP8266)}
	vals := map[model.DeviceID]map[string]string{1: {"PowerOnState": "3"}, 2: {"PowerOnState": "3"}}
	fs := Check(rules, devs, vals)
	if len(fs) != 1 || fs[0].DeviceID != 2 || fs[0].RuleIndex != 1 {
		t.Fatalf("plug is compliant with the higher rule, lamp deviates from the general one: %+v", fs)
	}
	// swapping the order flips who wins for the plug
	rules[0], rules[1] = rules[1], rules[0]
	fs = Check(rules, devs, vals)
	if len(fs) != 2 || fs[0].RuleIndex != 0 || fs[1].RuleIndex != 0 {
		t.Fatalf("the general rule on top must judge both devices: %+v", fs)
	}
}

// Why: an exception or a scope that leaves a device out means the higher rule
// does not set that command there, so the lower rule must still judge it;
// otherwise an exception would turn into "never checked".
func TestRuleThatDoesNotApplyDoesNotShadow(t *testing.T) {
	devs := []model.Device{dev(1, "special", model.ChipESP8266, "plug"), dev(2, "quin", model.ChipESP32)}
	vals := map[model.DeviceID]map[string]string{1: {"SetOption0": "1"}, 2: {"SetOption0": "1"}}
	for name, top := range map[string]Rule{
		"except":   {Command: "SetOption0", Want: "1", Except: []string{"special"}, Families: []model.ChipFamily{model.ChipESP32}},
		"family":   {Command: "SetOption0", Want: "1", Families: []model.ChipFamily{model.ChipESP32}},
		"tagscope": {Command: "SetOption0", Want: "1", Tags: []string{"esp32-board"}, Devices: []string{"quin"}},
	} {
		fs := Check([]Rule{top, {Command: "SetOption0", Want: "0"}}, devs, vals)
		if len(fs) != 1 || fs[0].DeviceID != 1 || fs[0].RuleIndex != 1 {
			t.Errorf("%s: device 1 must be judged by the lower rule, device 2 by the higher: %+v", name, fs)
		}
	}
}

// Why: precedence is never silent (plan risk table): the page shows each
// overlap with the number of shared devices, and a pair that agrees is told
// apart from one that contradicts.
func TestConflictsCountSharedDevices(t *testing.T) {
	rules := []Rule{
		{Command: "PowerOnState", Want: "3", Tags: []string{"plug"}},
		{Command: "SaveData", Want: "0"},
		{Command: "PowerOnState", Want: "0"},
		{Command: "PowerOnState", Want: "on", Disabled: true}, // disabled: no conflict
		{Command: "POWERONSTATE", Want: "3", Devices: []string{"plug2"}},
		{Command: "SaveData", Want: "0", Families: []model.ChipFamily{model.ChipESP32}}, // no ESP32 in the fleet
	}
	devs := []model.Device{
		dev(1, "plug1", model.ChipESP8266, "plug"),
		dev(2, "plug2", model.ChipESP8266, "plug"),
		dev(3, "lamp", model.ChipESP8266),
	}
	got := Conflicts(rules, devs)
	want := []Conflict{
		{Higher: 0, Lower: 2, Command: "PowerOnState", Devices: 2},
		{Higher: 0, Lower: 4, Command: "PowerOnState", Devices: 1, SameWant: true},
		{Higher: 2, Lower: 4, Command: "PowerOnState", Devices: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("conflicts\n got %+v\nwant %+v", got, want)
	}
	if c := Conflicts(rules[:2], devs); c != nil {
		t.Fatalf("different commands never conflict: %+v", c)
	}
}

// Why: a device whose chip is unknown may be covered by a family exception;
// the overlap with a general rule is real and must be shown.
func TestConflictsIncludeUnknownFamily(t *testing.T) {
	rules := []Rule{
		{Command: "SetOption0", Want: "1", Families: []model.ChipFamily{model.ChipESP32}},
		{Command: "SetOption0", Want: "0", Disabled: false},
	}
	got := Conflicts(rules, []model.Device{dev(1, "new", model.ChipUnknown), dev(2, "plug", model.ChipESP8266)})
	if len(got) != 1 || got[0].Devices != 1 || got[0].SameWant {
		t.Fatalf("conflicts %+v", got)
	}
}

// Why: the wording table maps warn to "Required" and info to "Advisory";
// an unset severity is the stricter one, like severityOf.
func TestSeverityLabel(t *testing.T) {
	for in, want := range map[string]string{SeverityWarn: "Required", "": "Required", SeverityInfo: "Advisory", "bogus": "Required"} {
		if got := SeverityLabel(in); got != want {
			t.Errorf("SeverityLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// Why: "disabled" is part of the stored POLICY_RULES format; a disabled rule is
// validated like an enabled one so enabling it later cannot fail.
func TestParseRulesDisabled(t *testing.T) {
	rs, err := ParseRules([]byte(`[{"command":"SaveData","want":"0","disabled":true},{"command":"SaveData","want":"1"}]`), nil)
	if err != nil || len(rs) != 2 || !rs[0].Disabled || rs[1].Disabled {
		t.Fatalf("rules %+v err %v", rs, err)
	}
	if _, err := ParseRules([]byte(`[{"command":"Bad Cmd","want":"0","disabled":true}]`), nil); err == nil {
		t.Fatal("a disabled rule with an invalid command must be refused")
	}
}
