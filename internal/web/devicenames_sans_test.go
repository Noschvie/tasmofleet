package web

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/templates"
)

var monoClassRe = regexp.MustCompile(`class="[^"]*\bmono\b`)

// nameOpeners returns the opening tag of every element whose whole text is name, so a test can read the classes of
// the element that holds a device name without caring which tag or card draws it.
func nameOpeners(body, name string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<[a-z]+[^<>]*>`+regexp.QuoteMeta(name)+`</`).FindAllString(body, -1) {
		out = append(out, m[:strings.LastIndex(m, "</")])
	}
	return out
}

// noMonoName fails when an element holding one of names is in the mono font, or when no element holds it at all (a
// test that finds nothing judges nothing).
func noMonoName(t *testing.T, where, body string, names ...string) {
	t.Helper()
	for _, name := range names {
		opens := nameOpeners(body, name)
		if len(opens) == 0 {
			t.Errorf("%s: no element holds the device name %q, the test judges nothing:\n%s", where, name, body)
		}
		for _, o := range opens {
			if monoClassRe.MatchString(o) {
				t.Errorf("%s: the device name %q is in the mono font: %s", where, name, o)
			}
		}
	}
}

// Why (#1103, A5-policy-templates-019): mono is for addresses, MACs, versions, ids and commands. A device name is a
// word somebody typed; the Devices card of a rule drew it bold mono while the editor's list above used the page
// font, so one name had three looks on one page. A device name is sans on every policy page.
func TestPolicyPagesDrawDeviceNamesInSans(t *testing.T) {
	rp := defaultRP()
	e, mem := ruleEnvWith(t, rp)
	id := ruleSeed(t, e, mem, url.Values{"name": {"Savedata"}, "family": {}, "tag": {}, "tags_extra": {""}, "except": {}, "want": {"0"}})
	_, body := e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	noMonoName(t, "the Devices card of a rule", section(t, body, "dc-devs"), "kitchen", "shed")
	// the values stay mono: "Actual" is a value a person compares character by character
	if devs := section(t, body, "dc-devs"); !strings.Contains(devs, `<span class="mono">`) {
		t.Errorf("the Actual value must stay mono:\n%s", devs)
	}

	rep := devReport()
	rep.Devices[2].Identity.Variant = "safeboot" // pantry: the firmware fold
	page := devPage(t, "admin", "", rep)
	noMonoName(t, "the group header of the deviations", page, "kitchen", "attic")
	at := strings.Index(page, `id="unjudged"`)
	if at < 0 {
		t.Fatal("no fold of unjudged devices")
	}
	fold := page[at:]
	noMonoName(t, "the not-judged folds of the deviations", fold, "pantry", "cellar")
	// Expected and Actual are values, not names
	if !strings.Contains(page, `<td data-label="Actual"`) || !strings.Contains(page, `class="mono pol-dev-v pol-dev-bad"`) {
		t.Errorf("the Actual value of a deviation must stay mono")
	}
}

// Why (#1103): the GPIO templates page lists the same devices as the rules do; a name there is a name too. The rows
// of a group, the devices that cannot be compared, the built-in modules, the single devices, the reference line and
// the devices left out of a preview named them in mono.
func TestGPIOTemplatesDrawDeviceNamesInSans(t *testing.T) {
	f := gtFixture()
	f.ov.BuiltIn = []templates.GroupView{{Key: policy.GroupKey{Family: model.ChipESP8266, Name: "Sonoff Basic", BuiltIn: true},
		Devices: []templates.DeviceView{tdev(20, "bulb-1", policy.GPIOTemplate{}, true)}}}
	solo := policy.GPIOTemplate{Name: "Shelly 1", GPIO: gpio8266(12), Base: 45}
	f.ov.Groups = append(f.ov.Groups, templates.GroupView{Key: policy.GroupKey{Family: model.ChipESP8266, Name: "Shelly 1"},
		Devices: []templates.DeviceView{tdev(21, "solo-plug", solo, true)}})
	e := gtEnv(t, f)
	_, body := e.do(t, "GET", "/policy/templates", "admin", nil)
	noMonoName(t, "the templates page", body, "tv-plug", "router-plug", "balcony-plug", "attic-plug", "bulb-1", "solo-plug")

	_, prev := e.do(t, "POST", "/policy/templates/preview", "admin", url.Values{"csrf": {"tok"}, "family": {"esp8266"}, "name": {"Gosund SP111"}, "ref": {"10"}, "how": {"chosen"}, "pick": {"12", "11"}})
	noMonoName(t, "the preview", prev, "tv-plug", "router-plug", "balcony-plug")
	if !strings.Contains(prev, `class="mono gt-want"`) {
		t.Errorf("the wanted pin is a value and stays mono")
	}
}

// Why (#1103): the Identity card's Device name is a name; its Hostname, MQTT topic and MAC are addresses and stay mono.
func TestIdentityDeviceNameIsSansAndTheAddressesStayMono(t *testing.T) {
	e := nameSetup(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	i := strings.Index(body, "<dt>Device name</dt>")
	if i < 0 {
		t.Fatalf("no Device name row:\n%s", body)
	}
	if dd := body[i+len("<dt>Device name</dt>") : i+len("<dt>Device name</dt>")+60]; strings.Contains(dd, "mono") || !strings.Contains(dd, "ukv-name") {
		t.Errorf("the Device name value is a name: sans 500 (ukv-name), not mono: %s", dd)
	}
	if !strings.Contains(body, `<dt>MQTT topic</dt><dd class="mono"`) {
		t.Errorf("the MQTT topic is an address and stays mono")
	}
}
