package policy

import (
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: a rule that is meant for the kitchen devices must not flag the attic.
// Scope is the union of the listed tags and device names; an empty scope is the
// whole fleet; chip families narrow it further.
func TestCheckHonoursTagAndDeviceScope(t *testing.T) {
	devs := []model.Device{
		dev(1, "plug-a", model.ChipESP8266, "kitchen"),
		dev(2, "plug-b", model.ChipESP8266, "attic"),
		dev(3, "lamp", model.ChipESP32, "attic"),
		dev(4, "other", model.ChipESP8266),
	}
	vals := map[model.DeviceID]map[string]string{}
	for _, d := range devs {
		vals[d.ID] = map[string]string{"SaveData": "1"}
	}
	r := Rule{Command: "SaveData", Want: "0", Tags: []string{"kitchen"}, Devices: []string{"lamp"}}
	got := Check([]Rule{r}, devs, vals)
	if len(got) != 2 || got[0].DeviceID != 1 || got[1].DeviceID != 3 {
		t.Fatalf("findings %+v", got)
	}
	r.Families = []model.ChipFamily{model.ChipESP32}
	got = Check([]Rule{r}, devs, vals)
	if len(got) != 1 || got[0].DeviceID != 3 {
		t.Fatalf("families narrow the scope: %+v", got)
	}
	// An exception still wins.
	r.Except = []string{"lamp"}
	if got := Check([]Rule{r}, devs, vals); len(got) != 0 {
		t.Fatalf("exception: %+v", got)
	}
}

func TestFindingCarriesTheRuleSeverity(t *testing.T) {
	d := dev(1, "a", model.ChipESP8266)
	rules := []Rule{{Command: "SaveData", Want: "0", Severity: SeverityInfo}, {Command: "SetOption0", Want: "0"}}
	got := Check(rules, []model.Device{d}, map[model.DeviceID]map[string]string{1: {"SaveData": "1", "SetOption0": "1"}})
	if len(got) != 2 || got[0].Severity != SeverityInfo || got[1].Severity != SeverityWarn {
		t.Fatalf("%+v", got)
	}
}

func TestParseRulesScopeSeverityAndName(t *testing.T) {
	in := `[{"name":"Flash wear","command":"SaveData","want":"0","severity":"info","tags":["kitchen"],"devices":["lamp"],"families":["esp32*"]}]`
	rs, err := ParseRules([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := rs[0]
	if r.Name != "Flash wear" || r.Severity != SeverityInfo || r.Tags[0] != "kitchen" || r.Devices[0] != "lamp" || len(r.Families) < 2 {
		t.Fatalf("%+v", r)
	}
	rs, _ = ParseRules([]byte(`[{"command":"SaveData","want":"0"}]`), nil)
	if rs[0].Severity != SeverityWarn {
		t.Fatalf("the default severity is warn: %q", rs[0].Severity)
	}
	bad := map[string]string{
		"severity":      `[{"command":"SaveData","want":"0","severity":"fatal"}]`,
		"long name":     `[{"command":"SaveData","want":"0","name":"` + strings.Repeat("x", 81) + `"}]`,
		"control name":  `[{"command":"SaveData","want":"0","name":"a\u0000b"}]`,
		"empty tag":     `[{"command":"SaveData","want":"0","tags":[""]}]`,
		"empty device":  `[{"command":"SaveData","want":"0","devices":[""]}]`,
		"long tag":      `[{"command":"SaveData","want":"0","tags":["` + strings.Repeat("x", 65) + `"]}]`,
		"too many tags": `[{"command":"SaveData","want":"0","tags":[` + strings.Repeat(`"t",`, 32) + `"t"]}]`,
	}
	for name, s := range bad {
		if _, err := ParseRules([]byte(s), nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
