package web

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
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
		"2 files · 3.0\u00a0KiB", "Delete 1 unused…", `name="id" value="2"`,
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
// what still works. Error texts can carry a source URL; credentials in it never reach the page.
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
			[]string{"Last check failed", "GitHub not reachable.", "answered 503", "Showing releases from the last successful check (3h ago); cached firmware files still work for updates.", ">Try again</button>", "https://api.example/releases"},
			[]string{"hunter2", "bot:"}},
		{"failed before any list", firmware.IndexStatus{LastAttempt: now, LastError: "source github: dial tcp: refused"},
			[]string{"GitHub not reachable.", "refused", "No release list yet"}, []string{"Index loading"}},
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
	if r.StatusCode != 200 || !strings.Contains(body, `data-flash><span class="banner-dot"></span><span class="banner-text">No release newer than 16.1.0</span>`) {
		t.Errorf("check without news = %d:\n%s", r.StatusCode, body)
	}
	fw.refreshed = newer
	if _, body = e.do(t, "POST", "/firmware/refresh", "admin", nil); !strings.Contains(body, "New release 16.2.0 found") {
		t.Errorf("the new release is not announced:\n%s", body)
	}
	fw.err = errors.New("source github: answered 503")
	r, body = e.do(t, "POST", "/firmware/refresh", "admin", nil)
	if r.StatusCode != 502 || !strings.Contains(body, "GitHub not reachable.") || !strings.Contains(body, "Tasmota 16.2.0") {
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
	if r.StatusCode != 502 || !strings.Contains(card, "Tasmota 16.0.0") || !strings.Contains(card, `title="firmware: SHA-256 differs between sources">Download failed</span>`) || !strings.Contains(card, `aria-label="Retry tasmota.bin.gz"`) {
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
	blocks := firmware.ParseNotes("## Changelog\n\nIntro text.\n\n- one\n- two\n- three\n- four\n- five\n- six\n- seven")
	ex := excerpt(blocks)
	if len(ex) != excerptItems || ex[0][0].Text != "one" {
		t.Errorf("excerpt = %+v, want the first %d points of the list", ex, excerptItems)
	}
	if ex := excerpt(firmware.ParseNotes("Just a paragraph.")); len(ex) != 1 || ex[0][0].Text != "Just a paragraph." {
		t.Errorf("excerpt without a list = %+v", ex)
	}
}
