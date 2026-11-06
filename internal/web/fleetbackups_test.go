package web

import (
	"context"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/snapshot"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
)

// fakeFleet is the BackupFleet the Backups page reads.
type fakeFleet struct {
	run    *model.BackupRun
	rows   []model.BackupDeviceRow
	prune  []model.Backup
	missed time.Time // when a scheduled run was last skipped; zero = never
}

func (f *fakeFleet) LastRun(context.Context) (model.BackupRun, bool, error) {
	if f.run == nil {
		return model.BackupRun{}, false, nil
	}
	return *f.run, true, nil
}
func (f *fakeFleet) LastMissed(context.Context) (time.Time, error)             { return f.missed, nil }
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
				{DeviceID: 2, DeviceName: "balcony", Outcome: model.BackupOutcomeSkipped, Reason: "offline", Since: now.Add(-3 * 24 * time.Hour), At: at},
				{DeviceID: 3, DeviceName: "cellar", Outcome: model.BackupOutcomeFailed, Reason: "no answer within 30 s", At: at},
				{DeviceID: 4, DeviceName: "garage", Outcome: model.BackupOutcomeSkipped, Reason: "never answered", At: at},
				{DeviceID: 5, DeviceName: "kitchen", Outcome: model.BackupOutcomeOK, At: at},
			}},
		rows: []model.BackupDeviceRow{
			{DeviceID: 1, Name: "attic", NewestAt: at, Runs: 10, Kept: 1, Bytes: 4096, Files: 12, LastOutcome: model.BackupOutcomeOK, LastAt: at},
			{DeviceID: 2, Name: "balcony", NewestAt: old, Runs: 8, Bytes: 2048, LastOutcome: model.BackupOutcomeSkipped, LastReason: "offline", LastSince: now.Add(-3 * 24 * time.Hour), LastAt: at, SkipReason: "offline", SkipSince: now.Add(-3 * 24 * time.Hour)},
			{DeviceID: 3, Name: "cellar", NewestAt: now.Add(-3 * 24 * time.Hour), Runs: 9, Bytes: 2048, FilesNote: "settings only: no file system", LastOutcome: model.BackupOutcomeFailed, LastReason: "no answer within 30 s", LastAt: at},
			{DeviceID: 4, Name: "garage", LastOutcome: model.BackupOutcomeSkipped, LastReason: "never answered", LastAt: at, SkipReason: "never answered"},
			{DeviceID: 5, Name: "kitchen", NewestAt: at, Runs: 3, Bytes: 1024, FilesNote: "files need HTTP", LastOutcome: model.BackupOutcomeOK, LastAt: at},
			{DeviceID: 6, Name: "porch", Runs: 0}, // added after the run, never backed up
		},
		prune: []model.Backup{{ID: 1, DeviceID: 1, DeviceName: "attic", Size: 100, TakenAt: old}},
	}
}

// fleetEnv is the Backups page with a schedule on every day at 03:00.
func fleetEnv(t *testing.T, fleet *fakeFleet, extraEnv ...string) *env {
	t.Helper()
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleet }, append([]string{"TASMOFLEET_BACKUP_DAYS=mon,tue,wed,thu,fri,sat,sun"}, extraEnv...)...)
	return e
}

// fleetEnvManual is the Backups page with no weekday chosen: manual only.
func fleetEnvManual(t *testing.T, fleet *fakeFleet) *env {
	t.Helper()
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleet })
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
	for _, want := range []string{"Last run", "Backed up", "Failed", "Skipped", "cellar", "No answer within 30 s", "balcony", "Offline since <time", "Never answered", `href="/tf/devices/3/backups"`} {
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
	for _, want := range []string{`name="v.BACKUP_KEEP"`, `name="v.BACKUP_DAYS" value="mon"`, `name="v.BACKUP_TIME"`, `name="v.BACKUP_MAX_AGE"`, "Save settings", "data-sys-bar"} {
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
	e := fleetEnvManual(t, fleetFixture(now))
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

// Why (#409): "Back up all now" leaves skipped devices out, and the person must see which ones and why
// before starting: a device that is not backed up must never be a surprise. The scope page lists
// them (count, names, reasons) and keeps listing them when a row is removed.
func TestBackUpAllScopePageListsTheDevicesLeftOut(t *testing.T) {
	fleet := &fakeFleet{rows: []model.BackupDeviceRow{
		{DeviceID: 1, Name: "kitchen"},
		{DeviceID: 2, Name: "attic", SkipReason: "offline", SkipSince: time.Now().Add(-48 * time.Hour)},
	}}
	fbk := &fakeBackupBatch{}
	e := setupWith(t, func(d *Deps) { d.BackupBatch = fbk; d.BackupFleet = fleet })
	_, body := e.do(t, "POST", "/devices/bulk/backup", "admin", bulkForm("id", "1", "left", "2"))
	for _, want := range []string{"Left out: 1", "attic", "Offline since", `data-ts="day"`, `name="left" value="2"`} {
		if !strings.Contains(body, want) {
			t.Errorf("scope page lacks %q", want)
		}
	}
	// "x" on a row posts the page again: the left-out list stays
	_, again := e.do(t, "POST", "/devices/bulk/backup", "admin", bulkForm("id", "1", "left", "2", "drop", "1"))
	if !strings.Contains(again, "Left out: 1") {
		t.Error("the left-out list is gone after a row was removed")
	}
	// an id that is not a skipped device of the fleet is not listed as one
	_, other := e.do(t, "POST", "/devices/bulk/backup", "admin", bulkForm("id", "1", "left", "1"))
	if strings.Contains(other, "Left out") {
		t.Error("a device that would be tried is not left out")
	}
	if len(fbk.ids) != 0 {
		t.Error("the scope page submits nothing")
	}
}

// Why: the Back up all now form carries the skipped ids as "left" so the scope page can name them.
func TestBackUpAllFormCarriesTheLeftOutIDs(t *testing.T) {
	e := fleetEnv(t, fleetFixture(time.Now()))
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	form := regexp.MustCompile(`(?s)<form id="bk-all".*?</form>`).FindString(body)
	for _, id := range []string{"2", "4"} {
		if !strings.Contains(form, `name="left" value="`+id+`"`) {
			t.Errorf("skipped device %s is not posted as left out", id)
		}
	}
}

// Why (review of #590, High): a setting that needs attention on another Settings tab (here an unreadable
// stored MQTT password) used to move the active tab, so /devices/backups answered with the admin Settings
// page, for an operator too, with the webhook address (its token is part of the URL). The Backups and
// Snapshots pages are chosen by the route, never by where a problem is.
func TestOwnPagesAreChosenByTheRouteNotByAProblemOnAnotherTab(t *testing.T) {
	const token = "fake-webhook-token-123"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, secretbox.KeyFile), []byte(fakeKey), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := snapshot.New(snapSource{}, dir)
	mem := &sysMem{rows: map[string]store.SystemSetting{
		"MQTT_URL":           {Key: "MQTT_URL", Value: "tcp://broker.lan:1883"},
		"MQTT_PASSWORD":      {Key: "MQTT_PASSWORD", Secret: true, Unreadable: true}, // sealed with a key we no longer have
		"NOTIFY_WEBHOOK_URL": {Key: "NOTIFY_WEBHOOK_URL", Value: "https://hook.example.test/in?token=" + token},
	}}
	cfg, err := config.Load([]string{"TASMOFLEET_DATA_DIR=/nonexistent-tasmofleet-test", "TASMOFLEET_CONFIG_FILE=/dev/null"})
	if err != nil {
		t.Fatal(err)
	}
	cfg.DeviceNetworks = []netip.Prefix{netip.MustParsePrefix("172.31.13.0/24")}
	aud := &fakeAudit{}
	svc, err := sysconfig.New(t.Context(), sysconfig.Options{Base: cfg, Store: mem, Audit: aud,
		TestMQTT: func(context.Context, sysconfig.MQTTParams) error { return nil },
		TestSSO:  func(context.Context, string) (string, error) { return "ok", nil }})
	if err != nil {
		t.Fatal(err)
	}
	e := setupWith(t, func(d *Deps) {
		d.SysConfig, d.Guard, d.Recorder = svc, stepGuard{fakeGuard: fakeGuard{}, stepped: &atomic.Bool{}}, aud
		d.BackupFleet, d.Snapshots = fleetFixture(time.Now()), mgr
	})
	// the fixture really has a problem on another tab (else the test proves nothing)
	if _, body := e.do(t, "GET", "/settings/system", "admin", nil); !strings.Contains(body, `data-sys-tab="mqtt">`) {
		t.Fatal("fixture: the Settings page does not open the MQTT tab for its problem")
	}
	for _, role := range []string{"operator", "admin"} {
		_, body := e.do(t, "GET", "/devices/backups", role, nil)
		if !strings.Contains(body, `id="card-devices"`) || strings.Contains(body, `data-sys-tab="mqtt"`) {
			t.Errorf("%s: /devices/backups is not the Backups page", role)
		}
		if role == "operator" && strings.Contains(body, token) {
			t.Error("the webhook token reached an operator")
		}
	}
	_, snaps := e.do(t, "GET", "/settings/system/snapshots", "admin", nil)
	if !strings.Contains(snaps, `id="card-stored"`) || strings.Contains(snaps, `data-sys-tab="mqtt"`) {
		t.Error("the Snapshots page is not the Snapshots page")
	}
}

// Why (review of #590): an operator may look at the backups of deleted devices and keep or release them,
// but never download or delete one (both routes are admin-only): the buttons must not be offered.
func TestOperatorSeesNoDownloadOrDeleteOnDeletedDevicesBackups(t *testing.T) {
	e, _, _, _ := storageEnv(t)
	_, op := e.do(t, "GET", "/devices/backups", "operator", nil)
	if !strings.Contains(op, "garage &lt;b&gt;plug&lt;/b&gt;") {
		t.Fatal("fixture: the deleted device's backup is not listed")
	}
	for _, bad := range []string{`href="/tf/backups/70"`, `>Download<`, `data-dlg-action`, `dlg-bkdel`, "bk-del"} {
		if strings.Contains(op, bad) {
			t.Errorf("operator page offers %q", bad)
		}
	}
	if strings.Contains(op, "Download one to restore") || !strings.Contains(op, "admins download or delete them") {
		t.Error("the operators' text promises a Download they do not have")
	}
	if !strings.Contains(op, `formaction="/tf/backups/70/keep"`) {
		t.Error("Keep stays for operators")
	}
	_, adm := e.do(t, "GET", "/devices/backups", "admin", nil)
	for _, want := range []string{`href="/tf/backups/70"`, `data-dlg-action="/tf/backups/70/delete"`} {
		if !strings.Contains(adm, want) {
			t.Errorf("admin page lacks %q", want)
		}
	}
}

// Why: Replace is an admin action; an operator's row menu must show only what they may run. The admin's
// shows it when the restore service can replace a device.
func TestRowMenuShowsReplaceToAdminsOnly(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()); d.Restore = &replRestore{} })
	_, adm := e.do(t, "GET", "/devices/backups", "admin", nil)
	_, op := e.do(t, "GET", "/devices/backups", "operator", nil)
	if !strings.Contains(adm, `href="/tf/devices/1/replace"`) {
		t.Error("admin row menu lacks Replace")
	}
	if strings.Contains(op, "/replace") || strings.Contains(op, "Replace") {
		t.Error("operator row menu offers Replace")
	}
}

// Why (mockup 1c): with the schedule off the page still lists the last stored run's failed and skipped
// devices under the "No schedule" note, as the mockup shows; they are not hidden with the sums.
func TestNoScheduleStillListsTheLastRunsFailedAndSkippedDevices(t *testing.T) {
	e := fleetEnvManual(t, fleetFixture(time.Now()))
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	last := cardOf(t, body, "lastrun")
	for _, want := range []string{"No schedule", "Failed (1)", "cellar", "No answer within 30 s", "Skipped (2)", "balcony", "Never answered"} {
		if !strings.Contains(last, want) {
			t.Errorf("no-schedule Last run lacks %q", want)
		}
	}
}

// Why (#633, as #511): a host-style name has no break point at its dots, so once the Device column
// wraps instead of clipping, a browser cuts it inside a label ("readinglamp.exampl / e.internal").
// Each dot and dash gets a <wbr>, so the name breaks between its labels; the name itself is unchanged.
func TestFleetBackupsDeviceNamesBreakAtTheirLabels(t *testing.T) {
	fleet := fleetFixture(time.Now())
	fleet.rows[0].Name = "livingroom-readinglamp.example.internal"
	e := fleetEnv(t, fleet)
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	if want := ">livingroom-<wbr>readinglamp.<wbr>example.<wbr>internal</a>"; !strings.Contains(cardOf(t, body, "devices"), want) {
		t.Errorf("the device name has no break points after its dots and dash, want %q", want)
	}
}

// Why: a cut-off reason or age must be readable in full; the cells carry a title.
func TestTableCellsCarryATitleForCutOffText(t *testing.T) {
	e := fleetEnv(t, fleetFixture(time.Now()))
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	devs := cardOf(t, body, "devices")
	if !strings.Contains(devs, `class="bk-rsn" title="No answer within 30 s"`) {
		t.Error("a failure reason is not in a title")
	}
	// the times carry a title on their own <time>, which time.js writes in the reader's zone (a title
	// in the server's zone would disagree with the text next to it)
	if !regexp.MustCompile(`<td data-label="Newest backup"[^>]*><time [^>]*data-ts="day"[^>]* title="`).MatchString(devs) {
		t.Error("the newest backup's time has no title for time.js")
	}
	if !regexp.MustCompile(`Offline since <time [^>]*data-ts="day"[^>]* title="`).MatchString(devs) {
		t.Error("the offline time has no title for time.js")
	}
	if strings.Contains(devs, `<td data-label="Newest backup" class="nowrap s" title=`) {
		t.Error("a title on the cell would show the server's zone")
	}
}

// Why (#601): the person must be able to see, on the page and not only in a notification they may
// have switched off, that a scheduled run was skipped because TasmoFleet was down. The note shows
// only while it is the newest event: a later real run, or no schedule at all, removes it.
func TestFleetBackupsSaysWhenAScheduledRunWasSkipped(t *testing.T) {
	now := time.Now()
	fleet := fleetFixture(now)
	fleet.missed = now.Add(-time.Hour) // after the fixture's last run, 6 h ago
	_, body := fleetEnv(t, fleet).do(t, "GET", "/devices/backups", "admin", nil)
	last := cardOf(t, body, "lastrun")
	for _, want := range []string{"A scheduled run was skipped", "60 minutes late", "The next run is as scheduled"} {
		if !strings.Contains(last, want) {
			t.Errorf("Last run lacks %q: %s", want, last)
		}
	}
	older := fleetFixture(now)
	older.missed = now.Add(-30 * time.Hour) // before the last run: that run is the news
	if _, body := fleetEnv(t, older).do(t, "GET", "/devices/backups", "admin", nil); strings.Contains(cardOf(t, body, "lastrun"), "was skipped") {
		t.Error("a skip older than the last run must not be shown")
	}
	if _, body := fleetEnvManual(t, fleet).do(t, "GET", "/devices/backups", "admin", nil); strings.Contains(cardOf(t, body, "lastrun"), "was skipped") {
		t.Error("with no schedule there is no skipped run to show")
	}
}

// Why (#601): the Last run card names the schedule it follows and the Schedule card the next run, so
// "when does it run?" is answered without opening a form; an operator, who cannot change it, reads
// the days and the time as text.
func TestFleetBackupsNamesTheScheduleAndTheNextRun(t *testing.T) {
	e := fleetEnv(t, fleetFixture(time.Now()), "TASMOFLEET_BACKUP_DAYS=mon,wed,fri", "TASMOFLEET_BACKUP_TIME=04:30")
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	if last := cardOf(t, body, "lastrun"); !strings.Contains(last, "Mon, Wed, Fri at 04:30") {
		t.Errorf("Last run does not name the schedule: %s", last)
	}
	if sched := cardOf(t, body, "schedule"); !strings.Contains(sched, "Next run") {
		t.Errorf("Schedule card lacks the next run: %s", sched)
	}
	_, op := e.do(t, "GET", "/devices/backups", "operator", nil)
	sched := cardOf(t, op, "schedule")
	for _, want := range []string{"Mon, Wed, Fri", "04:30", "Read-only for operators"} {
		if !strings.Contains(sched, want) {
			t.Errorf("operator Schedule lacks %q: %s", want, sched)
		}
	}
	if strings.Contains(sched, `name="v.BACKUP`) {
		t.Error("an operator must get no schedule fields")
	}
}

// Why (#601): "older than" was twice the interval. With weekdays it is twice the longest wait between
// two runs: a weekly schedule must not call a six-day-old backup old, and a daily one keeps the floor of a week.
func TestFleetBackupsOldThresholdFollowsTheLongestWait(t *testing.T) {
	for _, c := range []struct{ days, want string }{
		{"mon", "Older than 14 days"},
		{"mon,tue,wed,thu,fri,sat,sun", "Older than 7 days"},
	} {
		_, body := fleetEnv(t, fleetFixture(time.Now()), "TASMOFLEET_BACKUP_DAYS="+c.days).do(t, "GET", "/devices/backups", "admin", nil)
		if !strings.Contains(body, c.want) {
			t.Errorf("%s: want %q on the page", c.days, c.want)
		}
	}
}

// Why (#601, review of #668): "Wed, MON" from the environment is a valid schedule and the page must show
// it as one: both chips ticked (else the next save of the page posts no day and turns the schedule off)
// and the days named for an operator.
func TestFleetBackupsShowsDaysGivenInAnyCase(t *testing.T) {
	e := fleetEnv(t, fleetFixture(time.Now()), "TASMOFLEET_BACKUP_DAYS=Wed, MON")
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	sched := cardOf(t, body, "schedule")
	for _, d := range []string{"mon", "wed"} {
		if !strings.Contains(sched, `value="`+d+`" checked`) {
			t.Errorf("the %s chip is not ticked: %s", d, sched)
		}
	}
	_, op := e.do(t, "GET", "/devices/backups", "operator", nil)
	if sched := cardOf(t, op, "schedule"); !strings.Contains(sched, "Mon, Wed") {
		t.Errorf("an operator does not read the days: %s", sched)
	}
}
