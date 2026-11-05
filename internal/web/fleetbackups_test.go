package web

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// fakeFleet is the BackupFleet the Backups page reads.
type fakeFleet struct {
	run   *model.BackupRun
	rows  []model.BackupDeviceRow
	prune []model.Backup
}

func (f *fakeFleet) LastRun(context.Context) (model.BackupRun, bool, error) {
	if f.run == nil {
		return model.BackupRun{}, false, nil
	}
	return *f.run, true, nil
}
func (f *fakeFleet) Overview(context.Context) ([]model.BackupDeviceRow, error) { return f.rows, nil }
func (f *fakeFleet) PruneNext(context.Context) ([]model.Backup, error)         { return f.prune, nil }

// fleetFixture is the fleet of the mockup in small: three backed up (one of them old), one that never
// was, one failed and two skipped in the last scheduled run.
func fleetFixture(now time.Time) *fakeFleet {
	at := now.Add(-6 * time.Hour)
	old := now.Add(-20 * 24 * time.Hour)
	return &fakeFleet{
		run: &model.BackupRun{ID: 1, Trigger: model.BackupRunScheduled, StartedAt: at, FinishedAt: at.Add(time.Minute), Completed: true,
			Tried: 3, OK: 2, Failed: 1, Skipped: 2,
			Items: []model.BackupRunItem{
				{DeviceID: 1, DeviceName: "attic", Outcome: model.BackupOutcomeOK, At: at},
				{DeviceID: 2, DeviceName: "balcony", Outcome: model.BackupOutcomeSkipped, Reason: "offline since 2 Nov", At: at},
				{DeviceID: 3, DeviceName: "cellar", Outcome: model.BackupOutcomeFailed, Reason: "no answer within 30 s", At: at},
				{DeviceID: 4, DeviceName: "garage", Outcome: model.BackupOutcomeSkipped, Reason: "never answered", At: at},
				{DeviceID: 5, DeviceName: "kitchen", Outcome: model.BackupOutcomeOK, At: at},
			}},
		rows: []model.BackupDeviceRow{
			{DeviceID: 1, Name: "attic", NewestAt: at, Runs: 10, Kept: 1, Bytes: 4096, Files: 12, LastOutcome: model.BackupOutcomeOK, LastAt: at},
			{DeviceID: 2, Name: "balcony", NewestAt: old, Runs: 8, Bytes: 2048, LastOutcome: model.BackupOutcomeSkipped, LastReason: "offline since 2 Nov", LastAt: at, SkipReason: "offline since 2 Nov"},
			{DeviceID: 3, Name: "cellar", NewestAt: now.Add(-3 * 24 * time.Hour), Runs: 9, Bytes: 2048, FilesNote: "settings only: no file system", LastOutcome: model.BackupOutcomeFailed, LastReason: "no answer within 30 s", LastAt: at},
			{DeviceID: 4, Name: "garage", LastOutcome: model.BackupOutcomeSkipped, LastReason: "never answered", LastAt: at, SkipReason: "never answered"},
			{DeviceID: 5, Name: "kitchen", NewestAt: at, Runs: 3, Bytes: 1024, FilesNote: "files need HTTP", LastOutcome: model.BackupOutcomeOK, LastAt: at},
			{DeviceID: 6, Name: "porch", Runs: 0}, // added after the run, never backed up
		},
		prune: []model.Backup{{ID: 1, DeviceID: 1, DeviceName: "attic", Size: 100, TakenAt: old}},
	}
}

func fleetEnv(t *testing.T, fleet *fakeFleet, extraEnv ...string) *env {
	t.Helper()
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleet }, append([]string{"TASMOFLEET_BACKUP_INTERVAL=24h"}, extraEnv...)...)
	return e
}

// section returns the HTML of the card with the given id (up to the next card).
func cardOf(t *testing.T, body, id string) string {
	t.Helper()
	i := strings.Index(body, `id="card-`+id+`"`)
	if i < 0 {
		t.Fatalf("no card %q in the page", id)
	}
	rest := body[i:]
	if j := strings.Index(rest[1:], "<section"); j >= 0 {
		rest = rest[:j+1]
	}
	return rest
}

var rowCount = regexp.MustCompile(`<tr[ >]`)

// Why (#409): the page is the one place that says how the fleet is backed up. An admin sees the last
// run with its failures and skipped devices apart, the table with one row per device, the schedule
// as a form and the actions that start a run.
func TestFleetBackupsPageAdmin(t *testing.T) {
	now := time.Now()
	e := fleetEnv(t, fleetFixture(now))
	r, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	last := cardOf(t, body, "lastrun")
	for _, want := range []string{"Last run", "Backed up", "Failed", "Skipped", "cellar", "No answer within 30 s", "balcony", "Offline since 2 Nov", "Never answered", `href="/tf/devices/3/backups"`} {
		if !strings.Contains(last, want) {
			t.Errorf("Last run lacks %q", want)
		}
	}
	devs := cardOf(t, body, "devices")
	for _, want := range []string{"Back up selected", "Back up all now", `aria-label="Select attic"`, "Settings only", "Needs HTTP", "Not in a run yet", "12 files"} {
		if !strings.Contains(devs, want) {
			t.Errorf("Devices card lacks %q", want)
		}
	}
	// the row menu carries what an admin may do
	for _, want := range []string{"Open backups", "Open device"} { // Replace needs the restore service, absent here
		if !strings.Contains(devs, want) {
			t.Errorf("admin row menu lacks %q", want)
		}
	}
	if n := len(rowCount.FindAllString(devs, -1)); n != 7 { // header + 6 devices
		t.Errorf("table has %d rows, want 7 (header and 6 devices)", n)
	}
	for _, want := range []string{`name="v.BACKUP_KEEP"`, `name="v.BACKUP_INTERVAL"`, `name="v.BACKUP_MAX_AGE"`, "Save settings", "data-sys-bar"} {
		if !strings.Contains(body, want) {
			t.Errorf("schedule lacks %q", want)
		}
	}
	// the Storage card totals what the table lists
	st := cardOf(t, body, "storage")
	for _, want := range []string{"Total size", humanBytes(9216), "Devices without a backup", "Pruned at the next run"} {
		if !strings.Contains(st, want) {
			t.Errorf("Storage lacks %q: %s", want, st)
		}
	}
}

// Why: a filter chip that says "(2)" and then lists 3 rows is worse than no chip. The counts of
// every chip are the rows it shows (plan 409B, data rules), and the dashboard tile's target
// (?filter=none) shows exactly the devices without a backup.
func TestFleetBackupsFilterCountsAreTheRows(t *testing.T) {
	now := time.Now()
	e := fleetEnv(t, fleetFixture(now))
	_, all := e.do(t, "GET", "/devices/backups", "admin", nil)
	chip := regexp.MustCompile(`<a class="dv-chip"[^>]*href="[^"]*filter=([a-z]*)"[^>]*>([^<]*)<span class="dv-n">(\d+)</span>`)
	counts := map[string]string{}
	for _, m := range chip.FindAllStringSubmatch(all, -1) {
		counts[m[1]] = m[3]
	}
	// "All" has no filter parameter; "old" is older than max(7 days, 2 x interval): balcony only;
	// "none" has garage and porch.
	want := map[string]int{"none": 2, "old": 1, "failed": 1, "skipped": 2}
	for key, n := range want {
		if counts[key] != itoa(n) {
			t.Errorf("chip %s says %q, want %d", key, counts[key], n)
		}
		_, body := e.do(t, "GET", "/devices/backups?filter="+key, "admin", nil)
		if got := len(rowCount.FindAllString(cardOf(t, body, "devices"), -1)) - 1; got != n {
			t.Errorf("filter=%s lists %d rows, its chip says %d", key, got, n)
		}
		if !strings.Contains(body, `aria-current="true"`) {
			t.Errorf("filter=%s: no chip is current", key)
		}
	}
	_, none := e.do(t, "GET", "/devices/backups?filter=none", "admin", nil)
	devs := cardOf(t, none, "devices")
	if !strings.Contains(devs, "garage") || !strings.Contains(devs, "porch") || strings.Contains(devs, "attic") {
		t.Errorf("filter=none must list the devices without a backup only")
	}
}

// Why: skipped devices are not tried, so "Back up all now" must not pretend to cover them; it posts
// exactly the devices a run would try, to the bulk backup scope page (which lists what it leaves out).
func TestFleetBackupsBackUpAllPostsOnlyDevicesThatWouldBeTried(t *testing.T) {
	now := time.Now()
	e := fleetEnv(t, fleetFixture(now))
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	form := regexp.MustCompile(`(?s)<form id="bk-all".*?</form>`).FindString(body)
	if form == "" {
		t.Fatal("no form for Back up all now")
	}
	if !strings.Contains(form, `/devices/bulk/backup`) {
		t.Errorf("Back up all posts to the bulk backup page: %s", form)
	}
	for _, id := range []string{"1", "3", "5", "6"} {
		if !strings.Contains(form, `name="id" value="`+id+`"`) {
			t.Errorf("device %s would be tried and must be posted", id)
		}
	}
	for _, id := range []string{"2", "4"} {
		if strings.Contains(form, `name="id" value="`+id+`"`) {
			t.Errorf("device %s is skipped and must not be posted", id)
		}
	}
}

// Why (decision 7): without a schedule nothing is "due"; the card says so and still shows what the
// last stored run was, and the schedule switch is off.
func TestFleetBackupsNoSchedule(t *testing.T) {
	now := time.Now()
	e := fleetEnv(t, fleetFixture(now), "TASMOFLEET_BACKUP_INTERVAL=0")
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	last := cardOf(t, body, "lastrun")
	for _, want := range []string{"No schedule", "Last scheduled run", "2 backed up", "1 failed", "2 skipped"} {
		if !strings.Contains(last, want) {
			t.Errorf("no-schedule Last run lacks %q: %s", want, last)
		}
	}
	if strings.Contains(last, "Next run") {
		t.Error("no schedule, no next run")
	}
}

// Why: an operator may look and may start single backups, but never changes the schedule and never
// starts a fleet run (the bulk routes are admin-only). The page must not offer what would answer 403.
func TestFleetBackupsOperatorIsReadOnly(t *testing.T) {
	now := time.Now()
	e := fleetEnv(t, fleetFixture(now))
	r, body := e.do(t, "GET", "/devices/backups", "operator", nil)
	if r.StatusCode != 200 {
		t.Fatalf("operator status %d", r.StatusCode)
	}
	for _, bad := range []string{"Back up all now", "Back up selected", `type="checkbox"`, `name="v.BACKUP_KEEP"`, "Save settings", "Replace"} {
		if strings.Contains(body, bad) {
			t.Errorf("operator page must not contain %q", bad)
		}
	}
	for _, want := range []string{"Admins change the schedule", "Open backups"} {
		if !strings.Contains(body, want) {
			t.Errorf("operator page lacks %q", want)
		}
	}
	for _, c := range []struct{ method, path string }{{"POST", "/devices/backups"}} {
		if r, _ := e.do(t, c.method, c.path, "operator", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"3"}}); r.StatusCode != 403 {
			t.Errorf("operator %s %s: %d, want 403", c.method, c.path, r.StatusCode)
		}
	}
	if r, _ := e.do(t, "GET", "/devices/backups", "", nil); r.StatusCode != 401 {
		t.Errorf("signed out: %d", r.StatusCode)
	}
}

// Why: the schedule form moved from Settings; saving it returns to the page it was saved on, with
// the stored value shown (not to the Settings page).
func TestFleetBackupsSaveReturnsToThePage(t *testing.T) {
	now := time.Now()
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(now) })
	r, _ := e.do(t, "POST", "/devices/backups", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"4"}})
	if r.StatusCode != 303 || !strings.HasSuffix(r.Header.Get("Location"), "/devices/backups?ok=saved") {
		t.Fatalf("save: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	for _, st := range svc.State() {
		if st.Key == "BACKUP_KEEP" && st.Value != "4" {
			t.Errorf("BACKUP_KEEP = %q, want 4", st.Value)
		}
	}
	// a refused value keeps the person on this page
	r, body := e.do(t, "POST", "/devices/backups", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"nope"}})
	if r.StatusCode == 303 || !strings.Contains(body, "Nothing was saved") || !strings.Contains(body, `id="card-devices"`) {
		t.Errorf("a refused save answers on the Backups page: %d", r.StatusCode)
	}
}

// Why (#409): the Backups tab moved to Devices -> Backups. Settings no longer lists it, but its layout
// entry stays: sysAdvanced is an index into sysLayout.
func TestSettingsHasNoBackupsTab(t *testing.T) {
	now := time.Now()
	e := fleetEnv(t, fleetFixture(now))
	_, body := e.do(t, "GET", "/settings/system", "admin", nil)
	for _, bad := range []string{`data-sys-tab="backups"`, `tab=backups`, `BACKUP_KEEP`} {
		if strings.Contains(body, bad) {
			t.Errorf("Settings still shows %q", bad)
		}
	}
	if sysLayout[sysAdvanced].Slug != "advanced" {
		t.Errorf("sysAdvanced no longer points at the Advanced tab: %q", sysLayout[sysAdvanced].Slug)
	}
	if sysTabSlug("backups") != "backups" {
		t.Error("the backups layout entry must stay")
	}
}

// Why: Backups is a page of the Devices group, between Policy and Add.
func TestDevicesSubNavHasBackups(t *testing.T) {
	now := time.Now()
	e := fleetEnv(t, fleetFixture(now))
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	order := []string{`href="/tf/devices"`, `href="/tf/devices/backups"`, `href="/tf/devices/new"`}
	prev := -1
	for _, s := range order {
		i := strings.Index(body, s)
		if i < 0 || i < prev {
			t.Fatalf("sub-navigation order broken at %q (index %d, previous %d)", s, i, prev)
		}
		prev = i
	}
	_, page := e.do(t, "GET", "/devices/backups", "admin", nil)
	if !regexp.MustCompile(`href="/tf/devices/backups"[^>]*aria-current="page"`).MatchString(page) &&
		!regexp.MustCompile(`aria-current="page"[^>]*href="/tf/devices/backups"`).MatchString(page) {
		t.Error("Backups is not the current sub-navigation entry on its page")
	}
}

// Why: a run that only skipped devices is not a failure: the bar stays calm and says "skipped"; the
// whole item opens the page that lists them. The phone line shows the short form.
func TestStatusBarBackupLinksToPageAndSaysSkipped(t *testing.T) {
	done := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)
	run := func(failed, skipped int) string {
		e, _, _, _ := sysEnvFull(t, true, func(d *Deps) {
			d.BackupFleet = &fakeFleet{}
			d.ScheduledBackup = func() BackupRunInfo {
				return BackupRunInfo{Finished: done, LastTotal: 24, LastFailed: failed, LastSkipped: skipped}
			}
		})
		_, body := e.do(t, "GET", "/devices", "operator", nil)
		return barOf(body)
	}
	calm := run(0, 3)
	for _, want := range []string{`href="/tf/devices/backups"`, "24 of 24 backed up", "3 skipped", "All good"} {
		if !strings.Contains(calm, want) {
			t.Errorf("skipped only: bar lacks %q: %s", want, calm)
		}
	}
	for _, bad := range []string{"t-warn", "t-bad", "sb-warn", "failed"} {
		if strings.Contains(calm, bad) {
			t.Errorf("skipped only must stay calm, found %q", bad)
		}
	}
	mixed := run(2, 3)
	for _, want := range []string{"22 of 24 backed up · 2 failed · 3 skipped", "t-warn", "Backup partly failed", "· 2 failed · 3 skipped"} {
		if !strings.Contains(mixed, want) {
			t.Errorf("failures and skipped: bar lacks %q: %s", want, mixed)
		}
	}
}
