package web

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (design v2, F5): a phone shows the device list as cards, a wider screen as the table. The
// server renders both (CSS shows one), so a card must carry everything a row does: the device
// link, the selection box with the same device id as its row (select.js keeps the two in step,
// and the bulk actions post either), and the live parts under the event names the stream sends,
// or a card would show a stale status, power state or uptime. Names are text, never markup.
func TestDeviceListRendersTableAndCards(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{} })
	e.inv.devs[2] = model.Device{ID: 2, Name: `attic <b>x</b>`, Address: "10.0.0.2", Port: 8080,
		Identity: model.Identity{Family: model.ChipESP32, Version: "15.0.0"}}
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	if !regexp.MustCompile(`<table[^>]*data-table="devices"`).MatchString(body) {
		t.Fatalf("no device table: %s", body)
	}
	cards := regexp.MustCompile(`(?s)<li class="dcard" data-id="(\d+)">(.*?)</li>`).FindAllStringSubmatch(body, -1)
	if len(cards) != 2 {
		t.Fatalf("want 2 cards, got %d", len(cards))
	}
	for _, c := range cards {
		id := c[1]
		for _, want := range []string{
			`<input type="checkbox" name="id" value="` + id + `"`, `href="/tf/devices/` + id + `"`,
			`sse-swap="health-` + id + `"`, `sse-swap="power-` + id + `"`, `sse-swap="uptime-` + id + `"`, `sse-swap="wifi-` + id + `"`,
			`<details class="rowmenu`, // the same row actions as the table
		} {
			if !strings.Contains(c[2], want) {
				t.Errorf("card %s: missing %q in %s", id, want, c[2])
			}
		}
		if n := strings.Count(body, `name="id" value="`+id+`"`); n != 2 {
			t.Errorf("device %s: %d selection boxes, want 2 (row and card)", id, n)
		}
	}
	if !regexp.MustCompile(`dcard-meta-part">10\.0\.0\.2:8080</span> · \s*<span class="dcard-meta-part">ESP32</span> · \s*<span class="dcard-meta-part">15\.0\.0</span>`).MatchString(body) {
		t.Error("the card's second line names address, chip and firmware")
	}
	if strings.Contains(body, "<b>x</b>") || !strings.Contains(body, "attic &lt;b&gt;x&lt;/b&gt;") {
		t.Error("a device name must be escaped on the card and in the row")
	}
	if strings.Contains(body, "data-select-all-row") {
		t.Error("the phone list has no Select all row above the cards (#1133): it sits in the bulk bar")
	}
	// the table refresh (SSE) brings the cards along: they must not go stale while the table updates
	_, frag := e.do(t, "GET", "/devices/table", "admin", nil)
	if strings.Count(frag, `<li class="dcard"`) != 2 || !strings.Contains(frag, `data-table="devices"`) {
		t.Errorf("the refreshed list lacks the table or the cards: %s", frag)
	}
	// a power toggle replaces the cell it sits in, in a row and on a card alike
	var cell strings.Builder
	if err := powerCellView(powerCell{Base: "/tf", ID: 1, Enabled: true, Relays: []relayState{{N: 1, State: "ON"}}}).Render(context.Background(), &cell); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cell.String(), `hx-target="closest .pcell"`) || !strings.Contains(body, `class="nowrap p2 pcell"`) || !strings.Contains(body, `<div class="pcell"`) {
		t.Error("the power control must target its own cell (row td or card box)")
	}
}

// Why: a viewer without the bulk actions gets no boxes to tick, on a card as in a row.
func TestDeviceCardsWithoutBulkActionsHaveNoSelection(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(body, `class="dcard"`) || strings.Contains(body, `class="dcard-sel"`) || strings.Contains(body, "data-select-all") {
		t.Errorf("no selection without the command service: %s", body)
	}
}

// Why (design v2, 3.4): the bulk bar offers the prototype's actions in its order and wording:
// Update firmware, Send command, Back up, Restart, Power on, Power off, then Clear. Power on/off
// post the selection to the bulk power route (ON / OFF), which confirms first; Update firmware
// posts it to the new-transaction page. Operators get only what their role may do.
func TestBulkBarActions(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{}; d.OTA = &fakeOTA{}; d.Firmware = &fakeFW{} })
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	bar := regexp.MustCompile(`(?s)<div class="bulkbar" data-bulkbar.*?</div></form>`).FindString(body)
	if bar == "" {
		t.Fatalf("no bulk bar: %s", body)
	}
	labels := regexp.MustCompile(`<button[^>]*>(?:<span class="bulk-lg">)?([^<]+)`).FindAllStringSubmatch(bar, -1)
	var got []string
	for _, m := range labels {
		got = append(got, m[1])
	}
	// Select all (a phone's way to take the whole list, #1133) sits between the actions and Clear; the label match stops at its count
	if strings.Join(got, ",") != "Update firmware,Power on,Power off,Select all ,Clear" {
		t.Errorf("operator bar: %q", got)
	}
	for _, want := range []string{`formaction="/tf/updates/new"`, `name="action" value="ON">Power on`, `name="action" value="OFF">Power off`, `data-bar-label>0 devices selected`} {
		if !strings.Contains(bar, want) {
			t.Errorf("bar: missing %q", want)
		}
	}
	if strings.Contains(bar, "rowmenu") {
		t.Error("the bar has no folded Actions menu any more (design v2)")
	}
}

// Why: the bar posts the ids of the ticked row and card both; the server counts a device once,
// or "Power on" would confirm and switch it twice.
func TestBulkPowerCountsADeviceOnce(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{} })
	_, body := e.do(t, "POST", "/devices/bulk/power", "operator", url.Values{"action": {"ON"}, "id": {"1", "1", "2", "2"}})
	if !strings.Contains(body, "Devices (2)") {
		t.Errorf("want a scope page with 2 devices: %s", body)
	}
}

// Why: Update firmware in the bulk bar posts the selection (a GET would put the form's CSRF
// token into the URL); the new-transaction page opens with exactly those devices ticked.
func TestUpdateFormTakesThePostedSelection(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	_, body := e.do(t, "POST", "/updates/new", "operator", url.Values{"id": {"2"}})
	if !strings.Contains(body, `value="2" aria-label="Select attic" checked`) {
		t.Errorf("device 2 must be preselected: %s", body)
	}
	if strings.Contains(body, `value="1" aria-label="Select kitchen" checked`) {
		t.Error("device 1 was not posted and must not be selected")
	}
}

// Why (#1133, A1-024): at 375 px every filter control had its own line (chips on three rows, Chip, Tag, Sort) and the
// first device started at y 550. Chip, Tag and Sort now share one "Filters" fold, so a phone shows one chip strip and
// one button. The fold stays inside the GET form, so the filter works without JavaScript (the noscript Filter button
// submits it), and its summary counts the Chip and Tag filters that are on, so a person who folded it still sees
// that the list is narrowed.
func TestDevicesToolbarFoldsChipTagAndSortIntoOneFiltersFold(t *testing.T) {
	e := setup(t)
	form := func(path string) string {
		_, body := e.do(t, "GET", path, "admin", nil)
		f := regexp.MustCompile(`(?s)<form class="dv-tools".*?</form>`).FindString(body)
		if f == "" {
			t.Fatalf("%s: no toolbar form: %s", path, body)
		}
		return f
	}
	f := form("/devices")
	loc := regexp.MustCompile(`<details class="dv-filters"[^>]*>`).FindStringIndex(f)
	if loc == nil {
		t.Fatalf("no Filters fold in the toolbar: %s", f)
	}
	if strings.Contains(f[loc[0]:loc[1]], "open") {
		t.Error("the Filters fold starts closed")
	}
	end := strings.Index(f[loc[1]:], "</details>")
	if end < 0 {
		t.Fatalf("the Filters fold never closes: %s", f)
	}
	fold, outside := f[loc[1]:loc[1]+end], f[:loc[0]]+f[loc[1]+end:]
	for _, name := range []string{`name="family"`, `name="tag"`, `name="sort"`} {
		if !strings.Contains(fold, name) || strings.Contains(outside, name) {
			t.Errorf("%s belongs inside the Filters fold, once (the form submits duplicate names twice)", name)
		}
	}
	if strings.Contains(fold, `class="chip`) || !strings.Contains(outside, `class="dv-chips"`) || !strings.Contains(outside, `class="chip`) {
		t.Errorf("the chips stay in their own strip, outside the fold: %s", f)
	}
	if !strings.Contains(f, "<noscript>") || !strings.Contains(f, `type="submit"`) {
		t.Error("without JavaScript the form still needs its Filter button")
	}
	label := regexp.MustCompile(`(?s)<summary[^>]*>(.*?)</summary>`)
	for _, c := range []struct{ path, want string }{
		{"/devices", "Filters"},
		{"/devices?tag=kitchen", "Filters (1)"},
		{"/devices?family=esp8266&tag=kitchen", "Filters (2)"},
		{"/devices?chip=update&q=kit", "Filters"}, // a chip and the search are on screen already, only Chip and Tag fold
	} {
		fl := form(c.path)
		at := strings.Index(fl, `<details class="dv-filters"`)
		if at < 0 {
			t.Fatalf("%s: no Filters fold", c.path)
		}
		m := label.FindStringSubmatch(fl[at:])
		if m == nil || strings.TrimSpace(m[1]) != c.want {
			t.Errorf("%s: the Filters summary reads %q, want %q", c.path, m, c.want)
		}
	}
}

// Why (#1133): "Select all N" was a row above the cards, one more line before the first device on a phone. It is a
// button of the bulk bar now. Every card keeps its own box, so a selection can start without it (the bar shows from
// the first ticked box); select.js writes N into data-select-total, so the number follows a list swap.
func TestBulkBarHoldsSelectAllForThePhone(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{} })
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	bar := regexp.MustCompile(`(?s)<div class="bulkbar" data-bulkbar.*?</div></form>`).FindString(body)
	if bar == "" {
		t.Fatalf("no bulk bar: %s", body)
	}
	if !regexp.MustCompile(`<button type="button"[^>]*data-pick="all"[^>]*>Select all <span data-select-total>2</span></button>`).MatchString(bar) {
		t.Errorf("the bar offers Select all N: %s", bar)
	}
	if strings.Contains(body, "data-select-all-row") {
		t.Error("no Select all row above the cards any more")
	}
	if n := strings.Count(body, `class="dcard-sel"`); n != 2 {
		t.Errorf("every card keeps its own box so a selection can start without Select all: %d boxes", n)
	}
}
