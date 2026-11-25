package web

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/firmware"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/ota"
)

// Tests of the firmware page as the design v2 port (WP P5) builds it: release cards with the
// selected release, the files this fleet needs, the cached files, the check status line.

// checkFW is a firmware cache that reports its index status and can be refreshed.
type checkFW struct {
	*fakeMgrFW
	st        firmware.IndexStatus
	refreshed []firmware.Release // the releases after Refresh
	err       error
}

func (c *checkFW) IndexStatus() firmware.IndexStatus  { return c.st }
func (c *checkFW) CachedReleases() []firmware.Release { return c.releases }
func (c *checkFW) Refresh(context.Context) error {
	if c.err != nil {
		c.st.LastError, c.st.LastAttempt = c.err.Error(), time.Now()
		return c.err
	}
	c.releases = c.refreshed
	c.st = firmware.IndexStatus{Loaded: true, LastSuccess: time.Now(), LastAttempt: time.Now()}
	return nil
}

// fwEnvWithUpdates is fwEnv with the update engine, so the page offers "Update N devices".
func fwEnvWithUpdates(t *testing.T, fw *fakeMgrFW, fleet []model.Device) *env {
	t.Helper()
	return setupWith(t, func(d *Deps) {
		d.Firmware, d.Artifacts = fw, fw
		d.Inventory = newInv(fleet...)
		d.Jobs = &fakeJobs{}
		d.OTA = &fakeOTA{items: map[model.DeviceID]ota.PreviewItem{}}
	})
}

func checkEnv(t *testing.T, fw *checkFW, feed string) *env {
	t.Helper()
	return setupWith(t, func(d *Deps) {
		d.Firmware, d.Artifacts = fw, fw
		d.Inventory = newInv(mixedFleet()...)
		d.Jobs = &fakeJobs{}
		d.FirmwareSource, d.FirmwareFeed = "GitHub releases (arendst/Tasmota)", feed
	})
}

// fwSection returns the part of body from the element with the id to the end of its section.
func fwSection(t *testing.T, body, id string) string {
	t.Helper()
	i := strings.Index(body, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no element %q in page:\n%s", id, body)
	}
	j := strings.Index(body[i:], "</section>")
	if j < 0 {
		return body[i:]
	}
	return body[i : i+j]
}

// Why: the page opens on the newest release, the one an update would install. Its notes are an
// excerpt with the whole notes one click away (a dialog, or in the page without JavaScript), and
// "Update N devices" counts the devices that can actually take it.
func TestFirmwarePageOpensOnTheNewestRelease(t *testing.T) {
	rels := baseRelease()
	rels[0].Notes, rels[1].Notes = "## Added\n- new thing\n- second thing", "- old thing"
	rels[0].Published = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	e := fwEnvWithUpdates(t, &fakeMgrFW{releases: rels}, mixedFleet())
	_, body := e.do(t, "GET", "/firmware", "operator", nil)
	card := fwSection(t, body, "release")
	for _, want := range []string{"Tasmota 16.1.0", "Released <time", "6 Oct 2026", "<li>new thing</li>", "Update 1 device…", `href="/tf/updates/new?release=16.1.0"`, `data-dialog="dlg-notes"`} {
		if !strings.Contains(card, want) {
			t.Errorf("selected release card lacks %q:\n%s", want, card)
		}
	}
	// the newest card is the current one and carries the Latest badge; 16.0.0 is a plain card
	if !regexp.MustCompile(`id="release-16\.1\.0"[^>]*aria-current="true"`).MatchString(body) || strings.Contains(body, `id="release-16.0.0" href="/tf/firmware?release=16.0.0" aria-current`) {
		t.Errorf("the newest release must be the selected card:\n%s", body)
	}
	if !strings.Contains(body, `<span class="bdg t-ok">Latest</span>`) {
		t.Error("the newest stable release is not marked Latest")
	}
	// whole notes: in a closed dialog, and in the page for ?notes=1
	if !strings.Contains(body, `<dialog id="dlg-notes"`) || strings.Contains(body, "<dialog open") || strings.Contains(body, `id="release-notes"`) {
		t.Errorf("notes must sit in a closed dialog:\n%s", body)
	}
	if _, inline := e.do(t, "GET", "/firmware?notes=1", "operator", nil); !strings.Contains(fwSection(t, inline, "release"), `id="release-notes"`) {
		t.Error("?notes=1 does not show the notes in the page (the no-JS Release notes)")
	}
	// another release is a link away and keeps its own figures
	_, old := e.do(t, "GET", "/firmware?release=16.0.0", "operator", nil)
	if card := fwSection(t, old, "release"); !strings.Contains(card, "Tasmota 16.0.0") || !strings.Contains(card, "old thing") || !strings.Contains(card, "Update 0 devices…") || !strings.Contains(card, `aria-disabled="true"`) {
		t.Errorf("?release=16.0.0 card:\n%s", card)
	}
}

// Why: with more releases than cards, the older ones are one toggle away and selecting one of
// them keeps the list open (otherwise the selected release would not be visible on the left).
func TestFirmwarePageOlderReleasesToggle(t *testing.T) {
	var rels []firmware.Release
	for _, v := range []string{"16.1.0", "16.0.0", "15.9.0", "15.8.0", "15.7.0"} {
		rels = append(rels, firmware.Release{Version: v, Files: []string{"tasmota.bin.gz"}})
	}
	e := fwEnv(t, &fakeMgrFW{releases: rels})
	_, body := e.do(t, "GET", "/firmware", "operator", nil)
	if !strings.Contains(body, "Show 2 older releases") || strings.Contains(body, `class="fw-chip"`) {
		t.Errorf("older releases must be folded:\n%s", body)
	}
	_, open := e.do(t, "GET", "/firmware?older=1", "operator", nil)
	if !strings.Contains(open, "Hide older releases") || strings.Count(open, `class="fw-chip"`) != 2 {
		t.Errorf("?older=1 must list the two older releases:\n%s", open)
	}
	_, sel := e.do(t, "GET", "/firmware?release=15.7.0", "operator", nil)
	if !strings.Contains(sel, "Hide older releases") || !strings.Contains(sel, "Tasmota 15.7.0") {
		t.Errorf("an older selected release must keep the list open:\n%s", sel)
	}
}

// Why: "Files this fleet needs" decides whether an update can start without downloads: every
// file with its chip and devices, what is cached, and for admins a Prefetch per file and for
// the whole fleet. Operators see the state but no download buttons.
func TestFirmwarePageFilesThisFleetNeeds(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{{ID: 1, Release: "16.1.0", Filename: "tasmota32.bin", Size: 2048, Family: model.ChipESP32}}}
	e := fwEnv(t, fw)
	_, body := e.do(t, "GET", "/firmware", "admin", nil)
	card := strings.ReplaceAll(fwSection(t, body, "release"), "<wbr>", "") // file names carry soft break points
	for file, chip := range map[string]string{"tasmota32.bin": "ESP32", "tasmota32c3.bin": "ESP32-C3", "tasmota-minimal.bin.gz": "ESP8266"} {
		if !regexp.MustCompile(`<span class="fw-file-n">` + regexp.QuoteMeta(file) + `</span>\s*<span class="fw-file-c">` + chip + `</span>`).MatchString(card) {
			t.Errorf("%s is not listed with its chip %s", file, chip)
		}
	}
	for _, want := range []string{
		"2.0\u00a0KiB", // the cached file's size
		`<span class="bdg t-ok">Cached</span>`,
		`aria-label="Prefetch tasmota32c3.bin"`,
		"Prefetch for my fleet",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("missing %q in:\n%s", want, card)
		}
	}
	if strings.Contains(card, `aria-label="Prefetch tasmota32.bin"`) {
		t.Error("a cached file is offered for download again")
	}
	// the fleet's position against the release is in the notes
	if !strings.Contains(body, `Older than this release (1):`) || !strings.Contains(body, `<a href="/tf/devices/1" title="lamp-1m">lamp-1m</a>`) {
		t.Errorf("the fleet lists of the release are missing:\n%s", body)
	}
	_, op := e.do(t, "GET", "/firmware", "operator", nil)
	if card := fwSection(t, op, "release"); strings.Contains(card, "Prefetch") || !strings.Contains(card, "Not cached") {
		t.Errorf("operators see the state, no downloads:\n%s", card)
	}
}

// Why (finding 30): devices that never reported a version are in no count; the page says so
// instead of silently leaving them out, and unknown hardware is older but never "to update".
func TestFirmwarePageCountsKnownDevicesOnly(t *testing.T) {
	fleet := append(mixedFleet()[:1],
		model.Device{ID: 5, Name: "mystery-chip", Address: "10.0.0.5", Identity: model.Identity{Version: "15.0.0", Variant: "tasmota"}}, // unknown hardware
		model.Device{ID: 6, Name: "silent", Address: "10.0.0.6"},                                                                        // never answered
	)
	e := fwEnvWithUpdates(t, &fakeMgrFW{releases: baseRelease()}, fleet)
	_, body := e.do(t, "GET", "/firmware", "operator", nil)
	card := fwSection(t, body, "release")
	if !regexp.MustCompile(`No file needed for:[^<]*silent \(`).MatchString(card) {
		t.Errorf("the device without a version is not named on the card:\n%s", card)
	}
	for _, want := range []string{"Update 1 device…", "Older than this release (2, 1 updatable)", "Version unknown (1)"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "Update 2 devices") {
		t.Error("unknown hardware counted as updatable")
	}
}

// Why: the cached files are where an admin frees space. Each row says what the file is and
// whether a transaction needs it; a needed file links its transaction and has no Delete, the
// others open a confirmation that says who would download it again.
func TestFirmwareCachedFiles(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{
		{ID: 1, Release: "16.1.0", Filename: "tasmota-minimal.bin.gz", Family: model.ChipESP8266, Variant: "minimal", Size: 2048, SHA256: strings.Repeat("a", 60) + "41ab", Source: "github", FetchedAt: time.Now().Add(-2 * time.Hour)},
		{ID: 2, Release: "16.1.0", Filename: "tasmota32.bin", Family: model.ChipESP32, Variant: "tasmota", Size: 1024, SHA256: "9f2c" + strings.Repeat("b", 56) + "e77c", Source: "ota.tasmota.com", FetchedAt: time.Now()},
	}, inUse: map[model.ArtifactID][]model.JobID{1: {7}}}
	e := fwEnv(t, fw)
	_, body := e.do(t, "GET", "/firmware", "admin", nil)
	list := fwSection(t, body, "cache")
	for _, want := range []string{
		"2 files · 3.0\u00a0KiB", "Delete 1 unused", `name="id" value="2"`,
		`href="/tf/transactions/5"`, "Needed by #5",
		"sha256 9f2c…e77c", "ESP32 / tasmota, from ota.tasmota.com; installed on dimmer",
		`href="/tf/firmware/artifacts/2/delete" data-dialog="dlg-file-2"`,
	} {
		if !strings.Contains(list, want) {
			t.Errorf("missing %q in:\n%s", want, list)
		}
	}
	if strings.Contains(list, "/tf/firmware/artifacts/1/delete") || strings.Contains(list, `name="id" value="1"`) || strings.Contains(body, `id="dlg-file-1"`) {
		t.Error("a file a transaction needs is offered for deletion")
	}
	// the dialog of tasmota32.bin: lamp-1m... no: dimmer runs 16.1.0, nobody older uses tasmota32.bin
	dlg := body[strings.Index(body, `id="dlg-file-2"`):]
	dlg = dlg[:strings.Index(dlg, "</dialog>")]
	for _, want := range []string{"Delete tasmota32.bin (16.1.0) from the cache?", "No device needs this file for an update to 16.1.0.", "Frees", "Checksum", `data-copy-text="9f2c`, "Delete file"} {
		if !strings.Contains(dlg, want) {
			t.Errorf("delete dialog lacks %q:\n%s", want, dlg)
		}
	}
	// operators see the list, not one control that changes it
	_, op := e.do(t, "GET", "/firmware", "operator", nil)
	if l := fwSection(t, op, "cache"); strings.Contains(l, "Delete") || strings.Contains(l, `name="id"`) || strings.Contains(op, "dlg-file-") {
		t.Errorf("operators must not get delete controls:\n%s", l)
	}
}

// Why: the status line answers "how fresh is this list" without logs, and a failed check says
// what still works. Error texts can carry a source URL; neither it nor any credential in it reaches the page.
func TestFirmwareCheckStatusLine(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name      string
		st        firmware.IndexStatus
		want      []string
		forbidden []string
	}{
		{"first check running", firmware.IndexStatus{}, []string{"Checking GitHub…", ">Checking…</button>", "Index loading…"}, []string{"not reachable"}},
		{"fresh", firmware.IndexStatus{Loaded: true, LastSuccess: now.Add(-5 * time.Minute), LastAttempt: now.Add(-5 * time.Minute)},
			[]string{"Last checked", "5m ago", "Check for new releases now", "16.1.0"}, []string{"not reachable", "Checking GitHub…</span>"}},
		{"failed with a list", firmware.IndexStatus{Loaded: true, LastSuccess: now.Add(-3 * time.Hour), LastAttempt: now.Add(-time.Minute), LastError: "source github: GET https://bot:hunter2@api.example/releases: answered 503"},
			[]string{"Last check failed", "Could not check for new releases.", "Cached files and the last list still work.", ">Try again</button>"},
			[]string{"hunter2", "bot:", "answered 503", "api.example", "not reachable"}},
		{"failed before any list", firmware.IndexStatus{LastAttempt: now, LastError: "source github: dial tcp: refused"},
			[]string{"Could not check for new releases.", "Cached files still work; the release list appears after the first successful check."}, []string{"Index loading", "refused", "not reachable"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fw := &checkFW{fakeMgrFW: &fakeMgrFW{releases: baseRelease()}, st: c.st}
			e := checkEnv(t, fw, "GitHub")
			_, body := e.do(t, "GET", "/firmware", "admin", nil)
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("missing %q", w)
				}
			}
			for _, f := range c.forbidden {
				if strings.Contains(body, f) {
					t.Errorf("unexpected %q", f)
				}
			}
			if !strings.Contains(body, "Source: GitHub releases (arendst/Tasmota)") {
				t.Error("the subtitle does not name the source")
			}
			// operators read the state, only admins can start a check
			_, op := e.do(t, "GET", "/firmware", "operator", nil)
			if strings.Contains(op, "/firmware/refresh") {
				t.Error("operators get the check button")
			}
		})
	}
}

// Why: while the first refresh after start runs, rendering the page must not fetch the index
// (it would block the page behind the slow upstream).
func TestFirmwarePageDoesNotFetchWhileChecking(t *testing.T) {
	fw := &statusFW{}
	body := firmwarePageBody(t, fw)
	if fw.releases.Load() != 0 || !strings.Contains(body, "Index loading") {
		t.Fatalf("fetched %d times, page:\n%s", fw.releases.Load(), body)
	}
}

// Why: "Check for new releases now" answers with what it found, as a toast; a failed check is
// a 502 with the reason in the banner and the old list still shown.
func TestFirmwareCheckNow(t *testing.T) {
	newer := append([]firmware.Release{{Version: "16.2.0", Files: []string{"tasmota.bin.gz"}}}, baseRelease()...)
	fw := &checkFW{fakeMgrFW: &fakeMgrFW{releases: baseRelease()}, st: firmware.IndexStatus{Loaded: true, LastSuccess: time.Now(), LastAttempt: time.Now()}, refreshed: baseRelease()}
	e := checkEnv(t, fw, "GitHub")
	if r, _ := e.do(t, "POST", "/firmware/refresh", "operator", nil); r.StatusCode != 403 {
		t.Errorf("operator check = %d", r.StatusCode)
	}
	r, body := e.do(t, "POST", "/firmware/refresh", "admin", nil)
	if r.StatusCode != 200 || !strings.Contains(body, `data-flash><span class="banner-dot" aria-hidden="true"></span><div class="banner-text">No release newer than 16.1.0</div>`) {
		t.Errorf("check without news = %d:\n%s", r.StatusCode, body)
	}
	fw.refreshed = newer
	if _, body = e.do(t, "POST", "/firmware/refresh", "admin", nil); !strings.Contains(body, "New release 16.2.0 found") {
		t.Errorf("the new release is not announced:\n%s", body)
	}
	fw.err = errors.New("source github: answered 503")
	r, body = e.do(t, "POST", "/firmware/refresh", "admin", nil)
	if r.StatusCode != 502 || !strings.Contains(body, "Could not check for new releases.") || strings.Contains(body, "answered 503") || !strings.Contains(body, "Tasmota 16.2.0") {
		t.Errorf("failed check = %d:\n%s", r.StatusCode, body)
	}
}

func TestCheckNotice(t *testing.T) {
	for _, c := range []struct{ before, after, want string }{
		{"16.1.0", "16.1.0", "No release newer than 16.1.0"},
		{"16.1.0", "16.2.0", "New release 16.2.0 found"},
		{"", "16.2.0", "New release 16.2.0 found"},
		{"16.1.0", "", "Checked: no release found"},
	} {
		if got := checkNotice(c.before, c.after); got != c.want {
			t.Errorf("checkNotice(%q, %q) = %q, want %q", c.before, c.after, got, c.want)
		}
	}
}

// Why: a file that could not be downloaded must say so on its row, with Retry, and the page
// must stay on the release the admin was looking at.
func TestFirmwareFetchFailureStaysOnItsRow(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), failFile: "tasmota.bin.gz"}
	e := fwEnv(t, fw)
	r, body := e.do(t, "POST", "/firmware/fetch", "admin", url.Values{"release": {"16.0.0"}, "filename": {"tasmota.bin.gz"}})
	card := fwSection(t, body, "release")
	if strings.Contains(body, `role="alert"`) {
		t.Error("the failure is the row's; no banner repeats it")
	}
	if r.StatusCode != 502 || !strings.Contains(card, "Tasmota 16.0.0") || !strings.Contains(visibleText(card), "Download failed") || !strings.Contains(visibleText(card), "firmware: SHA-256 differs between sources") || !strings.Contains(card, `aria-label="Retry tasmota.bin.gz"`) {
		t.Errorf("failed fetch = %d:\n%s", r.StatusCode, card)
	}
	fw.failFile = ""
	r, body = e.do(t, "POST", "/firmware/fetch", "admin", url.Values{"release": {"16.0.0"}, "filename": {"tasmota.bin.gz"}})
	if r.StatusCode != 200 || !strings.Contains(body, "tasmota.bin.gz 16.0.0 cached") || !strings.Contains(fwSection(t, body, "release"), `<span class="bdg t-ok">Cached</span>`) {
		t.Errorf("retried fetch = %d:\n%s", r.StatusCode, body)
	}
}

// Why: only admins upload custom firmware; the button and the drop zone open the upload form,
// which stays reachable as a page section without JavaScript.
func TestFirmwareUploadEntryPoints(t *testing.T) {
	e, _, _ := customEnv(t)
	_, body := e.do(t, "GET", "/firmware", "admin", nil)
	for _, want := range []string{"Upload custom firmware", `<dialog id="dlg-upload"`, `class="fw-drop"`, "choose a file", `hx-post="/tf/firmware/custom"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if _, page := e.do(t, "GET", "/firmware?upload=1", "admin", nil); !strings.Contains(page, `id="upload"`) || strings.Contains(page, `<dialog id="dlg-upload"`) {
		t.Error("?upload=1 must show the form in the page instead of the dialog")
	}
	_, op := e.do(t, "GET", "/firmware", "operator", nil)
	if strings.Contains(op, "Upload custom firmware") || strings.Contains(op, "fw-drop") || strings.Contains(op, `name="image"`) {
		t.Error("operators get the upload")
	}
}

// Why: the cached files read like the releases, newest release first; uploads come after the
// upstream files, which a newer upstream fetch must not scatter.
func TestSortCached(t *testing.T) {
	now := time.Now()
	arts := []model.Artifact{
		{Release: "14.2.0", Filename: "tasmota32.bin", FetchedAt: now},
		{Release: "16.0.0.5", Filename: "tasmota.bin.gz", Custom: "k1", FetchedAt: now.Add(time.Hour)},
		{Release: "14.10.0", Filename: "tasmota32.bin", FetchedAt: now.Add(-time.Hour)},
		{Release: "14.10.0", Filename: "tasmota.bin.gz", FetchedAt: now.Add(-2 * time.Hour)},
		{Release: "14.2.0", Filename: "tasmota.bin.gz", FetchedAt: now},
	}
	sortCached(arts)
	var got []string
	for _, a := range arts {
		got = append(got, a.Release+"/"+a.Filename)
	}
	want := "14.10.0/tasmota.bin.gz 14.10.0/tasmota32.bin 14.2.0/tasmota.bin.gz 14.2.0/tasmota32.bin 16.0.0.5/tasmota.bin.gz"
	if strings.Join(got, " ") != want {
		t.Errorf("order %v, want %s", got, want)
	}
}

func TestFirmwareHelpers(t *testing.T) {
	if got := shortHash("9f2c" + strings.Repeat("0", 56) + "41ab"); got != "9f2c…41ab" {
		t.Errorf("shortHash = %q", got)
	}
	for f, want := range map[model.ChipFamily]string{model.ChipESP8266: "ESP8266", model.ChipESP32: "ESP32", model.ChipESP32C3: "ESP32-C3", model.ChipESP32S3: "ESP32-S3"} {
		if got := chipLabel(f); got != want {
			t.Errorf("chipLabel(%s) = %q, want %q", f, got, want)
		}
	}
	if got := safeErr(`GET "https://u:p@h.example/x": 503; ftp://a@b/c`); strings.Contains(got, "u:p") || strings.Contains(got, "a@") || !strings.Contains(got, "https://h.example/x") {
		t.Errorf("safeErr = %q", got)
	}
	blocks := firmware.ParseNotesWith("## Changelog\n\nIntro text.\n\n- one\n- two\n- three\n- four\n- five\n- six\n- seven", nil)
	ex := excerpt(blocks)
	if len(ex) != excerptItems || ex[0][0].Text != "one" {
		t.Errorf("excerpt = %+v, want the first %d points of the list", ex, excerptItems)
	}
	if ex := excerpt(firmware.ParseNotesWith("Just a paragraph.", nil)); len(ex) != 1 || ex[0][0].Text != "Just a paragraph." {
		t.Errorf("excerpt without a list = %+v", ex)
	}
}

// ctxFailFW is a cache whose forced refresh fails before it records anything (a cancelled request): the index status
// keeps its old, error-free state, yet the check did fail.
type ctxFailFW struct{ *checkFW }

func (c *ctxFailFW) Refresh(context.Context) error { return context.Canceled }

// pageBanners is the inline banners of the firmware page: they are what the person reads about the check.
func pageBanners(t *testing.T, body string) []string {
	t.Helper()
	i := strings.Index(body, `class="page-firmware"`)
	if i < 0 {
		t.Fatalf("no firmware page in:\n%s", body)
	}
	return regexp.MustCompile(`(?s)<div class="banner tone-[a-z]+".*?</div>(?:\s*<button[^>]*>[^<]*</button>)?`).FindAllString(body[i:], -1)
}

// Why (#1090): one failed check was drawn three times (a header status with its own Try again, a danger banner
// "GitHub not reachable." with a second one, and for a manual check a third banner "Check failed: ..."), and the banner
// printed the Go chain ("source ...: 127.0.0.1:46693 answered 503") that only the log should hold. A person needs one
// calm sentence, what still works and one button; nothing here is theirs to fix, so it is a warning, not an alert.
func TestFirmwareFailedCheckIsOneWarnBanner(t *testing.T) {
	const chain = "firmware: source override: 127.0.0.1:46693 answered 503 Service Unavailable"
	now := time.Now()
	failed := func(loaded bool) firmware.IndexStatus {
		st := firmware.IndexStatus{LastAttempt: now.Add(-2 * time.Minute), LastError: chain}
		if loaded {
			st.Loaded, st.LastSuccess = true, now.Add(-3*time.Hour)
		}
		return st
	}
	for _, c := range []struct {
		name   string
		fw     func() (*checkFW, firmware.Cache)
		method string
		path   string
		status int
		text   string
	}{
		{"page, list loaded", func() (*checkFW, firmware.Cache) {
			f := &checkFW{fakeMgrFW: &fakeMgrFW{releases: baseRelease()}, st: failed(true)}
			return f, f
		}, "GET", "/firmware", 200, "Cached files and the last list still work."},
		{"page, no list yet", func() (*checkFW, firmware.Cache) {
			f := &checkFW{fakeMgrFW: &fakeMgrFW{}, st: failed(false)}
			return f, f
		}, "GET", "/firmware", 200, "the release list appears after the first successful check"},
		{"manual check fails", func() (*checkFW, firmware.Cache) {
			f := &checkFW{fakeMgrFW: &fakeMgrFW{releases: baseRelease()}, st: firmware.IndexStatus{Loaded: true, LastSuccess: now, LastAttempt: now}, err: errors.New(chain)}
			return f, f
		}, "POST", "/firmware/refresh", 502, "Cached files and the last list still work."},
		{"manual check cancelled before the cache records it", func() (*checkFW, firmware.Cache) {
			f := &checkFW{fakeMgrFW: &fakeMgrFW{releases: baseRelease()}, st: firmware.IndexStatus{Loaded: true, LastSuccess: now, LastAttempt: now}}
			return f, &ctxFailFW{f}
		}, "POST", "/firmware/refresh", 502, "Cached files and the last list still work."},
	} {
		t.Run(c.name, func(t *testing.T) {
			inner, fw := c.fw()
			e := setupWith(t, func(d *Deps) {
				d.Firmware, d.Artifacts = fw, inner
				d.Inventory = newInv(mixedFleet()...)
				d.Jobs = &fakeJobs{}
				d.FirmwareSource, d.FirmwareFeed = "GitHub releases (arendst/Tasmota)", "GitHub"
			})
			res, body := e.do(t, c.method, c.path, "admin", nil)
			if res.StatusCode != c.status {
				t.Fatalf("status %d, want %d", res.StatusCode, c.status)
			}
			banners := pageBanners(t, body)
			if len(banners) != 1 {
				t.Fatalf("%d banners, want exactly 1:\n%s", len(banners), strings.Join(banners, "\n---\n"))
			}
			b := banners[0]
			if !strings.Contains(b, "tone-warn") || strings.Contains(b, `role="alert"`) || !strings.Contains(b, "Could not check for new releases.") || !strings.Contains(b, c.text) {
				t.Errorf("the banner is not the warn banner with its lead and %q:\n%s", c.text, b)
			}
			page := body[strings.Index(body, `class="page-firmware"`):]
			if n := strings.Count(page, ">Try again<"); n != 1 {
				t.Errorf("%d Try again buttons, want 1", n)
			}
			if strings.Contains(page, "tone-danger") || strings.Contains(page, "t-danger") {
				t.Error("a failed check is drawn as danger")
			}
			for _, leak := range []string{"answered 503", "127.0.0.1", "source override", "Service Unavailable", "context canceled", "Check failed"} {
				if strings.Contains(body, leak) {
					t.Errorf("the page prints %q from the error chain", leak)
				}
			}
			// the header status says it too, and has no button of its own
			head := regexp.MustCompile(`(?s)class="pghead-acts">(.*?)</div></div>`).FindStringSubmatch(page)
			if head == nil || !strings.Contains(head[1], "Last check failed") || strings.Contains(head[1], "<form") || strings.Contains(head[1], "Try again") {
				t.Errorf("the header must say 'Last check failed' and carry no form:\n%v", head)
			}
			// operators read the state; only admins can start a check
			if c.method == "GET" { // a failed POST shows its answer to the admin only: the flag ends with that response
				_, op := e.do(t, "GET", "/firmware", "operator", nil)
				if ob := pageBanners(t, op); len(ob) != 1 || strings.Contains(op, ">Try again<") {
					t.Errorf("operator: %d banners, Try again present: %v", len(ob), strings.Contains(op, ">Try again<"))
				}
			}
		})
	}
}

// Why (#1090): the operator and the log are different readers. The chain of a failed manual check (a status code, the
// source URL) goes to the server log once, so an admin can find the cause, and the credentials of a source URL are cut
// before it is written (the page and the log are both places a secret must not reach).
func TestFirmwareManualCheckFailureIsLoggedOnce(t *testing.T) {
	var logs strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	fw := &checkFW{fakeMgrFW: &fakeMgrFW{releases: baseRelease()}, st: firmware.IndexStatus{Loaded: true, LastSuccess: time.Now(), LastAttempt: time.Now()},
		err: errors.New("source github: GET https://bot:hunter2@api.example/releases: answered 503")}
	e := checkEnv(t, fw, "GitHub")
	e.do(t, "POST", "/firmware/refresh", "admin", nil)
	out := logs.String()
	if n := strings.Count(out, "answered 503"); n != 1 {
		t.Errorf("the chain is in the log %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "bot:") {
		t.Errorf("credentials of the source URL reached the log:\n%s", out)
	}
}

// Why (#1090): "Update 0 devices…" was an <a> with aria-disabled that kept its href, so a click, a middle click and
// Enter still opened the update form for a release no device can take, and a title alone said why. At 0 it is a
// blocked button: no link, no action, and the reason is visible text that aria-describedby names. With devices it
// stays a link.
func TestUpdateZeroDevicesIsNoLink(t *testing.T) {
	updatesLink := regexp.MustCompile(`<a [^>]*href="/tf/updates/new[^"]*"[^>]*>`)
	blocked := func(t *testing.T, scope, label string) {
		t.Helper()
		if links := updatesLink.FindAllString(scope, -1); len(links) != 0 {
			t.Errorf("a link to the update form at 0 devices: %v", links)
		}
		// Why: the #1072 focus rule (button.btn[aria-disabled="true"]:focus-visible) draws a focused blocked button in
		// the text-3 grey at full strength; on the accent fill of a primary that is a 1.04:1 label, so Tab onto the
		// button would make its text vanish. A button that cannot act is no primary action anyway: it is the plain look.
		if strings.Contains(scope, `class="btn btn-primary" aria-disabled="true"`) {
			t.Error("the blocked button is btn-primary: keyboard focus draws a grey label on the accent")
		}
		m := regexp.MustCompile(`<button type="button" class="btn" aria-disabled="true" aria-describedby="([^"]+)">` + regexp.QuoteMeta(label) + `</button>`).FindStringSubmatch(scope)
		if m == nil {
			t.Fatalf("no blocked %q button:\n%s", label, scope)
		}
		i := strings.Index(scope, `id="`+m[1]+`"`)
		if i < 0 || !strings.Contains(visibleText(scope[i:min(len(scope), i+200)]), "No device can be updated to this release.") {
			t.Errorf("aria-describedby=%q names no visible reason", m[1])
		}
	}
	t.Run("firmware page", func(t *testing.T) {
		e := fwEnvWithUpdates(t, &fakeMgrFW{releases: baseRelease()}, mixedFleet())
		_, none := e.do(t, "GET", "/firmware?release=16.0.0", "operator", nil)
		blocked(t, fwSection(t, none, "release"), "Update 0 devices…")
		_, some := e.do(t, "GET", "/firmware", "operator", nil)
		if !updatesLink.MatchString(fwSection(t, some, "release")) {
			t.Error("with a device that can take the release, Update is a link")
		}
	})
	t.Run("release page", func(t *testing.T) {
		fw := newFeed(feedReleases())
		e := setupWith(t, func(d *Deps) {
			d.Firmware, d.Artifacts = fw, fw
			d.Inventory = newInv(relFleet()...)
			d.Jobs, d.OTA = &fakeJobs{}, &fakeOTA{items: map[model.DeviceID]ota.PreviewItem{}}
			d.FirmwareSource = "GitHub releases"
		})
		_, none := e.do(t, "GET", "/firmware/releases/15.0.0", "admin", nil) // nothing is older than the oldest device
		blocked(t, none[strings.Index(none, `<main id="main">`):], "Update 0 devices…")
		_, some := e.do(t, "GET", "/firmware/releases/16.2.0", "admin", nil)
		if !updatesLink.MatchString(some) {
			t.Error("with devices behind the release, Update is a link")
		}
	})
}

// Why (#1090): the header link and the drop zone's "choose a file" opened the same upload, two ways to one thing. The
// drop zone with its link is the one entry (the design system's file drop); operators have none.
func TestFirmwareUploadHasOneEntry(t *testing.T) {
	e, _, _ := customEnv(t)
	for _, path := range []string{"/firmware", "/firmware?release=16.0.0"} {
		_, body := e.do(t, "GET", path, "admin", nil)
		page := body[strings.Index(body, `class="page-firmware"`):]
		entries := regexp.MustCompile(`<a [^>]*(?:data-dialog="dlg-upload"|href="[^"]*upload=1[^"]*")[^>]*>[^<]*</a>`).FindAllString(page, -1)
		if len(entries) != 1 || !strings.Contains(entries[0], ">choose a file<") {
			t.Errorf("%s: %d ways into the upload, want the one \"choose a file\": %v", path, len(entries), entries)
		}
	}
}

// relErrFW is a cache whose release list cannot be read although its index status says it is loaded.
type relErrFW struct{ *checkFW }

func (relErrFW) Releases(context.Context) ([]firmware.Release, error) {
	return nil, errors.New("firmware: source override: 127.0.0.1:46693 answered 500")
}

// Why (#1090): the same rule for the list read on every page view: the Go chain (a source address, a status) is for the
// log, the page gets one plain sentence that says where the details are.
func TestFirmwareListReadErrorKeepsTheChainOffThePage(t *testing.T) {
	fw := relErrFW{&checkFW{fakeMgrFW: &fakeMgrFW{}, st: firmware.IndexStatus{Loaded: true, LastSuccess: time.Now(), LastAttempt: time.Now()}}}
	e := setupWith(t, func(d *Deps) {
		d.Firmware, d.Artifacts = fw, fw
		d.Inventory = newInv(mixedFleet()...)
		d.Jobs = &fakeJobs{}
	})
	_, body := e.do(t, "GET", "/firmware", "admin", nil)
	if !strings.Contains(body, "The release list could not be read. The server log has the details.") || strings.Contains(body, "127.0.0.1") || strings.Contains(body, "answered 500") {
		t.Errorf("the page does not map the list error to a sentence:\n%s", body)
	}
}

// Why (#1093): "Delete N unused" is destructive, so it is a default button at the end of the cached list, after the
// rows, not red text in the card head among the file's own controls (.fw-del), where it read as a harmless action of
// the header. The per-file Delete moved into each row's menu.
func TestFirmwareBulkDeleteStandsInTheCardFooter(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{
		{ID: 1, Release: "16.1.0", Filename: "tasmota-minimal.bin.gz", Family: model.ChipESP8266, Variant: "minimal", Size: 2048, SHA256: strings.Repeat("a", 64), Source: "github", FetchedAt: time.Now()},
		{ID: 2, Release: "16.1.0", Filename: "tasmota32.bin", Family: model.ChipESP32, Variant: "tasmota", Size: 1024, SHA256: strings.Repeat("b", 64), Source: "ota.tasmota.com", FetchedAt: time.Now()},
	}}
	e := fwEnv(t, fw)
	_, body := e.do(t, "GET", "/firmware", "admin", nil)
	list := fwSection(t, body, "cache")
	if strings.Contains(list, "fw-del") {
		t.Errorf("the per-file red Delete link is back in the cache card:\n%s", list)
	}
	head := list[:strings.Index(list, `class="fw-crow`)]
	if strings.Contains(head, "fw-unused") || strings.Contains(head, "Delete") {
		t.Errorf("the bulk delete is in the card head:\n%s", head)
	}
	foot := strings.Index(list, `class="fw-cache-foot"`)
	if foot < 0 || foot < strings.LastIndex(list, `class="fw-crow`) {
		t.Fatalf("the bulk delete is not in a footer after the last row:\n%s", list)
	}
	if f := list[foot:]; !strings.Contains(f, "fw-unused") || !strings.Contains(f, "Delete 2 unused") {
		t.Errorf("the footer lacks the bulk delete:\n%s", f)
	}
}

// Why (#1094): the state of a file (Cached, Prefetch, Download failed with Retry, ...) is the one cell of a row that
// changes width. It has its own cell, fw-file-st, which the stylesheet gives a fixed column, so the other cells of the
// row stay where they are; the Firmware page and the release page draw it from the same template, so a file cannot look
// different on the two. The release page's Prefetch and Retry keep answering with the release page (from=release).
func TestFileStateHasItsOwnCellOnBothPages(t *testing.T) {
	admin := view{IsAdmin: true, CSRF: "tok", Base: "/tf"}
	for _, tc := range []struct {
		name   string
		v      view
		f      fileView
		failed map[string]string
		want   string
	}{
		{"cached", admin, fileView{Name: "tasmota.bin", Cached: true, Size: 1}, nil, "Cached"},
		{"not offered", admin, fileView{Name: "tasmota.bin", Missing: true}, nil, "Not offered upstream"},
		{"prefetch", admin, fileView{Name: "tasmota.bin"}, nil, "Prefetch tasmota.bin"},
		{"failed", admin, fileView{Name: "tasmota.bin"}, map[string]string{"16.1.0/tasmota.bin": "boom"}, "Download failed"},
		{"retry", admin, fileView{Name: "tasmota.bin"}, map[string]string{"16.1.0/tasmota.bin": "boom"}, "Retry tasmota.bin"},
		{"operator", view{Base: "/tf"}, fileView{Name: "tasmota.bin"}, nil, "Not cached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fw, rel strings.Builder
			p := firmwarePage{Failed: tc.failed}
			if err := fwFileRow(tc.v, p, releaseView{Version: "16.1.0"}, tc.f).Render(context.Background(), &fw); err != nil {
				t.Fatal(err)
			}
			rp := releasePage{firmwarePage: p, Version: "16.1.0", Rel: &releaseView{Version: "16.1.0", Files: []fileView{tc.f}}}
			if err := relFiles(tc.v, rp).Render(context.Background(), &rel); err != nil {
				t.Fatal(err)
			}
			for page, html := range map[string]string{"Firmware page": fw.String(), "release page": rel.String()} {
				cell := strings.Index(html, `<span class="fw-file-st">`)
				if cell < 0 || strings.Index(html, tc.want) < cell {
					t.Errorf("%s: %q is not inside the state cell:\n%s", page, tc.want, html)
				}
			}
			if strings.Contains(fw.String(), `name="from"`) {
				t.Errorf("the Firmware page's form must answer with the Firmware page:\n%s", fw.String())
			}
			if strings.Contains(rel.String(), `action="/tf/firmware/fetch"`) && !strings.Contains(rel.String(), `name="from" value="release"`) {
				t.Errorf("the release page's form must answer with the release page:\n%s", rel.String())
			}
		})
	}
}

// The reason a download failed is a line of the whole row under the state (#1094), on the release page as on the
// Firmware page: inside the name block it was squeezed beside the state column and sat above the state on a phone.
func TestReleasePageFailureReasonFollowsTheStateCell(t *testing.T) {
	v := view{IsAdmin: true, CSRF: "tok", Base: "/tf"}
	rp := releasePage{
		firmwarePage: firmwarePage{Failed: map[string]string{"16.1.0/tasmota.bin": "boom"}},
		Version:      "16.1.0",
		Rel:          &releaseView{Version: "16.1.0", Files: []fileView{{Name: "tasmota.bin"}}},
	}
	var b strings.Builder
	if err := relFiles(v, rp).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	st, why := strings.Index(html, `<span class="fw-file-st">`), strings.Index(html, `<span class="fw-file-why">boom</span>`)
	if st < 0 || why < st {
		t.Errorf("the reason must follow the state cell:\n%s", html)
	}
}
