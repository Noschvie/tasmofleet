package web

import (
	"context"
	"html"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/snapshot"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// fakeFleet is the BackupFleet the Backups page reads.
type fakeFleet struct {
	run    *model.BackupRun
	rows   []model.BackupDeviceRow
	prune  []PruneDevice
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
func (f *fakeFleet) PrunePlan(context.Context) ([]PruneDevice, error)          { return f.prune, nil }

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
		prune: []PruneDevice{{Device: 1, Name: "attic", Files: []model.Backup{{ID: 1, DeviceID: 1, DeviceName: "attic", Kind: model.BackupConfigDump, Size: 100, TakenAt: old}}}},
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
	if j := strings.Index(rest[1:], `<section class="scard`); j >= 0 { // the next card, not a list inside this one
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
	for _, want := range []string{"Last run", "Backed up", "Failed", "Skipped"} {
		if !strings.Contains(last, want) {
			t.Errorf("Last run lacks %q", want)
		}
	}
	devs := cardOf(t, body, "devices")
	// the failed and skipped devices with their reasons are in the table (#684, #996: Last run only counts them)
	for _, want := range []string{"Back up selected", `aria-label="Select attic"`, "Settings only", "Needs HTTP", "Not in a run yet", "12 files", "cellar", "No answer within 30 s", `href="/tf/devices/3/backups"`, "balcony", "Offline since <time", "Never answered"} {
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
	// Why (#996): the device lists are gone from the card; the counts in the line above are what is left of them.
	for _, not := range []string{"cellar", "balcony", "garage", "Failed (1)", "show the 2 skipped"} {
		if strings.Contains(last, not) {
			t.Errorf("no-schedule Last run still lists devices (%q): %s", not, last)
		}
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
	for _, want := range []string{`href="/tf/devices/backups"`, "24 backed up", "3 skipped", "All good"} {
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
	for _, want := range []string{"22 backed up · 2 failed · 3 skipped", "t-warn", "Backup partly failed", "· 2 failed · 3 skipped"} {
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

// manyFailed is the fixture's fleet after a bad night: five devices failed in the last scheduled run
// (cellar from the fixture, then lobby, mudroom, nook and office).
func manyFailed(now time.Time) *fakeFleet {
	fleet := fleetFixture(now)
	at := fleet.run.StartedAt
	for i, name := range []string{"lobby", "mudroom", "nook", "office"} {
		id := model.DeviceID(10 + i)
		fleet.run.Items = append(fleet.run.Items, model.BackupRunItem{DeviceID: id, DeviceName: name, Outcome: model.BackupOutcomeFailed, Reason: "no answer within 30 s", At: at})
		fleet.rows = append(fleet.rows, model.BackupDeviceRow{DeviceID: id, Name: name, Runs: 1, LastOutcome: model.BackupOutcomeFailed, LastReason: "no answer within 30 s", LastAt: at})
	}
	fleet.run.Failed = 5
	return fleet
}

// skippedTile is the HTML of the Skipped tile of the Last run card: from its label to the closing tag
// of the tile.
var skippedTile = regexp.MustCompile(`(?s)<a class="sys-sum"[^>]*>\s*<div class="sys-sum-label">Skipped</div>.*?</a>`)

// Why (#996, after #684): the table below lists every failed and skipped device with its reason, and the
// Failed and Skipped tiles link it filtered. A Last run card that names devices as well shows the same
// devices twice, and on a night when many fail it grows by one row each. The card is its four tiles; the
// devices are one click away in the table, which is the one place that has all of them.
func TestLastRunCardIsItsFourTilesWithoutADeviceList(t *testing.T) {
	e := fleetEnv(t, manyFailed(time.Now())) // 5 failed, 2 skipped
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	last := cardOf(t, body, "lastrun")
	for _, want := range []string{"Started", "Backed up", "Failed", "Skipped",
		`href="/tf/devices/backups?filter=failed#card-devices"`, `href="/tf/devices/backups?filter=skipped#card-devices"`} {
		if !strings.Contains(last, want) {
			t.Errorf("Last run lacks %q: %s", want, last)
		}
	}
	for _, not := range []string{"cellar", "lobby", "mudroom", "nook", "office", "balcony", "garage", "Show all", "more device", "sys-orphan", "Failed (", "Skipped ("} {
		if strings.Contains(last, not) {
			t.Errorf("Last run still holds a device list (%q): %s", not, last)
		}
	}
	// what the card no longer names is in the table it links to, with its reason
	devs := cardOf(t, body, "devices")
	for _, want := range []string{"cellar", "lobby", "mudroom", "nook", "office", "balcony", "garage", "No answer within 30 s"} {
		if !strings.Contains(devs, want) {
			t.Errorf("the table does not hold %q", want)
		}
	}
}

// Why (#996): skipped devices are not failures and are not in the notification (#409 decision). That
// sentence used to sit under the device lists; with the lists gone it belongs to the Skipped tile, as a
// visible line (a tooltip does not show on touch). It is on the tile, not beside it, so it cannot be
// read as being about the failed devices, and only while something was skipped.
func TestLastRunSkippedTileSaysSkippedAreNotFailures(t *testing.T) {
	_, body := fleetEnv(t, fleetFixture(time.Now())).do(t, "GET", "/devices/backups", "admin", nil)
	tile := skippedTile.FindString(cardOf(t, body, "lastrun"))
	if tile == "" {
		t.Fatal("no Skipped tile in the Last run card")
	}
	if !strings.Contains(tile, "Not failures, not in the notification") {
		t.Errorf("the Skipped tile does not carry the note: %s", tile)
	}
	// a run that skipped nobody has nothing to explain
	clean := fleetFixture(time.Now())
	clean.run.Skipped = 0
	_, body = fleetEnv(t, clean).do(t, "GET", "/devices/backups", "admin", nil)
	if last := cardOf(t, body, "lastrun"); strings.Contains(last, "not in the notification") {
		t.Errorf("no note on a run with nothing skipped: %s", last)
	}
}

// Why (#684): the schedule form is five fields that are set once; open all the time it is the longest
// card of the side column and pushes everything below it away. It shows a line with what is set and
// the fields sit in a fold (the app's details.fold). A fold that hides a field with an error would make
// a refused save look ignored, so a row with a problem renders it open.
func TestScheduleFieldsSitInAFoldThatOpensOnAnError(t *testing.T) {
	e := fleetEnv(t, fleetFixture(time.Now()))
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	sched := cardOf(t, body, "schedule")
	fold := regexp.MustCompile(`<details class="fold"( open)?><summary>Change the schedule`).FindStringSubmatch(sched)
	if fold == nil {
		t.Fatalf("the schedule fields are not in a fold with a summary: %s", sched)
	}
	if fold[1] != "" {
		t.Error("the fold is open without a problem")
	}
	if strings.Index(sched, "Every day at 03:00") > strings.Index(sched, "<details") || !strings.Contains(sched, "Next run") {
		t.Errorf("the summary line (the days and time, the next run) does not come before the fold: %s", sched)
	}
	if !strings.Contains(sched[strings.Index(sched, "<details"):], `name="v.BACKUP_KEEP"`) {
		t.Error("the fields are not inside the fold")
	}
	// a refused value opens the fold, with the error beside the field
	_, body = e.do(t, "POST", "/devices/backups", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"nope"}})
	sched = cardOf(t, body, "schedule")
	if !regexp.MustCompile(`<details class="fold" open><summary>Change the schedule`).MatchString(sched) {
		t.Errorf("a refused value leaves the field in a closed fold: %s", sched)
	}
	if !strings.Contains(sched, `data-key="BACKUP_KEEP"`) || !strings.Contains(sched, "ufield-err") && !strings.Contains(sched, "aria-invalid") {
		t.Errorf("the error is not shown by its field: %s", sched)
	}
	// a save that fails on a setting this page does not show is no reason to open the fold
	_, body = e.do(t, "POST", "/devices/backups", "admin", url.Values{"csrf": {"tok"}, "v.LOG_LEVEL": {"nope"}})
	if sched = cardOf(t, body, "schedule"); strings.Contains(sched, `<details class="fold" open>`) {
		t.Error("the fold opens although no schedule field has a problem")
	}
}

// Why (#949): the Backups page is mostly a status page with one small folded form. A save bar that is
// always on screen floats over the rows of the device table while nothing is edited, so the page asks
// sysform.js to show the bar only while there are unsaved changes (data-sbar-auto). The exception is a
// refused save: the page then shows values that were NOT saved, sysform.js would call them the
// starting values and hide the bar, and Save would be gone exactly when the person needs it again.
// Settings and Snapshots are all form, so their bar stays on screen as the #362 tests expect.
func TestBackupsSaveBarShowsOnlyWhileDirtyButNeverAfterARefusedSave(t *testing.T) {
	e := fleetEnv(t, fleetFixture(time.Now()))
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	if !strings.Contains(body, "data-sys-bar data-sbar-auto") {
		t.Errorf("the Backups save bar does not ask to be hidden while the form is clean: %s", between(body, "data-sys-bar", ">"))
	}
	// a refused save re-renders the posted values: the bar stays on screen for that answer
	_, body = e.do(t, "POST", "/devices/backups", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"nope"}})
	if !strings.Contains(body, "data-sys-bar") || strings.Contains(body, "data-sbar-auto") {
		t.Errorf("after a refused save the bar must stay visible (no data-sbar-auto): %s", between(body, "data-sys-bar", ">"))
	}
	// Settings is all form: its bar stays on screen all the time (#362)
	_, body = e.do(t, "GET", "/settings/system", "admin", nil)
	if !strings.Contains(body, "data-sys-bar") || strings.Contains(body, "data-sbar-auto") {
		t.Error("the Settings save bar must stay on screen all the time")
	}
}

// Why (#684): the long list comes last. Everything above the device table is short (the last run, the
// totals, one line each for the deleted devices' backups, the unreferenced files and the schedule), so on a phone nothing
// hides behind 60 table rows. Devices therefore carries the highest phone rank of the page; a card that
// is ranked after it would be at the bottom of the table again.
func TestBackupsPageRanksTheDeviceTableLastOnAPhone(t *testing.T) {
	e, _, _, _ := storageEnv(t)
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	rank := func(id string) int {
		m := regexp.MustCompile(`<section class="scard r(\d)"[^>]*id="card-` + id + `"`).FindStringSubmatch(body)
		if m == nil {
			m = regexp.MustCompile(`<section class="scard[^"]*\br(\d)\b[^"]*" id="card-` + id + `"`).FindStringSubmatch(body)
		}
		if m == nil {
			t.Fatalf("card %s has no phone rank", id)
		}
		return int(m[1][0] - '0')
	}
	devs := rank("devices")
	for _, id := range []string{"lastrun", "storage", "orphans", "schedule"} {
		if r := rank(id); r >= devs {
			t.Errorf("card %s has phone rank %d, the device table %d: the table must come last", id, r, devs)
		}
	}
	if rank("lastrun") != 1 {
		t.Error("the last run is not the first card on a phone")
	}
}

// Why (#684): Back up all now acts on the whole fleet, not on the table. With the table last on a phone,
// a button inside its card would sit far down the page, so it is in the page header, where it stays at
// the top on every width. Back up selected stays by the table and the selection it uses.
func TestBackUpAllNowIsInThePageHeader(t *testing.T) {
	e := fleetEnv(t, fleetFixture(time.Now()))
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	head := body[strings.Index(body, `class="pghead"`):strings.Index(body, `class="syswrap"`)]
	if !strings.Contains(head, `form="bk-all"`) || !strings.Contains(head, "Back up all now") {
		t.Errorf("Back up all now is not in the page header: %s", head)
	}
	devs := cardOf(t, body, "devices")
	if strings.Contains(devs, "Back up all now") || !strings.Contains(devs, "Back up selected") {
		t.Error("the Devices card holds Back up all now, or lost Back up selected")
	}
	// an operator starts nothing
	_, body = e.do(t, "GET", "/devices/backups", "operator", nil)
	if strings.Contains(body, "Back up all now") {
		t.Error("an operator sees Back up all now")
	}
}

// Why (#684): one line per row keeps the table to its length of rows. A device name or a reason that
// wraps makes a row two or three lines high; they are cut to the cell and their full text is the
// cell's title (the layout guard accepts a cut-off cell only with a title).
func TestTableNameAndReasonCellsCarryTheirFullTextAsTitle(t *testing.T) {
	fleet := fleetFixture(time.Now())
	fleet.rows[2].Name = "a-device-name-that-is-longer-than-its-column"
	e := fleetEnv(t, fleet)
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	devs := cardOf(t, body, "devices")
	if !strings.Contains(devs, `class="wrap p1 dv-name" title="a-device-name-that-is-longer-than-its-column"`) {
		t.Errorf("the name cell has no title with the full name: %s", devs)
	}
	if !strings.Contains(devs, `<td data-label="Last result" class="wrap s" title="No answer within 30 s">`) {
		t.Error("the result cell of a failed device has no title with its reason")
	}
}

// Why (#684): the lists of the side column (the devices the next run prunes, the backups of deleted
// devices, the unreferenced files) are as long as the fleet's history. Each card says in one line what it
// holds, and the list is in a fold with its count, so the column stays short; the entries are still on
// the page (a fold only hides them), with their own Keep, Download and Delete.
func TestSideColumnListsAreFoldedBehindOneLine(t *testing.T) {
	_, sb, _, _ := storageEnv(t)
	f := []backup.UnreferencedFile{{Path: "backups/85/20261027T104345Z-dmp.dmp", Size: 2048, ModTime: time.Date(2026, 10, 27, 10, 43, 45, 0, time.UTC)}}
	fleet := fleetFixture(time.Now())
	e, _, _, _ := sysEnvWithDeps(t, func(d *Deps) { d.Backups = &unrefStorage{storageBackups: sb, files: f}; d.BackupFleet = fleet })
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	for _, c := range []struct{ card, fold, entry string }{
		{"storage", `<details class="fold"><summary>Devices <span class="meta">1</span>`, "attic"},
		{"orphans", `<details class="fold"><summary>Backups <span class="meta">`, "garage"},
		{"unreferenced", `<details class="fold"><summary>Files <span class="meta">1</span>`, "20261027T104345Z-dmp.dmp"},
	} {
		card := cardOf(t, body, c.card)
		at := strings.Index(card, c.fold)
		if at < 0 {
			t.Errorf("%s: the list is not in a fold (want %s): %s", c.card, c.fold, card)
			continue
		}
		if !strings.Contains(card[at:], c.entry) {
			t.Errorf("%s: %q is not inside the fold", c.card, c.entry)
		}
		if strings.Contains(card[:at], c.entry) {
			t.Errorf("%s: %q is listed above the fold too", c.card, c.entry)
		}
	}
	if o := cardOf(t, body, "orphans"); !strings.Contains(o, "<b>2 backups · 2.1 KiB · 1 kept.</b>") {
		t.Errorf("the deleted devices' card has no summary line with the count and the kept ones: %s", o)
	}
}

// attrOf is the value of an attribute of a tag, unescaped; "" when the tag has none.
func attrOf(tag, name string) string {
	m := regexp.MustCompile(`\s` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

// Why (#950): during a backup run the page must follow the run without a reload. That needs the event
// stream (so the status bar refreshes too) and a region that reloads the cards a run changes: the
// Devices card (newest backup, runs, last result), Last run, Storage and the Prune now dialog, which is
// built from the same plan. The region reloads from the page's own address with the filter and sort the
// reader chose, never with the notice of a save (?ok=saved would show it again on every refresh), and it
// must always run once after the last event of a burst: the final "Backed up" of a run comes within
// 2 s of the one before, and a throttle would drop it. Every id it replaces must be on the page exactly
// once, also when nothing is to prune, or the swap leaves a stale dialog or logs an error.
func TestBackupsPageIsLive(t *testing.T) {
	now := time.Now()
	for _, role := range []string{"admin", "operator"} {
		for name, fleet := range map[string]*fakeFleet{"with a prune plan": fleetFixture(now), "nothing to prune": {}} {
			t.Run(role+" "+name, func(t *testing.T) {
				e := fleetEnv(t, fleet)
				_, body := e.do(t, "GET", "/devices/backups?filter=failed&sort=newest&dir=desc&ok=saved", role, nil)
				if !strings.Contains(body, `sse-connect="/tf/events?since=`) {
					t.Error("the page opens no event stream")
				}
				if !strings.Contains(body, `hx-get="/tf/statusbar"`) {
					t.Error("the status bar does not refresh itself")
				}
				tag := regexp.MustCompile(`<section[^>]*id="card-devices"[^>]*>`).FindString(body)
				if tag == "" {
					t.Fatal("no Devices card")
				}
				if !strings.Contains(tag, " data-live-region") {
					t.Errorf("the Devices card is no live region (the ticked rows and the open menus are kept by it): %s", tag)
				}
				for _, want := range []string{"sse:backup", "sse:device", "sse:resync"} {
					if !strings.Contains(attrOf(tag, "hx-trigger"), want) {
						t.Errorf("the card does not reload on %s: %s", want, tag)
					}
				}
				if strings.Contains(attrOf(tag, "hx-trigger"), "throttle") {
					t.Errorf("a throttle drops the last event of a run: %s", tag)
				}
				if got := attrOf(tag, "hx-sync"); got != "this:queue last" {
					t.Errorf("hx-sync = %q: the last event of a burst must be asked for once the request in flight ends", got)
				}
				if attrOf(tag, "hx-swap") != "innerHTML" {
					t.Errorf("the card is swapped inside itself, so the region and its ticks stay: %s", tag)
				}
				u, err := url.Parse(attrOf(tag, "hx-get"))
				if err != nil || u.Path != "/tf/devices/backups" {
					t.Fatalf("hx-get = %q", attrOf(tag, "hx-get"))
				}
				q := u.Query()
				if q.Get("filter") != "failed" || q.Get("sort") != "newest" || q.Get("dir") != "desc" || q.Has("ok") {
					t.Errorf("hx-get %q must keep filter, sort and dir and drop the save notice", u.RawQuery)
				}
				oob := attrOf(tag, "hx-select-oob")
				for _, id := range []string{"card-lastrun", "card-storage", "bk-prune"} {
					if !strings.Contains(oob, id) {
						t.Errorf("hx-select-oob %q lacks %s", oob, id)
					}
				}
				for _, id := range strings.Split(oob, ",") {
					id = strings.TrimSpace(id)
					if n := strings.Count(body, ` id="`+id+`"`); n != 1 {
						t.Errorf("the page has %d elements with id %q, want exactly 1 (it is swapped by id)", n, id)
					}
				}
				for _, bad := range []string{"card-schedule", "card-orphans", "card-unreferenced"} {
					if strings.Contains(oob, bad) {
						t.Errorf("%s holds a draft, a fold or a form of its own and is never swapped", bad)
					}
				}
			})
		}
	}
}

// publishingFleet stores a backup while the page is being read: it publishes the event inside Overview.
type publishingFleet struct {
	*fakeFleet
	hub *events.Hub
}

func (f *publishingFleet) Overview(ctx context.Context) ([]model.BackupDeviceRow, error) {
	f.hub.Publish(events.Event{Topic: events.TopicBackup})
	return f.fakeFleet.Overview(ctx)
}

// Why (#160, #950): a backup stored while the page is being built is not in the page, and its event
// is gone by the time the stream opens. The page names the sequence number it was built after, taken
// before the data is read, so the stream says "resync" at once and the cards reload. Taken after the
// read it would swallow that backup and the page would show an old state until the next event.
func TestBackupsPageSinceIsTakenBeforeTheDataIsRead(t *testing.T) {
	pf := &publishingFleet{fakeFleet: fleetFixture(time.Now())}
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = pf }, "TASMOFLEET_BACKUP_DAYS=mon,tue,wed,thu,fri,sat,sun")
	pf.hub = e.hub
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	m := regexp.MustCompile(`sse-connect="/tf/events\?since=(\d+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the page hands no sequence number to its stream")
	}
	if m[1] == strconvSeq(e.hub.Seq()) {
		t.Fatalf("since=%s is the number after the read: the backup stored during the render would be lost", m[1])
	}
	if got := readStream(t, e, m[1], nil, "event: resync"); !strings.Contains(got, "event: resync\n") {
		t.Fatalf("a stream opened with the page's number gets no resync:\n%q", got)
	}
}

// needsPWFacts are the facts of devices on TasmoFleet's broker without an MQTT password (mqttFacts: HTTP failing),
// and of the rest with their own password, keyed by device.
func needsPWFacts(without ...model.DeviceID) fakeFacts {
	now := time.Now()
	f := map[model.DeviceID]transport.Facts{}
	for id := model.DeviceID(1); id <= 6; id++ {
		f[id] = mqttFacts(now, model.MQTTPasswordOwn)
	}
	for _, id := range without {
		f[id] = mqttFacts(now, model.MQTTPasswordNone)
	}
	return fakeFacts{f: f}
}

// Why (#997): a backup that failed because only MQTT could carry it and the device has no usable MQTT password is
// fixed on the device's MQTT password form, which nothing on the Backups page led to (the reporter found it by reading
// the transport code). The Last result cell of such a failed device links that form, for an admin (the form is admin
// only); a failed device whose password is usable gets no link, and neither does a device that was not failed.
// The link follows the live facts (transport.BackupPasswordBlocker), so it goes once the password is set.
func TestBackupsTableLinksThePasswordFormOfAFailedDeviceThatNeedsIt(t *testing.T) {
	now := time.Now()
	link := `href="/tf/devices/3/edit"`
	for _, tc := range []struct {
		name, role string
		without    []model.DeviceID
		want       bool
	}{
		{"failed, no password, admin", "admin", []model.DeviceID{3}, true},
		{"failed, no password, operator", "operator", []model.DeviceID{3}, false},
		{"failed, password usable", "admin", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(now); d.Transport = needsPWFacts(tc.without...) })
			_, body := e.do(t, "GET", "/devices/backups", tc.role, nil)
			devs := cardOf(t, body, "devices")
			if got := strings.Contains(devs, link) && strings.Contains(devs, "Set MQTT password"); got != tc.want {
				t.Fatalf("link to the password form shown = %v, want %v:\n%s", got, tc.want, devs)
			}
		})
	}
	// a device that needs the password but did not fail (attic: OK) has no link
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(now); d.Transport = needsPWFacts(1) })
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	if strings.Contains(cardOf(t, body, "devices"), `href="/tf/devices/1/edit"`) {
		t.Error("a device whose last backup worked needs no link")
	}
}

// Why (#997): when several failed devices need their MQTT password, one note on the Backups page counts them and
// links the Device MQTT passwords card on System, MQTT, where "Use the broker password for these…" sets them all at
// once. One device is served by its own link in the table: no note for it.
func TestBackupsPageNotesSeveralDevicesThatNeedAnMQTTPassword(t *testing.T) {
	now := time.Now()
	note := `href="/tf/settings/system?tab=mqtt#card-devicepasswords"`
	two := fleetFixture(now)
	two.rows[0].LastOutcome, two.rows[0].LastReason = model.BackupOutcomeFailed, "Needs an MQTT password: ..."
	for _, tc := range []struct {
		name  string
		fleet *fakeFleet
		want  bool
	}{
		{"two failed devices need it", two, true},
		{"one failed device needs it", fleetFixture(now), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = tc.fleet; d.Transport = needsPWFacts(1, 3) })
			_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
			devs := cardOf(t, body, "devices")
			got := strings.Contains(devs, note) && strings.Contains(devs, "2 devices failed their last backup and have no usable MQTT password")
			if got != tc.want {
				t.Fatalf("note shown = %v, want %v:\n%s", got, tc.want, devs)
			}
		})
	}
}

// Why (#997): the device's Backups tab names the failure with its reason; when the device needs its MQTT password, the
// failed attempts since the last good backup link the password form, and so does the status line under Back up now and
// Retry right after the refusal. An older failure behind a good backup is history and gets none.
func TestBackupsTabLinksThePasswordFormAfterAFailureThatNeedsIt(t *testing.T) {
	ab := &adminBackups{}
	k := &keepBackups{adminBackups: ab, deviceOf: 1}
	t0 := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	ab.list = []model.Backup{{ID: 4, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: t0.Add(time.Hour), Version: "15.6.0", Size: 4096}}
	k.attempts = []model.BackupAttempt{
		{At: t0.Add(2 * time.Hour), Trigger: model.TriggerManual, Reason: "Needs an MQTT password: newest"},
		{At: t0, Trigger: model.TriggerManual, Reason: "The device did not answer: older"},
	}
	e := setupWith(t, func(d *Deps) { d.Backups = k; d.Transport = needsPWFacts(1) })
	_, body := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	link := `href="/tf/devices/1/edit"`
	newest, older := strings.Index(body, "newest"), strings.Index(body, ": older")
	if newest < 0 || older < 0 {
		t.Fatalf("rows missing:\n%s", body)
	}
	if !strings.Contains(body[newest:strings.Index(body, `value="4"`)], link) {
		t.Errorf("the failure since the last good backup must link the password form:\n%s", body)
	}
	if strings.Contains(body[older:], link) {
		t.Errorf("an older failure behind a good backup gets no link")
	}

	blocked := &transport.BackupBlockedError{Err: tasmota.ErrUnreachable, Reason: "needs an MQTT password: TasmoFleet cannot reach the device over HTTP"}
	for _, tc := range []struct {
		name, role string
		facts      fakeFacts
		want       bool
	}{
		{"admin, password needed", "admin", needsPWFacts(1), true},
		{"operator", "operator", needsPWFacts(1), false},
		{"admin, password usable", "admin", needsPWFacts(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupWith(t, func(d *Deps) { d.Backups = &fakeBackups{failErr: blocked}; d.Transport = tc.facts })
			_, status := e.do(t, "POST", "/devices/1/backup", tc.role, url.Values{"from": {"tab"}})
			if !strings.Contains(status, "Needs an MQTT password") {
				t.Fatalf("status line %q: the reason first", status)
			}
			if got := strings.Contains(status, link); got != tc.want {
				t.Fatalf("status line %q: link = %v, want %v", status, got, tc.want)
			}
		})
	}
}

// Why (#997): Back up selected and Back up all now end on the transaction's result page; a device that failed there
// because it needs its MQTT password links the password form from its row, the place where the reason is read.
func TestBackupTransactionRowLinksThePasswordForm(t *testing.T) {
	now := time.Now()
	jobs := []model.Job{
		{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobFailed, LastError: "Needs an MQTT password: x", CreatedAt: now, UpdatedAt: now},
		{ID: 2, BatchID: 7, DeviceID: 2, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobSucceeded, CreatedAt: now, UpdatedAt: now},
	}
	for _, tc := range []struct {
		name  string
		facts fakeFacts
		want  bool
	}{
		{"failed, password needed", needsPWFacts(1, 2), true},
		{"failed, password usable", needsPWFacts(2), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := resultEnvWith(t, func(d *Deps) { d.Transport = tc.facts }, jobs...)
			_, body := e.do(t, "GET", "/devices/bulk/result/7", "admin", nil)
			if got := strings.Contains(body, `href="/tf/devices/1/edit"`); got != tc.want {
				t.Fatalf("link on the failed row = %v, want %v:\n%s", got, tc.want, body)
			}
			if strings.Contains(body, `href="/tf/devices/2/edit"`) {
				t.Error("a device whose backup worked gets no link")
			}
		})
	}
}
