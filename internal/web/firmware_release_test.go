package web

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// feedFW is a firmware cache whose in-memory feed is cached (nil = not loaded) and that counts
// every request that could go upstream.
type feedFW struct {
	*fakeMgrFW
	cached  []firmware.Release
	fetches atomic.Int32
}

func (f *feedFW) CachedReleases() []firmware.Release { return f.cached }
func (f *feedFW) Releases(ctx context.Context) ([]firmware.Release, error) {
	f.fetches.Add(1)
	return f.fakeMgrFW.Releases(ctx)
}

// feedReleases is a feed from 15.0.0 to 16.2.0; the fleet's oldest version is 15.0.0 (relFleet).
func feedReleases() []firmware.Release {
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }
	return []firmware.Release{
		{Version: "16.2.0", Published: day(10, 6), URL: "https://github.com/arendst/Tasmota/releases/tag/v16.2.0", Files: []string{"tasmota.bin.gz", "tasmota-minimal.bin.gz", "tasmota32.bin"},
			Notes: "Maintenance release.\n\n### Breaking Changed\n- `SetOption65` removed\n- minimal first\n\n### Added\n- Berry thing\n\n### Fixed\n- dimmer flicker"},
		{Version: "16.1.0", Published: day(8, 18), Files: []string{"tasmota.bin.gz"}, Notes: "### Added\n- relays stagger"},
		{Version: "16.0.0", Published: day(6, 30), Files: []string{"tasmota.bin.gz"}, Notes: "### Fixed\n- crash"},
		{Version: "15.0.0", Published: day(2, 20), Files: []string{"tasmota.bin.gz"}, Notes: "### Added\n- the oldest"},
	}
}

func relFleet() []model.Device {
	return []model.Device{
		{ID: 1, Name: "lamp", Address: "10.0.0.1", Identity: model.Identity{Family: model.ChipESP8266, Version: "15.0.0", Variant: "tasmota"}},
		{ID: 2, Name: "plug", Address: "10.0.0.2", Identity: model.Identity{Family: model.ChipESP8266, Version: "16.1.0", Variant: "tasmota"}},
		{ID: 3, Name: "dimmer", Address: "10.0.0.3", Identity: model.Identity{Family: model.ChipESP32, Version: "16.1.0", Variant: "tasmota"}},
	}
}

func relEnv(t *testing.T, fw *feedFW) *env {
	t.Helper()
	return setupWith(t, func(d *Deps) {
		d.Firmware, d.Artifacts = fw, fw
		d.Inventory = newInv(relFleet()...)
		d.Jobs = &fakeJobs{}
		d.FirmwareSource = "GitHub releases"
	})
}

func newFeed(cached []firmware.Release) *feedFW {
	return &feedFW{fakeMgrFW: &fakeMgrFW{releases: feedReleases()}, cached: cached}
}

// Why: a page view must never wait for or hit the upstream feed (rate limits, a slow GitHub):
// it reads only the feed in memory. Not loaded yet is an empty state, not an error and not a
// fetch; a version the loaded feed does not have is a 404, not an empty page.
func TestReleasePageReadsOnlyTheCachedFeed(t *testing.T) {
	fw := newFeed(nil)
	e := relEnv(t, fw)
	res, body := e.do(t, "GET", "/firmware/releases/16.2.0", "operator", nil)
	if res.StatusCode != 200 || !strings.Contains(body, "Release list not loaded yet") {
		t.Fatalf("nil feed: status %d, want 200 with the empty state", res.StatusCode)
	}
	if n := fw.fetches.Load(); n != 0 {
		t.Errorf("the page asked upstream %d times", n)
	}
	fw.cached = feedReleases()
	if res, _ := e.do(t, "GET", "/firmware/releases/99.0.0", "operator", nil); res.StatusCode != 404 {
		t.Errorf("unknown version: status %d, want 404", res.StatusCode)
	}
	if res, _ := e.do(t, "GET", "/firmware/releases/16.2.0?view=since", "operator", nil); res.StatusCode != 200 {
		t.Errorf("since view: status %d", res.StatusCode)
	}
	if n := fw.fetches.Load(); n != 0 {
		t.Errorf("the page asked upstream %d times", n)
	}
}

// Why: the release page is what an admin reads before updating: the whole notes with the
// breaking section in its box, the files the fleet needs with their cache state, and how many
// devices are still on each older version.
func TestReleasePageShowsNotesFilesAndOlderVersions(t *testing.T) {
	fw := newFeed(feedReleases())
	fw.arts = []model.Artifact{{ID: 1, Release: "16.2.0", Filename: "tasmota.bin.gz", Size: 658 * 1024, Source: "github"}}
	e := relEnv(t, fw)
	_, body := e.do(t, "GET", "/firmware/releases/16.2.0", "admin", nil)
	main := strings.ReplaceAll(body[strings.Index(body, `<main id="main">`):], "<wbr>", "") // file names carry soft break points
	for _, want := range []string{"Tasmota 16.2.0", "Latest", "Released 6 Oct 2026 · from GitHub releases", "View on GitHub",
		`class="unotes-breaking rel-breaking"`, "<code>SetOption65</code>", "Berry thing",
		"Files this fleet needs", "tasmota.bin.gz", "ESP8266 · 2 devices · 658.0 KiB", "Cached", "Prefetch tasmota32.bin", `name="from" value="release"`,
		"Still on older versions", "16.1.0", "15.0.0", "Changes since 15.0.0"} {
		if !strings.Contains(main, want) {
			t.Errorf("release page lacks %q", want)
		}
	}
	if strings.Contains(main, "Breaking Changed") {
		t.Error("the breaking heading is drawn besides the box title")
	}
	// 16.1.0 (two devices) before 15.0.0 (one)
	if i, j := strings.Index(main, `<span class="mono">16.1.0</span> <span>2 devices</span>`), strings.Index(main, `<span class="mono">15.0.0</span> <span>1 device</span>`); i < 0 || j < 0 || i > j {
		t.Errorf("older versions not counted newest first (%d, %d)", i, j)
	}
}

// Why: "Changes since" lists exactly what a jump from the oldest version brings: every release
// after it up to the selected one, newest first, the oldest itself left out (the fleet runs it),
// and breaking items flagged so they are not missed.
func TestReleaseSinceView(t *testing.T) {
	e := relEnv(t, newFeed(feedReleases()))
	_, body := e.do(t, "GET", "/firmware/releases/16.2.0?view=since", "operator", nil)
	main := body[strings.Index(body, `<main id="main">`):]
	if strings.Contains(main, `id="rel-s-15.0.0"`) {
		t.Error("the oldest version in the fleet is listed")
	}
	a, b, c := strings.Index(main, `id="rel-s-16.2.0"`), strings.Index(main, `id="rel-s-16.1.0"`), strings.Index(main, `id="rel-s-16.0.0"`)
	if a < 0 || b < 0 || c < 0 || a >= b || b >= c {
		t.Fatalf("since list not newest first: %d %d %d", a, b, c)
	}
	first := main[a:b]
	if !strings.Contains(first, "2 breaking changes") || strings.Count(first, "rel-srow-brk") != 2 {
		t.Errorf("16.2.0 does not flag its two breaking items: %s", first)
	}
	if strings.Contains(main[b:], "breaking change") {
		t.Error("a release without a breaking section is flagged")
	}
	if !strings.Contains(main, `aria-current="page">Changes since 15.0.0`) {
		t.Error("the since tab is not current")
	}
}

// Why: release notes are third-party text; on the release page as on the firmware page nothing
// in them may become markup.
func TestReleasePageEscapesNotes(t *testing.T) {
	rels := feedReleases()
	rels[0].Notes = "### Breaking\n- <script>alert(1)</script>\n\n<img src=x onerror=alert(2)> [a](javascript:alert(3))"
	e := relEnv(t, newFeed(rels))
	for _, path := range []string{"/firmware/releases/16.2.0", "/firmware/releases/16.2.0?view=since"} {
		_, body := e.do(t, "GET", path, "operator", nil)
		main := body[strings.Index(body, `<main id="main">`):]
		for _, bad := range []string{"<script>alert", "<img src=x", `href="javascript`} {
			if strings.Contains(main, bad) {
				t.Errorf("%s: %q reached the page", path, bad)
			}
		}
	}
}

// Why: Prefetch and Retry on the release page answer with the release page (the admin stays
// where they were), with the failure on the file's row.
func TestReleasePageFetchAnswersWithItself(t *testing.T) {
	fw := newFeed(feedReleases())
	fw.failFile = "tasmota32.bin"
	e := relEnv(t, fw)
	form := map[string][]string{"release": {"16.2.0"}, "filename": {"tasmota32.bin"}, "from": {"release"}}
	res, body := e.do(t, "POST", "/firmware/fetch", "admin", form)
	if res.StatusCode != 502 || !strings.Contains(body, "Files this fleet needs") || !strings.Contains(body, "Download failed") || !strings.Contains(body, "Retry tasmota32.bin") {
		t.Errorf("failed fetch from the release page: status %d, want 502 and the release page with Retry", res.StatusCode)
	}
	form["filename"] = []string{"tasmota.bin.gz"}
	if res, body := e.do(t, "POST", "/firmware/fetch", "admin", form); res.StatusCode != 200 || !strings.Contains(body, "tasmota.bin.gz 16.2.0 cached") || !strings.Contains(body, "Still on older versions") {
		t.Errorf("fetch from the release page: status %d, want the release page with the toast", res.StatusCode)
	}
}

// Why: the firmware page's selected release leads to its release page and, when the fleet runs
// an older version, straight to "Changes since <oldest>".
func TestFirmwareCardLinksTheReleasePage(t *testing.T) {
	e := relEnv(t, newFeed(feedReleases()))
	_, body := e.do(t, "GET", "/firmware", "operator", nil)
	card := fwSection(t, body, "release")
	if !strings.Contains(card, `href="/tf/firmware/releases/16.2.0"`) || !strings.Contains(card, `href="/tf/firmware/releases/16.2.0?view=since">Changes since 15.0.0`) {
		t.Errorf("firmware card lacks the release page links: %s", card)
	}
}
