package web

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
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
	if !strings.Contains(body, `data-select-all-row hidden`) || !strings.Contains(body, "Select all 2") {
		t.Error("the phone list offers Select all N (shown by select.js)")
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
	if strings.Join(got, ",") != "Update firmware,Power on,Power off,Clear" {
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
