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
	if strings.Contains(body, "<details") && strings.Contains(body, `pol-scope`) && strings.Contains(body, "<summary>Applies") {
		t.Error("Applies to is still a fold")
	}
	if !strings.Contains(body, `<section class="pol-sec pol-scope"`) {
		t.Error("Applies to is not a section of the form")
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
	if n := strings.Count(body, `hx-swap-oob="true"`); n != 7 {
		t.Errorf("want 7 out-of-band parts (tiles, line, summary, title, trail, phone title, value warning), got %d", n)
	}
}

// Why: picked exceptions arrive whole (a device name may contain a comma) and are ticked boxes,
// so without JavaScript unticking one removes it; only the typed field is a comma list.
func TestRuleDraftExceptionsPickedStayWholeAndCanBeUnticked(t *testing.T) {
	e, mem := ruleEnvWith(t, defaultRP())
	id := ruleSeed(t, e, mem, url.Values{"family": {}, "tag": {}, "tags_extra": {""}, "except": {"Lamp, hall", "shed"}, "except_extra": {"attic, lamp"}})
	_, body := e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	for _, w := range []string{`name="except" value="Lamp, hall" checked`, `name="except" value="shed" checked`, `name="except" value="attic" checked`, `name="except" value="lamp" checked`} {
		if !strings.Contains(body, w) {
			t.Errorf("exception chip lacks %q", w)
		}
	}
	if strings.Contains(body, `name="except" value="Lamp"`) {
		t.Error("a picked device with a comma in its name was split into two exceptions")
	}
	// the form posted again without "shed" (its box unticked) drops it
	f := ruleForm(url.Values{"family": {}, "tag": {}, "tags_extra": {""}, "except": {"Lamp, hall", "attic", "lamp"}})
	f.Set("id", id)
	f.Set("version", "1")
	if r, b := e.do(t, "POST", "/policy/rules/save", "admin", f); r.StatusCode != 303 {
		t.Fatalf("save: %d\n%s", r.StatusCode, b)
	}
	_, body = e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	if strings.Contains(body, `value="shed" checked`) || !strings.Contains(body, `name="except" value="Lamp, hall" checked`) {
		t.Errorf("unticked exception survived or the others were lost:\n%s", body)
	}
}

// Why: like tags and devices, exceptions are capped at 32 so a form cannot store an endless list.
func TestRuleDraftRefusesMoreThanThirtyTwoExceptions(t *testing.T) {
	e, _ := ruleEnvWith(t, defaultRP())
	var many []string
	for i := 0; i < 33; i++ {
		many = append(many, "d"+vstr(int64(i)))
	}
	f := ruleForm(url.Values{"family": {}, "tag": {}, "tags_extra": {""}, "except": many})
	if r, _ := e.do(t, "POST", "/policy/rules/save", "admin", f); r.StatusCode == 303 {
		t.Error("33 exceptions were stored")
	}
}
