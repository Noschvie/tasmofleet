package web

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// fakeMgrFW is a firmware cache with the management surface: a consumer-side
// fake of firmware.Cache + firmware.Manager + the artifact lister.
type fakeMgrFW struct {
	mu       sync.Mutex
	releases []firmware.Release
	arts     []model.Artifact
	inUse    map[model.ArtifactID][]model.JobID
	ensured  []string
	deleted  []model.ArtifactID
	failFile string
	failErr  error // the failure of failFile; nil = a cross-check error
	nextID   model.ArtifactID
	slow     chan struct{}
}

func (f *fakeMgrFW) Releases(context.Context) ([]firmware.Release, error) { return f.releases, nil }
func (f *fakeMgrFW) URLFor(model.Artifact) string                         { return "" }
func (f *fakeMgrFW) Prune(context.Context, int) (int, error)              { return 0, nil }
func (f *fakeMgrFW) Ensure(_ context.Context, rel, file string) (model.Artifact, error) {
	if f.slow != nil {
		<-f.slow
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, rel+"/"+file)
	if file == f.failFile {
		if f.failErr != nil {
			return model.Artifact{}, f.failErr
		}
		return model.Artifact{}, fmt.Errorf("firmware: SHA-256 differs between sources")
	}
	f.nextID++
	a := model.Artifact{ID: 100 + f.nextID, Release: rel, Filename: file, Size: 1000, Source: "github", FetchedAt: time.Now()}
	f.arts = append(f.arts, a)
	return a, nil
}
func (f *fakeMgrFW) ListArtifacts(context.Context) ([]model.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.arts), nil
}
func (f *fakeMgrFW) ArtifactsInUse(context.Context) (map[model.ArtifactID][]model.JobID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inUse, nil
}
func (f *fakeMgrFW) Delete(_ context.Context, id model.ArtifactID) (model.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.arts {
		if a.ID != id {
			continue
		}
		if jobs, busy := f.inUse[id]; busy {
			return a, &firmware.InUseError{Artifact: a, Jobs: jobs}
		}
		f.arts = slices.Delete(f.arts, i, i+1)
		f.deleted = append(f.deleted, id)
		return a, nil
	}
	return model.Artifact{}, store.ErrNotFound
}
func (f *fakeMgrFW) DeleteUnused(context.Context) (firmware.DeleteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var res firmware.DeleteResult
	var keep []model.Artifact
	for _, a := range f.arts {
		if _, busy := f.inUse[a.ID]; busy {
			keep = append(keep, a)
			res.Kept++
			continue
		}
		res.Deleted = append(res.Deleted, a)
		res.Freed += a.Size
		f.deleted = append(f.deleted, a.ID)
	}
	f.arts = keep
	return res, nil
}

func mixedFleet() []model.Device {
	return []model.Device{
		{ID: 1, Name: "lamp-1m", Address: "10.0.0.1", Identity: model.Identity{LanguageKnown: true, Family: model.ChipESP8266, Version: "16.0.0", Variant: "tasmota", ProgramFlashSizeKB: 1024}},
		{ID: 2, Name: "dimmer", Address: "10.0.0.2", Identity: model.Identity{LanguageKnown: true, Family: model.ChipESP32, Version: "16.1.0", Variant: "tasmota"}},
		{ID: 3, Name: "c3-node", Address: "10.0.0.3", Identity: model.Identity{LanguageKnown: true, Family: model.ChipESP32C3, Version: "16.2.0", Variant: "tasmota", Image: "tasmota32c3"}},
	}
}

func fwEnv(t *testing.T, fw *fakeMgrFW) *env {
	t.Helper()
	return setupWith(t, func(d *Deps) {
		d.Firmware, d.Artifacts = fw, fw
		d.Inventory = newInv(mixedFleet()...)
		d.Jobs = &fakeJobs{jobs: []model.Job{{ID: 4, BatchID: 2}, {ID: 7, BatchID: 5}, {ID: 8, BatchID: 5}, {ID: 12, BatchID: 9}}}
	})
}

func baseRelease() []firmware.Release {
	return []firmware.Release{{Version: "16.1.0", URL: "https://github.com/arendst/Tasmota/releases/tag/v16.1.0", Files: []string{
		"tasmota.bin.gz", "tasmota-minimal.bin.gz", "tasmota32.bin", "tasmota32c3.bin",
	}}, {Version: "16.0.0", Files: []string{"tasmota.bin.gz"}}}
}

// Why: release notes are written by third parties. Whatever they contain, the
// page must show it as text: no element, attribute, script or javascript: URL
// may come out of a hostile body (the strict CSP is the second line, this is
// the first).
func TestFirmwarePageEscapesHostileReleaseNotes(t *testing.T) {
	payloads := "<script>alert(1)</script>\n\n" +
		"[a](javascript:alert(2)) [b](JaVaScRiPt:alert(3)) ![c](data:text/html;base64,PHNjcmlwdD4=) <a href=\"javascript:alert(4)\">d</a>\n\n" +
		"# <img src=x onerror=alert(5)>\n\n- <svg onload=alert(6)>\n- [e](https://example.org/\"onmouseover=\"alert(7))\n\n" +
		"```\n</code></pre><script>alert(8)</script>\n```\n\n" +
		"> <iframe src=//evil.example>\n\n**<b onclick=alert(9)>x</b>** `</code><script>alert(10)</script>`\n\n" +
		"[ok](https://github.com/arendst/Tasmota/pull/1)"
	rels := baseRelease()
	rels[0].Notes = payloads
	e := fwEnv(t, &fakeMgrFW{releases: rels})
	_, body := e.do(t, "GET", "/firmware", "operator", nil)
	body = body[strings.Index(body, `<main id="main">`):] // the shell's own logo is an <svg>

	for _, bad := range []string{
		"<script>alert", "<img src=x", "<svg", "<iframe", "<b onclick", "onerror=alert(5)>", "onload=alert(6)>",
		`href="javascript`, `href="JaVaScRiPt`, `href="data:`, `href="https://example.org/"onmouseover`,
	} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	for _, want := range []string{"&lt;img src=x onerror=alert(5)&gt;", "&lt;/code&gt;&lt;/pre&gt;&lt;script&gt;alert(8)"} {
		if !strings.Contains(body, want) {
			t.Errorf("escaped text %q missing", want)
		}
	}
	if !strings.Contains(body, `<a href="https://github.com/arendst/Tasmota/pull/1" rel="noopener noreferrer nofollow" target="_blank">ok</a>`) {
		t.Error("the safe link was not rendered with rel=noopener")
	}
	// Every anchor on the page leaving the app must carry the safe rel.
	for _, m := range regexp.MustCompile(`<a href="(https?://[^"]*)"([^>]*)>`).FindAllStringSubmatch(body, -1) {
		if !strings.Contains(m[2], "noopener") {
			t.Errorf("external link without rel=noopener: %s", m[0])
		}
	}
}

// Why: deleting changes what the OTA engine can serve, so it is admin only,
// needs an explicit confirmation, and leaves an audit trail.
func TestFirmwareDeleteNeedsAdminConfirmationAndIsAudited(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{{ID: 5, Release: "16.1.0", Filename: "tasmota32.bin", Size: 1024, SHA256: "def456"}}}
	e := fwEnv(t, fw)
	form := url.Values{"confirm": {"yes"}}

	if r, _ := e.do(t, "POST", "/firmware/artifacts/5/delete", "operator", form); r.StatusCode != 403 {
		t.Errorf("operator delete = %d", r.StatusCode)
	}
	if r, _ := e.do(t, "GET", "/firmware/artifacts/5/delete", "operator", nil); r.StatusCode != 403 {
		t.Errorf("operator confirm page = %d", r.StatusCode)
	}
	if r, body := e.do(t, "GET", "/firmware/artifacts/5/delete", "admin", nil); r.StatusCode != 200 || !strings.Contains(body, "Delete file") || !strings.Contains(body, "dimmer") {
		t.Errorf("confirm page = %d\n%s", r.StatusCode, body)
	}
	if r, _ := e.do(t, "POST", "/firmware/artifacts/5/delete", "admin", url.Values{}); r.StatusCode != 422 || len(fw.deleted) != 0 {
		t.Errorf("delete without confirmation = %d, deleted %v", r.StatusCode, fw.deleted)
	}
	if r, _ := e.do(t, "POST", "/firmware/artifacts/999/delete", "admin", form); r.StatusCode != 404 {
		t.Errorf("unknown artifact = %d", r.StatusCode)
	}
	r, body := e.do(t, "POST", "/firmware/artifacts/5/delete", "admin", form)
	if r.StatusCode != 200 || !strings.Contains(body, "Deleted 16.1.0/tasmota32.bin") || len(fw.deleted) != 1 {
		t.Fatalf("delete = %d deleted %v\n%s", r.StatusCode, fw.deleted, body)
	}
	ev := e.aud.events[len(e.aud.events)-1]
	if ev.Action != "firmware.delete" || ev.Target != "artifact:5" || ev.Result != "ok" || ev.Actor != "test:admin" ||
		!strings.Contains(ev.Detail, `"file":"tasmota32.bin"`) || !strings.Contains(ev.Detail, `"sha256":"def456"`) {
		t.Errorf("audit = %+v", ev)
	}
}

// Why: a file an active or queued job needs must survive the click, say which
// job holds it, and the refused attempt is itself worth auditing.
func TestFirmwareDeleteRefusedWhileInUse(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{{ID: 5, Release: "16.1.0", Filename: "tasmota-minimal.bin.gz", Size: 10}},
		inUse: map[model.ArtifactID][]model.JobID{5: {12}}}
	e := fwEnv(t, fw)
	// The confirm page already says so and offers no delete button.
	if _, body := e.do(t, "GET", "/firmware/artifacts/5/delete", "admin", nil); !strings.Contains(body, "needed by transaction #9") || strings.Contains(body, "Delete file") {
		t.Errorf("confirm page for a file in use:\n%s", body)
	}
	// A stale form (job started after the page was rendered) is refused too.
	fw.mu.Lock()
	fw.inUse = nil
	fw.mu.Unlock()
	_, _ = e.do(t, "GET", "/firmware/artifacts/5/delete", "admin", nil)
	fw.mu.Lock()
	fw.inUse = map[model.ArtifactID][]model.JobID{5: {12}}
	fw.mu.Unlock()
	r, body := e.do(t, "POST", "/firmware/artifacts/5/delete", "admin", url.Values{"confirm": {"yes"}})
	if r.StatusCode != 409 || !strings.Contains(body, "needed by transaction #9") || len(fw.deleted) != 0 || len(fw.arts) != 1 {
		t.Fatalf("status %d deleted %v\n%s", r.StatusCode, fw.deleted, body)
	}
	if ev := e.aud.events[len(e.aud.events)-1]; ev.Action != "firmware.delete" || !strings.HasPrefix(ev.Result, "refused: ") {
		t.Errorf("audit = %+v", ev)
	}
}

// Why: deleting several files at once needs its own confirmation that lists what
// goes and what is left out; a file a pending or running transaction needs can
// never be selected, and the server refuses it even when a stale form posts it.
func TestFirmwareDeleteSelected(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{
		{ID: 1, Release: "16.1.0", Filename: "tasmota.bin.gz", Size: 100},
		{ID: 2, Release: "16.1.0", Filename: "tasmota32.bin", Size: 200},
		{ID: 3, Release: "16.0.0", Filename: "tasmota.bin.gz", Size: 300},
	}, inUse: map[model.ArtifactID][]model.JobID{1: {4}}}
	e := fwEnv(t, fw)
	if r, _ := e.do(t, "POST", "/firmware/delete-selected", "operator", url.Values{"confirm": {"yes"}, "id": {"2"}}); r.StatusCode != 403 {
		t.Errorf("operator = %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/firmware/delete-selected", "admin", url.Values{}); r.StatusCode != 422 || len(fw.deleted) != 0 {
		t.Fatalf("nothing selected: %d %v", r.StatusCode, fw.deleted)
	}
	// Step one: the confirmation lists the selectable files and names what is left out.
	r, page := e.do(t, "POST", "/firmware/delete-selected", "admin", url.Values{"id": {"1", "2", "3"}})
	if r.StatusCode != 200 || !strings.Contains(page, "These 2 cached files (500\u00a0B)") || !strings.Contains(page, "needed by transaction #2") {
		t.Errorf("confirm page:\n%s", page)
	}
	if !strings.Contains(page, `name="id" value="2"`) || !strings.Contains(page, `name="id" value="3"`) || strings.Contains(page, `name="id" value="1"`) || len(fw.deleted) != 0 {
		t.Errorf("confirm page must carry exactly the selectable ids and delete nothing yet:\n%s", page)
	}
	// A file cached after the confirmation page was rendered (id 9) is not in the posted list.
	fw.arts = append(fw.arts, model.Artifact{ID: 9, Release: "16.1.0", Filename: "tasmota32c3.bin", Size: 7})
	// Step two, with a stale form: id 1 is needed (kept, audited as refused); id 77 vanished.
	r, body := e.do(t, "POST", "/firmware/delete-selected", "admin", url.Values{"confirm": {"yes"}, "id": {"1", "2", "3", "77"}})
	if r.StatusCode != 200 || !strings.Contains(body, "Deleted 2 files (500 B). 1 files that are needed were kept. 1 were already gone.") {
		t.Fatalf("%d\n%s", r.StatusCode, body)
	}
	if len(fw.arts) != 2 || fw.arts[0].ID != 1 || fw.arts[1].ID != 9 {
		t.Errorf("remaining %+v", fw.arts)
	}
	var refused, gone bool
	for _, ev := range e.aud.events {
		refused = refused || (ev.Target == "artifact:1" && strings.HasPrefix(ev.Result, "refused: "))
		gone = gone || (ev.Target == "artifact:77" && strings.HasPrefix(ev.Result, "not found"))
	}
	if !refused || !gone {
		t.Errorf("audit lacks the refused (%v) or the not-found (%v) outcome", refused, gone)
	}
	n := 0
	for _, ev := range e.aud.events {
		if ev.Action == "firmware.delete" && ev.Result == "ok" && strings.Contains(ev.Detail, "delete-selected") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d audit events, want one per deleted file", n)
	}
}

// Why: an uploaded image cannot be downloaded again, so the bulk delete never
// takes it, and a forged POST naming it is refused and audited.
func TestFirmwareDeleteSelectedNeverTakesAnUpload(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{{ID: 1, Release: "16.0.0.5", Filename: "tasmota.bin.gz", Size: 10, Source: "custom", Custom: "k1"}}}
	e := fwEnv(t, fw)
	_, body := e.do(t, "GET", "/firmware", "admin", nil)
	if strings.Contains(body, `name="id" value="1"`) || strings.Contains(body, "unused…") {
		t.Errorf("an upload is never part of \"Delete unused\":\n%s", body)
	}
	r, _ := e.do(t, "POST", "/firmware/delete-selected", "admin", url.Values{"confirm": {"yes"}, "id": {"1"}})
	if r.StatusCode != 200 || len(fw.deleted) != 0 || len(fw.arts) != 1 {
		t.Fatalf("status %d, deleted %v", r.StatusCode, fw.deleted)
	}
	if ev := e.aud.events[len(e.aud.events)-1]; ev.Action != "firmware.delete" || !strings.HasPrefix(ev.Result, "refused: ") {
		t.Errorf("audit = %+v", ev)
	}
}

func waitFinished(t *testing.T, e *env, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, body := e.do(t, "GET", path, "admin", nil); strings.Contains(body, "finished") {
			return body
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("prefetch did not finish")
	return ""
}

// Why: "Prefetch for my fleet" must fetch exactly what the fleet needs: for the
// mixed fleet that is the ESP8266 build, tasmota-minimal, the ESP32 build and
// the C3 build; nothing the fleet does not use, and nothing taken from the form.
func TestFirmwarePrefetchFetchesExactlyTheFleetFiles(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease()}
	e := fwEnv(t, fw)
	if r, _ := e.do(t, "POST", "/firmware/prefetch", "operator", url.Values{"release": {"16.1.0"}}); r.StatusCode != 403 {
		t.Errorf("operator = %d", r.StatusCode)
	}
	// Extra form fields cannot widen the set; unknown releases are refused.
	if r, _ := e.do(t, "POST", "/firmware/prefetch", "admin", url.Values{"release": {"../../x"}, "file": {"tasmota32s3.bin"}}); r.StatusCode != 422 {
		t.Errorf("unknown release = %d", r.StatusCode)
	}
	r, _ := e.do(t, "POST", "/firmware/prefetch", "admin", url.Values{"release": {"16.1.0"}, "file": {"tasmota32s3.bin"}})
	loc := r.Header.Get("Location")
	if r.StatusCode != 303 || !strings.HasPrefix(loc, "/tf/firmware/prefetch/") {
		t.Fatalf("start = %d %q", r.StatusCode, loc)
	}
	body := waitFinished(t, e, strings.TrimPrefix(loc, "/tf"))
	fw.mu.Lock()
	got := slices.Clone(fw.ensured)
	fw.mu.Unlock()
	slices.Sort(got)
	want := []string{"16.1.0/tasmota-minimal.bin.gz", "16.1.0/tasmota.bin.gz", "16.1.0/tasmota32.bin", "16.1.0/tasmota32c3.bin"}
	if !slices.Equal(got, want) {
		t.Fatalf("ensured %v, want %v", got, want)
	}
	if !strings.Contains(body, "4 of 4 files cached") {
		t.Errorf("progress page:\n%s", body)
	}
	ev := e.aud.events[len(e.aud.events)-1]
	if ev.Action != "firmware.prefetch" || ev.Target != "release:16.1.0" || ev.Result != "ok" {
		t.Errorf("audit = %+v", ev)
	}
	// The firmware page now shows every file the fleet needs as cached, with nothing left to prefetch.
	_, page := e.do(t, "GET", "/firmware", "admin", nil)
	if n := strings.Count(page, ">Cached</span>"); n != 4 || strings.Contains(page, "Prefetch for my fleet") || strings.Contains(page, ">Prefetch</button>") {
		t.Errorf("after the prefetch %d files show Cached, want 4 and no Prefetch:\n%s", n, page)
	}
}

// Why: a refused download (cross-check failure) is reported on its file and
// does not hide the others.
func TestFirmwarePrefetchShowsFailuresPerFile(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), failFile: "tasmota32.bin"}
	e := fwEnv(t, fw)
	r, _ := e.do(t, "POST", "/firmware/prefetch", "admin", url.Values{"release": {"16.1.0"}})
	body := waitFinished(t, e, strings.TrimPrefix(r.Header.Get("Location"), "/tf"))
	if !strings.Contains(body, "failed: firmware: SHA-256 differs between sources") || !strings.Contains(body, "3 of 4 files cached, 1 failed") {
		t.Errorf("page:\n%s", body)
	}
}

// Why: an override source URL can carry credentials, and a download error may quote that URL.
// The progress page shows the error, so it must show it without them.
func TestFirmwarePrefetchFailureHidesSourceCredentials(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), failFile: "tasmota32.bin",
		failErr: fmt.Errorf(`GET "http://user:secret@127.0.0.1:5555/x/tasmota32.bin": 503`)}
	e := fwEnv(t, fw)
	r, _ := e.do(t, "POST", "/firmware/prefetch", "admin", url.Values{"release": {"16.1.0"}})
	body := waitFinished(t, e, strings.TrimPrefix(r.Header.Get("Location"), "/tf"))
	if strings.Contains(body, "secret") || !strings.Contains(body, "http://127.0.0.1:5555/x/tasmota32.bin") {
		t.Errorf("progress page:\n%s", body)
	}
}

// Why: the progress page is live; the stream delivers HTML fragments and ends
// when the run is over, so the browser stops reconnecting.
func TestFirmwarePrefetchStreamEndsWithTheRun(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), slow: make(chan struct{})}
	e := fwEnv(t, fw)
	r, _ := e.do(t, "POST", "/firmware/prefetch", "admin", url.Values{"release": {"16.1.0"}})
	loc := strings.TrimPrefix(r.Header.Get("Location"), "/tf")
	go func() { time.Sleep(300 * time.Millisecond); close(fw.slow) }()
	resp, body := e.do(t, "GET", loc+"/stream", "admin", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "event: prefetch") || !strings.Contains(body, "running") || !strings.Contains(body, "finished") {
		t.Fatalf("stream %d:\n%s", resp.StatusCode, body)
	}
	if r, _ := e.do(t, "GET", "/firmware/prefetch/nope/stream", "admin", nil); r.StatusCode != 404 {
		t.Errorf("unknown run = %d", r.StatusCode)
	}
	if r, _ := e.do(t, "GET", loc, "operator", nil); r.StatusCode != 403 {
		t.Errorf("operator progress page = %d", r.StatusCode)
	}
}

// Why: a second click while a download runs must not start a parallel run.
func TestFirmwarePrefetchTwiceAttachesToTheRunningOne(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), slow: make(chan struct{})}
	e := fwEnv(t, fw)
	a, _ := e.do(t, "POST", "/firmware/prefetch", "admin", url.Values{"release": {"16.1.0"}})
	b, _ := e.do(t, "POST", "/firmware/prefetch", "admin", url.Values{"release": {"16.0.0"}})
	if a.Header.Get("Location") != b.Header.Get("Location") {
		t.Errorf("second start went to %q, want the running %q", b.Header.Get("Location"), a.Header.Get("Location"))
	}
	close(fw.slow)
	waitFinished(t, e, strings.TrimPrefix(a.Header.Get("Location"), "/tf"))
}

// Why: "10 devices still need this file" next to a list of 13 names reads as a contradiction; the
// list says which of them still need a download and which are already up to date.
func TestUsedByTextExplainsTheDevicesThatNeedNoDownload(t *testing.T) {
	ds := []deviceRef{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	if got := usedByText(artifactView{Devices: ds, Waiting: 2}); !strings.Contains(got, "2 devices still to update, 1 already up to date or not updatable") {
		t.Errorf("got %q", got)
	}
	if got := usedByText(artifactView{Devices: ds, Waiting: 3}); strings.Contains(got, "already") {
		t.Errorf("all devices wait, nothing to explain: %q", got)
	}
}
