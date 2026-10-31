package web

import (
	"net/url"
	"strings"
	"testing"
)

// Why (#235): Applies to is a section of the form, not a fold. A fold hid the scope behind a click
// and, once open, pushed the Command row down inside the right column. The section sits after
// "When a device deviates" and before the note, and it carries the pieces of the agreed design.
func TestRuleFormAppliesToIsAnOpenSectionAfterSeverity(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, url.Values{"family": {"esp8266"}, "tag": {}, "tags_extra": {""}, "except": {}})
	_, body := e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	if strings.Contains(body, `class="pol-scope"`) || strings.Contains(body, "<details class=\"pol-scope") {
		t.Error("Applies to is still a fold")
	}
	at := func(s string) int { return strings.Index(body, s) }
	order := []string{`name="name"`, `id="rule-command"`, `name="severity"`, `id="scope-summary"`, `name="note"`, `name="state" value="enabled"`}
	prev := -1
	for _, s := range order {
		i := at(s)
		if i < 0 || i < prev {
			t.Fatalf("form order wrong at %q (%d after %d); want Name, Command, severity, Applies to, Note, Enabled", s, i, prev)
		}
		prev = i
	}
	for _, w := range []string{
		`data-scope-search`, // the device search field
		`data-scope-list`,   // the fixed-height result list
		`data-scope-count`,  // "N of M devices match"
		`ESP8266`,           // the device meta shows the family
		`id="scope-names"`,  // exceptions search devices and tags
		`class="pol-pill"`,  // family toggles
	} {
		if !strings.Contains(body, w) {
			t.Errorf("Applies to section lacks %q", w)
		}
	}
}

// Why: a person weighing a tag wants to know how many devices carry it before ticking it.
func TestRuleFormTagsShowTheirDeviceCount(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, nil)
	_, body := e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	if !strings.Contains(body, `<span class="pol-pill-n">`) {
		t.Errorf("tags in use show no count:\n%s", body)
	}
}

// Why (#235): the tiles and the line under the title describe the rule, so they must follow the
// draft in the form, in the same answer that redraws the devices card (out-of-band), and the
// summary line of the section too. Otherwise changing the expected value leaves them stale.
func TestRulePreviewAlsoSwapsTilesTitleLineAndSummary(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, url.Values{"family": {}, "tag": {}, "tags_extra": {""}, "except": {}})
	f := ruleForm(url.Values{"want": {"1"}, "family": {"esp8266"}, "tag": {}, "tags_extra": {""}, "except": {"shed", "attic"}})
	f.Set("id", id)
	_, body := e.do(t, "POST", "/policy/rules/preview", "admin", f)
	for _, w := range []string{
		`id="rule-tiles"`, `data-tile="applies"`, `id="rule-line"`, `SaveData = 1`, `id="scope-summary"`,
		`hx-swap-oob="true"`, "2 exceptions",
	} {
		if !strings.Contains(body, w) {
			t.Errorf("preview answer lacks %q:\n%s", w, body)
		}
	}
	// the draft's own exceptions are counted in the tile and the title line, not only in the form
	for _, w := range []string{`ESP8266 · 2 exceptions</div>`, `SaveData = 1 · ESP8266 · 2 exceptions · version 1</p>`} {
		if !strings.Contains(body, w) {
			t.Errorf("preview answer lacks %q:\n%s", w, body)
		}
	}
	if n := strings.Count(body, `hx-swap-oob="true"`); n != 3 {
		t.Errorf("want 3 out-of-band parts (tiles, line, summary), got %d", n)
	}
}

// Why: exceptions are picked from a search and sent one per field; a comma list typed without
// JavaScript still works, so both reach the same rule.
func TestRuleDraftExceptionsAcceptRepeatedAndCommaSeparatedFields(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, url.Values{"family": {}, "tag": {}, "tags_extra": {""}, "except": {"shed", "attic, lamp"}})
	_, body := e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	for _, w := range []string{`name="except" value="shed"`, `name="except" value="attic"`, `name="except" value="lamp"`} {
		if !strings.Contains(body, w) {
			t.Errorf("exception chip lacks %q", w)
		}
	}
}
