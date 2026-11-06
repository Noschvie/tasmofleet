package web

import (
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/ota"
)

// before reports whether a occurs earlier in body than b (both must exist).
func before(t *testing.T, body, a, b string) bool {
	t.Helper()
	i, j := strings.Index(body, a), strings.Index(body, b)
	if i < 0 || j < 0 {
		t.Fatalf("missing %q (%d) or %q (%d)", a, i, b, j)
	}
	return i < j
}

// Why (#419-U): the New update page uses the app's column stacks like every other page, so its
// cards follow the Dashboard's content width instead of carrying their own flex widths. Devices
// and Options are the wide stack, Release and the Plan the narrow one; the Plan is a plain card
// (no sticky aside) and the old flex wrapper is gone.
func TestUpdateNewPageUsesColumnStacks(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	o.items[1] = ota.PreviewItem{DeviceID: 1, Decision: "upgrade", Plan: model.PlanStaged, Filename: "tasmota.bin.gz"}
	_, body := e.do(t, "GET", "/updates/new?id=1", "operator", nil)
	for _, want := range []string{`class="cstacks cstacks-3"`, `class="cstack cstack-w2"`, `class="cstack"`, `class="r2 ucard upd-rel"`, `class="r4 ucard upd-aside"`, `<section class="r4 ucard upd-aside"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, gone := range []string{`class="upd-split"`, `upd-main`, `<aside class="ucard upd-aside"`} {
		if strings.Contains(body, gone) {
			t.Errorf("the flex layout %q is still there", gone)
		}
	}
	// the wide stack comes first and holds devices then options; the narrow stack has release, plan
	if !before(t, body, `id="dev-h"`, `id="opt-h"`) || !before(t, body, `id="opt-h"`, `id="rel-h"`) || !before(t, body, `id="rel-h"`, `id="plan-h"`) {
		t.Error("stack order must be Devices, Options | Release, Plan")
	}
	// on a phone the reading order is 1 Devices, 2 Release, 3 Options, then the Plan
	for _, want := range []string{`class="r1 ulist upd-pick"`, `class="r3 ucard upd-opts"`} {
		if !strings.Contains(body, want) {
			t.Errorf("phone order class %q missing", want)
		}
	}
}

// Why (#419-U): the Firmware page keeps the releases and the cache in the narrow stack and the
// selected release plus the custom-firmware drop zone in the wide one. The drop zone is a card of
// its own so that the cache card does not end in a bare dashed box.
func TestFirmwarePageUsesColumnStacks(t *testing.T) {
	e, _, _ := customEnv(t)
	_, body := e.do(t, "GET", "/firmware", "admin", nil)
	for _, want := range []string{`class="cstacks cstacks-3"`, `class="r1 ulist fw-relcard"`, `class="r3 ulist fw-cache"`, `class="r4 ulist fw-upcard"`, ">Custom firmware</h2>"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, gone := range []string{"fw-grid", "fw-main"} {
		if strings.Contains(body, gone) {
			t.Errorf("%q is still there", gone)
		}
	}
	card := fwSection(t, body, "cache")
	if strings.Contains(card, "fw-drop") {
		t.Error("the drop zone belongs to the Custom firmware card, not the cache card")
	}
	if !before(t, body, `id="fw-rels-h"`, `id="fw-cache-h"`) {
		t.Error("narrow stack: Releases then Cached files")
	}
	// the selected release is the first card of the wide stack, ahead of Custom firmware
	_, sel := fwEnv(t, &fakeMgrFW{releases: baseRelease()}).do(t, "GET", "/firmware", "admin", nil)
	if !strings.Contains(sel, `class="r2 ucard fw-sel"`) || !before(t, sel, `class="cstack cstack-w2"`, `id="fw-sel-h"`) {
		t.Error("the selected release belongs to the wide stack")
	}
	_, op := e.do(t, "GET", "/firmware", "operator", nil)
	if strings.Contains(op, "fw-upcard") || strings.Contains(op, "Custom firmware") {
		t.Error("operators get no Custom firmware card")
	}
}

// Why (#419-U): a cached file is two lines in the narrow stack (name and Delete, then release,
// size, date and the short checksum); every datum is in one of the four cells.
func TestFirmwareCachedRowIsTwoLines(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{
		{ID: 2, Release: "16.1.0", Filename: "tasmota32.bin", Family: model.ChipESP32, Variant: "tasmota", Size: 1024, SHA256: "9f2c" + strings.Repeat("b", 56) + "e77c", Source: "ota.tasmota.com", FetchedAt: time.Now()},
	}}
	e := fwEnv(t, fw)
	_, body := e.do(t, "GET", "/firmware", "admin", nil)
	row := fwSection(t, body, "cache")
	if !strings.Contains(row, `class="fw-crow fw-crow-2"`) {
		t.Fatalf("no two-line row:\n%s", row)
	}
	v := row[strings.Index(row, `class="fw-c-v`):]
	v = v[:strings.Index(v, "</span>")]
	if !strings.Contains(v, "16.1.0") || !strings.Contains(v, "<time") {
		t.Errorf("version, size and date share the second line: %s", v)
	}
	if !before(t, row, `class="fw-c-a"`, `class="fw-c-v`) || !before(t, row, `class="fw-c-v`, `class="fw-c-h`) {
		t.Error("cells must be in the order name, action, details, checksum")
	}
}

// Why (#419-U): the release page's notes are the wide stack, the file lists the narrow one.
func TestReleasePageUsesColumnStacks(t *testing.T) {
	e := relEnv(t, newFeed(feedReleases()))
	_, body := e.do(t, "GET", "/firmware/releases/16.2.0", "admin", nil)
	for _, want := range []string{`class="cstacks cstacks-3"`, `class="cstack cstack-w2"`, `class="r1 ucard rel-notes"`, `class="r2 ulist rel-files"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, gone := range []string{"rel-grid", "rel-aside"} {
		if strings.Contains(body, gone) {
			t.Errorf("%q is still there", gone)
		}
	}
}
