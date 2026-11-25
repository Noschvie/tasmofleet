package web

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Why (#1096): four rules drew a pressed, open or selected state by filling it with the text colour (the theme
// switch, the open row menu, the bulk bar, the backups selection bar), so black in light and white in dark meant
// "selected" on some controls and "plain" on others. The design system has one selected look, the accent tint
// (docs/spec/design-system.md, Usage rules, "Selected"), and surface bars that are never inverted. No rule may
// fill with the text colour again: a stylesheet-wide scan, so a page that adds one fails here and not in review.
var textFill = regexp.MustCompile(`^\s*background(?:-color)?\s*:\s*var\(--color-text\)\s*(?:!important)?\s*;`)

func textFills(css string) []int {
	var lines []int
	for i, line := range strings.Split(css, "\n") {
		if textFill.MatchString(line) {
			lines = append(lines, i+1)
		}
	}
	return lines
}

func TestNoRuleFillsWithTheTextColour(t *testing.T) {
	b, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if lines := textFills(string(b)); len(lines) > 0 {
		t.Errorf("app.css fills with the text colour (inverted) at lines %v: a selected state is the accent tint, a bar is a surface bar", lines)
	}
}

// Why (#1096): the scan above is a regular expression; one that matches nothing would pass for ever. It must see
// each way the old rules were written and leave the tints and hovers (a color-mix of the text colour) alone.
func TestTextFillGuardSeesTheOldForms(t *testing.T) {
	for _, bad := range []string{
		"  background: var(--color-text);",
		"background:var(--color-text);",
		"\tbackground-color: var(--color-text) !important;",
	} {
		if len(textFills(bad)) == 0 {
			t.Errorf("the guard misses %q", bad)
		}
	}
	for _, ok := range []string{
		"  background: color-mix(in oklab, var(--color-text) 8%, transparent);",
		"  background: var(--color-accent-bg);",
		"  color: var(--color-text);",
		"  border-color: var(--color-text);",
		"  background: var(--color-text-2);",
	} {
		if len(textFills(ok)) != 0 {
			t.Errorf("the guard flags %q", ok)
		}
	}
}

// postedState is what a browser sends for the rule editor's Enabled control from the page the server rendered:
// every hidden field of that name, and the switch only when it is on. The page decides the field names and
// values; the test only plays the browser (switch the control on or leave it off).
func postedState(t *testing.T, page string, switchedOn bool) []string {
	t.Helper()
	var sent []string
	for _, tag := range regexp.MustCompile(`<input[^>]*name="state"[^>]*>`).FindAllString(page, -1) {
		val := regexp.MustCompile(`value="([^"]*)"`).FindStringSubmatch(tag)
		if val == nil {
			t.Fatalf("a state field without a value: %s", tag)
		}
		switch {
		case strings.Contains(tag, `type="hidden"`):
			sent = append(sent, val[1])
		case strings.Contains(tag, `type="checkbox"`):
			if switchedOn {
				sent = append(sent, val[1])
			}
		default:
			t.Fatalf("an unexpected state field: %s", tag)
		}
	}
	return sent
}

// Why (#1096, A5-018): Enabled was a bare 16 px checkbox where the design system has the setting switch
// (ui.Toggle, role="switch"). The switch must say what it is to a screen reader, and what the page sends for it
// must still decide the rule's state: off disables the rule, on enables it, and a post without the field (a
// script, a form without the switch) keeps the state the rule had, so a save never switches a rule off by itself.
func TestRuleEditorEnabledIsASwitchThatDecidesTheState(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, url.Values{"family": {}, "tag": {}, "tags_extra": {""}, "except": {}})
	_, page := e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	sw := regexp.MustCompile(`<input[^>]*name="state"[^>]*value="enabled"[^>]*>`).FindString(page)
	if !strings.Contains(sw, `type="checkbox"`) || !strings.Contains(sw, `role="switch"`) || !strings.Contains(sw, "checked") {
		t.Fatalf("the Enabled control of an enabled rule is not a checked switch: %q", sw)
	}
	if strings.Contains(page, `class="pol-enabled"`) {
		t.Error("the bare checkbox row is still on the page")
	}
	if !strings.Contains(page, `class="sw"`) {
		t.Error("Enabled does not draw the switch track of ui.Toggle")
	}
	// the hidden marker must reach the server before the switch: the last value wins
	if h, s := strings.Index(page, `type="hidden" name="state"`), strings.Index(page, sw); h < 0 || h > s {
		t.Errorf("the hidden state marker (%d) must come before the switch (%d)", h, s)
	}

	off := postedState(t, page, false)
	if len(off) != 1 || off[0] != "disabled" {
		t.Fatalf("a browser with the switch off sends %v, want just the disabled marker", off)
	}
	saveRule(t, e, url.Values{"id": {id}, "version": {"1"}, "want": {"1"}, "state": off})
	if s := storedSpecs(t, mem); !s[0].Disabled {
		t.Fatal("saving with the switch off must disable the rule")
	}

	_, page = e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	if sw := regexp.MustCompile(`<input[^>]*name="state"[^>]*value="enabled"[^>]*>`).FindString(page); strings.Contains(sw, "checked") {
		t.Errorf("a disabled rule shows its switch on: %q", sw)
	}
	on := postedState(t, page, true)
	saveRule(t, e, url.Values{"id": {id}, "version": {"2"}, "want": {"1"}, "state": on})
	if s := storedSpecs(t, mem); s[0].Disabled {
		t.Fatalf("saving with the switch on (%v) must enable the rule", on)
	}

	saveRule(t, e, url.Values{"id": {id}, "version": {"3"}, "want": {"1"}, "state": off})
	saveRule(t, e, url.Values{"id": {id}, "version": {"4"}, "want": {"0"}}) // no state field at all
	if s := storedSpecs(t, mem); !s[0].Disabled {
		t.Error("a save without the state field turned a disabled rule on")
	}
}

// Why (#1096, A5-018): "History of this rule" was a closed fold with no hint of what is inside, an empty-looking
// card at the foot of the page. The summary says how many versions it holds.
func TestRuleHistoryFoldSaysHowManyVersions(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	saveRule(t, e, nil) // v1
	a := ruleIDs(t, mem)[0]
	_, body := e.do(t, "GET", "/policy/rules/"+a, "admin", nil)
	if !strings.Contains(body, "<summary>History of this rule · 1 version</summary>") {
		t.Errorf("one version: the summary has no count:\n%s", sliceBetween(body, `id="rule-history"`, "</summary>"))
	}
	saveRule(t, e, url.Values{"id": {a}, "version": {"1"}, "want": {"1"}}) // v2
	_, body = e.do(t, "GET", "/policy/rules/"+a, "admin", nil)
	if !strings.Contains(body, "<summary>History of this rule · 2 versions</summary>") {
		t.Errorf("two versions: the summary has no count:\n%s", sliceBetween(body, `id="rule-history"`, "</summary>"))
	}
}

// Why (#1096, A3-029): the selection bar of the Backups tab said "Tick two backups to compare them" without
// JavaScript and "Select two backups to compare them" in the line under the list, and the bar of the orphaned
// backups said "Tick the backups to delete". One verb: a person who selects in the browser and one who ticks a box
// without script read the same sentence.
func TestSelectionBarsUseOneVerb(t *testing.T) {
	e, _, _ := subPageEnv(t)
	_, page := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	if n := strings.Count(page, "Select two backups to compare them"); n != 2 {
		t.Errorf("the no-script bar and the foot line say 'Select two backups to compare them' %d times, want 2", n)
	}
	if strings.Contains(page, "Tick") {
		t.Error("the Backups tab still says Tick")
	}
	e2, _ := cleanupEnv(t)
	_, orph := e2.do(t, "GET", "/devices/backups", "admin", nil)
	if !strings.Contains(orph, "Select the backups to delete") || strings.Contains(orph, "Tick the backups") {
		t.Error("the orphaned backups' no-script bar does not say 'Select the backups to delete'")
	}
}

// Why (#1096, DS-013): System settings drew its own switch (.swhit and .swtrack, 34 x 20, a different off state)
// beside ui.Toggle (38 x 22). Every on/off setting, TLS of the broker included, is the one component now: a
// checkbox with role="switch" followed by the .sw track, in the label that is the hit target.
func TestSystemSettingsSwitchesAreUIToggles(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	for _, path := range []string{"/settings/system", "/settings/system?tab=mqtt"} {
		_, body := e.do(t, "GET", path, "admin", nil)
		if strings.Contains(body, "swtrack") || strings.Contains(body, "swhit") {
			t.Errorf("%s: still draws the System settings switch of its own", path)
		}
		boxes := regexp.MustCompile(`<input type="checkbox" role="switch"[^>]*>`).FindAllString(body, -1)
		if len(boxes) == 0 {
			t.Fatalf("%s: no switch at all", path)
		}
		for _, b := range boxes {
			at := strings.Index(body, b) + len(b)
			if !strings.HasPrefix(body[at:], `<span class="sw" aria-hidden="true"></span></label>`) {
				t.Errorf("%s: a switch without the ui.Toggle track: %s", path, b)
			}
		}
	}
	_, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if !strings.Contains(body, `id="f-mqtt-tls"`) {
		t.Error("the broker's TLS switch is not on the MQTT tab")
	}
}
