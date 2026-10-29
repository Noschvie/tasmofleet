package policy

import (
	"reflect"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

type vals = map[model.DeviceID]map[string]string

func kinds(os []Outcome, id model.DeviceID) map[int]OutcomeKind {
	m := map[int]OutcomeKind{}
	for _, o := range os {
		if o.DeviceID == id {
			m[o.RuleIndex] = o.Kind
		}
	}
	return m
}

// Why: the page counts and Make compliant must read one evaluation. Every
// precedence case below is a place where two separate evaluations once could
// disagree about which rule judges a device.
func TestOutcomesPrecedence(t *testing.T) {
	rules := []Rule{
		{Command: "PowerOnState", Want: "3", Tags: []string{"plug"}},                       // 0: specific, wins on plugs
		{Command: "PowerOnState", Want: "0"},                                               // 1: general
		{Command: "TelePeriod", Want: "60", Disabled: true},                                // 2: parked
		{Command: "TelePeriod", Want: "300"},                                               // 3: judges, the disabled one above does not claim
		{Command: "SetOption0", Want: "0", Except: []string{"quin"}},                       // 4: quin is exempt
		{Command: "SetOption0", Want: "1"},                                                 // 5: an excepted rule does not claim: this one judges quin
		{Command: "Timezone", Want: "99", Families: []model.ChipFamily{model.ChipESP8266}}, // 6: family-limited
		{Command: "Timezone", Want: "+01:00"},                                              // 7: lower, must not judge what 6 could not rule out
		{Command: "SaveData", Want: "0", Families: []model.ChipFamily{model.ChipESP32}},    // 8: not for ESP8266 at all
	}
	devs := []model.Device{
		dev(1, "plug", model.ChipESP8266, "plug"),
		dev(2, "quin", model.ChipESP32),
		dev(3, "mystery", model.ChipUnknown),
	}
	v := vals{
		1: {"PowerOnState": "3", "TelePeriod": "300", "SetOption0": "0", "Timezone": "99"},
		2: {"PowerOnState": "3", "TelePeriod": "10", "SetOption0": "0", "SaveData": "0"},
		3: {"Timezone": "99"},
	}
	os := Outcomes(rules, devs, v)

	// plug: rule 1 is shadowed by rule 0 (why: a plug is judged by the specific rule only)
	want1 := map[int]OutcomeKind{0: OutcomeMatch, 1: OutcomeShadowed, 3: OutcomeMatch, 4: OutcomeMatch, 5: OutcomeShadowed, 6: OutcomeMatch, 7: OutcomeShadowed}
	if got := kinds(os, 1); !reflect.DeepEqual(got, want1) {
		t.Errorf("plug: %v, want %v", got, want1)
	}
	// quin: not a plug, so rule 0 is out of scope and rule 1 judges (3 != 0 deviates);
	// excepted rule 4 leaves the command to rule 5 (0 != 1 deviates); rules 6/7 are out of
	// family / unlimited: 7 judges quin (no Timezone value: unknown); 8 matches.
	want2 := map[int]OutcomeKind{1: OutcomeDeviate, 3: OutcomeDeviate, 4: OutcomeExcepted, 5: OutcomeDeviate, 7: OutcomeUnknown, 8: OutcomeMatch}
	if got := kinds(os, 2); !reflect.DeepEqual(got, want2) {
		t.Errorf("quin: %v, want %v", got, want2)
	}
	// mystery (unknown family): the family-limited rules 6 and 8 claim it and report unknown
	// (why: a lower rule must not judge a device the higher one could not rule out);
	// rule 7 is therefore shadowed even though a value exists.
	got3 := kinds(os, 3)
	if got3[6] != OutcomeUnknown || got3[7] != OutcomeShadowed || got3[8] != OutcomeUnknown {
		t.Errorf("mystery: %v", got3)
	}
	for _, o := range os {
		if o.RuleIndex == 2 {
			t.Errorf("a disabled rule has no outcome: %+v", o)
		}
		if o.Kind == OutcomeShadowed && (o.ByIndex >= o.RuleIndex || rules[o.ByIndex].Command != rules[o.RuleIndex].Command) {
			t.Errorf("shadowed must name a higher rule on the same command: %+v", o)
		}
	}
	for _, o := range os {
		if o.DeviceID == 1 && o.RuleIndex == 1 && o.ByIndex != 0 {
			t.Errorf("rule 1 is shadowed by rule 0, got %d", o.ByIndex)
		}
		if o.Kind == OutcomeMatch && o.Have == "" {
			t.Errorf("a match carries its value: %+v", o)
		}
	}
}

// Why: Make compliant writes what Check reports; the rewrite on top of
// Outcomes must not change one finding. This pins the exact output (fields,
// order) for a mixed fleet, independent of the older Check tests.
func TestCheckIsAFilterOfOutcomes(t *testing.T) {
	rules := []Rule{
		{Command: "PowerOnState", Want: "0", Severity: SeverityInfo},
		{Command: "Timezone", Want: "99", Families: []model.ChipFamily{model.ChipESP8266}},
		{Command: "TelePeriod", Want: "300", Except: []string{"b"}},
	}
	devs := []model.Device{dev(1, "a", model.ChipESP8266), dev(2, "b", model.ChipUnknown)}
	v := vals{1: {"PowerOnState": "3", "Timezone": "99", "TelePeriod": "10"}, 2: {"PowerOnState": "0"}}
	want := []Finding{
		{DeviceID: 1, Rule: rules[0], RuleIndex: 0, Have: "3", Severity: SeverityInfo},
		{DeviceID: 1, Rule: rules[2], RuleIndex: 2, Have: "10", Severity: SeverityWarn},
		{DeviceID: 2, Rule: rules[1], RuleIndex: 1, Severity: SeverityWarn, Unknown: true},
	}
	if got := Check(rules, devs, v); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	// an unknown finding never carries a value, even if one exists for another reason
	if f := Check(rules[1:2], devs[1:], vals{2: {"Timezone": "5"}}); len(f) != 1 || !f[0].Unknown || f[0].Have != "" {
		t.Errorf("unknown family: %+v", f)
	}
}

// Why: the tiles decide what an admin fixes first, so every row says why a
// device counts where it does.
func TestTallyAndFleetTally(t *testing.T) {
	rules := []Rule{
		{Command: "PowerOnState", Want: "0"},                       // 0 Required
		{Command: "SaveData", Want: "0", Severity: SeverityInfo},   // 1 Advisory
		{Command: "TelePeriod", Want: "300", Disabled: true},       // 2 disabled
		{Command: "Timezone", Want: "99", Except: []string{"ex"}},  // 3 Required, one exception
		{Command: "Timezone", Want: "+01:00", Tags: []string{"t"}}, // 4 shadowed on tagged devices
	}
	devs := []model.Device{
		dev(1, "ok", model.ChipESP8266),       // everything right: compliant
		dev(2, "req", model.ChipESP8266),      // Required deviates: Deviating (required), not compliant
		dev(3, "adv", model.ChipESP8266),      // only Advisory deviates: Deviating (advisory) but still compliant
		dev(4, "blind", model.ChipESP8266),    // Required unknown, nothing deviates: Not judged, not compliant
		dev(5, "both", model.ChipESP8266),     // deviates AND unknown: Deviating only, never also Not judged
		dev(6, "ex", model.ChipESP8266),       // excepted from Timezone: rule 4 does not apply (untagged) and rule 3 is Excepted
		dev(7, "advblind", model.ChipESP8266), // Advisory unknown only: Not judged but still compliant
	}
	full := map[string]string{"PowerOnState": "0", "SaveData": "0", "Timezone": "99"}
	with := func(k, val string) map[string]string {
		m := map[string]string{}
		for a, b := range full {
			m[a] = b
		}
		if val == "" {
			delete(m, k)
		} else {
			m[k] = val
		}
		return m
	}
	v := vals{
		1: full,
		2: with("PowerOnState", "3"),
		3: with("SaveData", "5"),
		4: with("Timezone", ""),
		5: func() map[string]string { m := with("PowerOnState", "3"); delete(m, "Timezone"); return m }(),
		6: with("Timezone", ""),
		7: with("SaveData", ""),
	}
	os := Outcomes(rules, devs, v)

	got := Tally(rules, os)
	// dev 6 is excepted from rule 3; rule 4 is out of scope for untagged devices.
	wantRules := []RuleTally{
		{Match: 5, Deviate: 2},              // PowerOnState: dev 2 and 5 deviate
		{Match: 5, Deviate: 1, Unknown: 1},  // SaveData: dev 3 deviates, dev 7 has no value
		{},                                  // disabled: no outcomes at all
		{Match: 4, Unknown: 2, Excepted: 1}, // Timezone: dev 4 and 5 have no value, dev 6 is exempt
		{},                                  // tagged devices only: none here
	}
	if !reflect.DeepEqual(got, wantRules) {
		t.Errorf("tally = %+v, want %+v", got, wantRules)
	}
	if got[3].Applies() != 6 || got[0].Applies() != 7 {
		t.Errorf("applies = %d, %d", got[3].Applies(), got[0].Applies())
	}

	f := FleetTally(rules, devs, os)
	want := Fleet{Rules: 5, Enabled: 4, Disabled: 1, Devices: 7,
		Compliant: 4, // ok, adv (advisory only), advblind (advisory unknown only), ex (exempt)
		Deviating: 3, DeviatingRequired: 2, DeviatingAdvisory: 1,
		NotJudged: 2, // blind, advblind
	}
	if f != want {
		t.Errorf("fleet = %+v, want %+v", f, want)
	}
}

// Why: a shadowed rule is counted nowhere as applying, or one device would
// count twice for one command.
func TestTallyShadowedDoesNotApply(t *testing.T) {
	rules := []Rule{{Command: "A", Want: "1"}, {Command: "A", Want: "2"}}
	devs := []model.Device{dev(1, "x", model.ChipESP8266)}
	tl := Tally(rules, Outcomes(rules, devs, vals{1: {"A": "1"}}))
	if tl[0].Applies() != 1 || tl[1].Applies() != 0 || tl[1].Shadowed != 1 {
		t.Errorf("%+v", tl)
	}
}

func TestBackupReadableAndReadProblem(t *testing.T) {
	for cmd, want := range map[string]bool{"TelePeriod": true, "Timezone": true, "SetOption0": true, "Sleep": false, "LedState": false} {
		if BackupReadable(cmd) != want {
			t.Errorf("BackupReadable(%s) != %v", cmd, want)
		}
	}
	for _, cmd := range []string{"Status", "Status0", "WifiPassword", "MqttPassword", "Restart", "", "Foo Bar", "powerOnState"} {
		if ReadProblem(cmd) == nil {
			t.Errorf("ReadProblem(%q) = nil, want a reason", cmd)
		}
	}
	for _, cmd := range []string{"TelePeriod", "Sleep", "SetOption19", "OtaUrl"} {
		if err := ReadProblem(cmd); err != nil {
			t.Errorf("ReadProblem(%q) = %v", cmd, err)
		}
	}
	if !ValidWant("a.b,c:+_/ -") || ValidWant("") || ValidWant("a\nb") || ValidWant("a;b") {
		t.Error("ValidWant")
	}
}

// Why: after a reorder the slot of a rule changes but its id does not; the
// page links and counts by id, so outcomes, shadowing and tallies must carry it.
func TestOutcomesAndTallyFollowRuleID(t *testing.T) {
	rules := []Rule{
		{ID: "rb", Command: "A", Want: "1"},
		{ID: "ra", Command: "A", Want: "2"},
	}
	devs := []model.Device{dev(1, "x", model.ChipESP8266)}
	os := Outcomes(rules, devs, vals{1: {"A": "1"}})
	if os[0].RuleID != "rb" || os[1].RuleID != "ra" || os[1].Kind != OutcomeShadowed || os[1].ByID != "rb" {
		t.Errorf("%+v", os)
	}
	tl := Tally(rules, os)
	if tl[0].RuleID != "rb" || tl[1].RuleID != "ra" || tl[1].Shadowed != 1 || tl[0].Match != 1 {
		t.Errorf("%+v", tl)
	}
}
