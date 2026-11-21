package web

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/clock"
	"github.com/hellwand/tasmofleet/internal/importer"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// #1073: meaning the page carries only in a title attribute (no hover on a phone, not read out by default) or only
// in a colour is not on screen. These tests read the rendered HTML with every tag, and so every attribute, removed:
// what is left is what a person without a hover sees.

// visibleText is body as plain text: tags and their attributes (title included) are gone.
func visibleText(body string) string {
	return strings.Join(strings.Fields(tagRe.ReplaceAllString(body, " ")), " ")
}

// Why: the Status badge's title explained a state (Stale after 11 minutes, Auth failed = the login was refused,
// MQTT down = HTTP only), and a phone has no hover. Every state that is not plain Online carries a short reason as
// text under its badge; the SSE fragment of a health event is the same statusCell, so the page and the live swap
// cannot differ. The Stale text names the configured threshold, like its title.
func TestStatusCellCarriesItsReasonAsText(t *testing.T) {
	h := &handler{d: Deps{Clock: clock.Real{}, StaleAfter: func() time.Duration { return 42 * time.Minute }}}
	now := time.Now()
	for _, c := range []struct {
		name string
		hl   model.Health
		want string
	}{
		{"stale", model.Health{State: model.HealthStale, LastSeen: now}, "no message for over 42m"},
		{"offline", model.Health{State: model.HealthOffline, LastSeen: now.Add(-time.Hour)}, "no answer"},
		{"auth failed", model.Health{State: model.HealthAuthFailed, LastSeen: now}, "login refused"},
		{"unknown", model.Health{State: model.HealthUnknown}, "not heard from yet"},
		{"mqtt down", model.Health{State: model.HealthOnline, LastSeen: now, SeenHTTP: now, LWT: "Offline"}, "MQTT down · HTTP only"},
	} {
		var b bytes.Buffer
		if err := h.statusCell(c.hl).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		if got := visibleText(b.String()); !strings.Contains(got, c.want) {
			t.Errorf("%s: the reason %q is not visible text, only %q in:\n%s", c.name, c.want, got, b.String())
		}
	}
	var b bytes.Buffer
	if err := h.statusCell(model.Health{State: model.HealthOnline, LastSeen: now, SeenMQTT: now}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got := visibleText(b.String()); got != "Online" {
		t.Errorf("a plain Online device needs no reason, got %q", got)
	}
}

// Why: the badges "update" and "1 deviation" said what they meant only in a title. The list now says which version
// the update is to ("Update 16.2.0": the one the cached feed names, the same fleetFacts.Latest the Firmware behind
// tile compares with) and what differs ("1 off policy"), in the table row and in the phone card alike.
func TestDeviceListSaysWhichUpdateAndWhatDiffers(t *testing.T) {
	fw := newFeed(feedReleases())
	e := setupWith(t, func(d *Deps) {
		d.Firmware, d.Artifacts = fw, fw
		d.Policy = &fakePolicy{rep: PolicyReport{Findings: []policy.Finding{
			{DeviceID: 2, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Have: "60"},
			{DeviceID: 2, Rule: policy.Rule{Command: "SetOption1", Want: "1"}, Have: "0"},
			{DeviceID: 1, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Have: "60"},
		}}}
	})
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	row := between(body, `<tr data-id="2">`, "</tr>")
	card := between(body, `<li class="dcard" data-id="2">`, "</li>")
	for name, part := range map[string]string{"row": row, "card": card} {
		text := visibleText(part)
		for _, want := range []string{"Update 16.2.0", "2 off policy"} {
			if !strings.Contains(text, want) {
				t.Errorf("device 2 %s: %q is not visible text: %s", name, want, text)
			}
		}
	}
	if text := visibleText(between(body, `<tr data-id="1">`, "</tr>")); !strings.Contains(text, "1 off policy") {
		t.Errorf("one deviation reads %q", text)
	}
	if strings.Contains(body, ">update</span>") || strings.Contains(body, "deviation</span>") {
		t.Error("a bare 'update' or 'deviation' badge is back")
	}
}

// Why: "Reject" in danger red made a normal outcome of a CSV (an address outside the device networks) look like a
// failure, and the three plan badges were a word plus a colour. Reject is a warning and every badge of the plan, its
// counts and its rows, carries a glyph beside the word. A failed device after Apply stays danger.
func TestImportPlanRejectIsWarnWithWordAndMark(t *testing.T) {
	v := planView(importer.Plan{
		Rows: []importer.Row{
			{Device: model.Device{Name: "a", Address: "10.0.0.1"}, Action: importer.ActionCreate},
			{Device: model.Device{Name: "b", Address: "10.0.0.2"}, Action: importer.ActionMerge},
			{Device: model.Device{Name: "c", Address: "8.8.8.8"}, Action: importer.ActionSkip, Reason: "Outside the device networks"},
		},
	})
	var b bytes.Buffer
	if err := importBodyPart(importBody{Base: "/tf", Plan: v}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	if strings.Contains(page, "t-danger") {
		t.Errorf("a rejected row is user input, not a failure: no danger badge expected:\n%s", between(page, `class="add-plan"`, "</section>"))
	}
	for _, word := range []string{"Create", "Merge", "Reject", "1 create", "1 merge", "1 reject"} {
		re := regexp.MustCompile(`<span class="bdg t-(?:ok|accent|warn)"><span class="bdg-mark" aria-hidden="true">[^<]+</span> ` + word + `</span>`)
		if !re.MatchString(page) {
			t.Errorf("the %q badge has no glyph beside its word", word)
		}
	}
	if !regexp.MustCompile(`class="bdg t-warn"><span class="bdg-mark" aria-hidden="true">[^<]+</span> Reject<`).MatchString(page) {
		t.Error("Reject is not a warn badge")
	}
	rep := reportView(importer.Report{Lines: []importer.Line{{Name: "x", Status: "failed"}, {Name: "y", Status: "skipped"}}})
	if rep.Rows[0].Tone != ui.ToneDanger || rep.Rows[1].Tone != ui.ToneWarn {
		t.Errorf("after Apply Failed stays danger and Rejected is warn: %+v", rep.Rows)
	}
}

// Why: a download that fails said why only in a title, so the failed state told a touch or keyboard user nothing
// about what to do. The reason is a line under the file, on the Firmware page and on the release page, with Retry.
func TestDownloadFailedSaysWhyAsText(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), failFile: "tasmota.bin.gz"}
	e := fwEnv(t, fw)
	_, body := e.do(t, "POST", "/firmware/fetch", "admin", map[string][]string{"release": {"16.0.0"}, "filename": {"tasmota.bin.gz"}})
	card := visibleText(fwSection(t, body, "release"))
	if !strings.Contains(card, "Download failed") || !strings.Contains(card, "SHA-256 differs between sources") {
		t.Errorf("Firmware page: the reason is not visible text: %s", card)
	}

	rfw := newFeed(feedReleases())
	rfw.failFile = "tasmota32.bin"
	re := relEnv(t, rfw)
	_, page := re.do(t, "POST", "/firmware/fetch", "admin", map[string][]string{"release": {"16.2.0"}, "filename": {"tasmota32.bin"}, "from": {"release"}})
	files := visibleText(between(page, `id="rel-files-h"`, "</section>"))
	if !strings.Contains(files, "Download failed") || !strings.Contains(files, "SHA-256 differs between sources") {
		t.Errorf("release page: the reason is not visible text: %s", files)
	}
}

// Why: the Actual value of a deviating row was marked by the amber tint alone. The cell now says "differs" in
// words for a screen reader and carries a not-equal sign for everyone else.
func TestDeviationActualIsMarkedBeyondColour(t *testing.T) {
	body := devPage(t, "admin", "", devReport())
	cell := between(body, `<td data-label="Actual"`, "</td>")
	if !strings.Contains(cell, `<span class="pol-dev-mark" aria-hidden="true">≠</span>`) || !strings.Contains(cell, `<span class="sr-only">Differs: </span>`) {
		t.Errorf("the Actual cell has no marker beyond its colour: %s", cell)
	}
}
