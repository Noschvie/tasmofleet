package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
)

// fakeBackupBatch records the backup transaction the page asks for.
type fakeBackupBatch struct {
	mu   sync.Mutex
	ids  [][]model.DeviceID
	opts []ota.BackupOptions
	skip []ota.Refusal
}

func (f *fakeBackupBatch) Submit(_ context.Context, _ string, ids []model.DeviceID, opt ota.BackupOptions) (model.BatchID, []ota.Refusal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = append(f.ids, ids)
	f.opts = append(f.opts, opt)
	return 21, f.skip, nil
}

// actionEnv has device 1 online and device 2 offline.
func actionEnv(t *testing.T) (*env, *fakeBackupBatch, *fakeBatch) {
	t.Helper()
	fbk, fb := &fakeBackupBatch{}, &fakeBatch{}
	e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{}; d.CommandBatch = fb; d.BackupBatch = fbk })
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOffline, LastSeen: time.Now()}
	return e, fbk, fb
}

// Why: bulk backup is an admin action; an operator or anonymous caller must not
// start it, and nothing is submitted.
func TestBulkActionsAreAdminOnly(t *testing.T) {
	e, fbk, fb := actionEnv(t)
	for _, path := range []string{"/devices/bulk/backup", "/devices/bulk/restart", "/devices/bulk/restart/run"} {
		for role, want := range map[string]int{"operator": 403, "": 401} {
			if r, _ := e.do(t, "POST", path, role, bulkForm("id", "1", "run", "1")); r.StatusCode != want {
				t.Errorf("%s as %q: %d, want %d", path, role, r.StatusCode, want)
			}
		}
	}
	if len(fbk.ids) != 0 || len(fb.reqs) != 0 {
		t.Fatal("nothing may be submitted")
	}
}

// Why: the bar's post opens the scope page, never an action: the selected devices in the
// order they were posted, who is skipped and why, the run button counting only the online
// ones. Nothing is submitted and no restart token exists for a page that cannot run.
func TestBulkScopeListsTheSelectionAndWhoIsSkipped(t *testing.T) {
	e, fbk, fb := actionEnv(t)
	for _, kind := range []string{"backup", "restart"} {
		resp, body := e.do(t, "POST", "/devices/bulk/"+kind, "admin", bulkForm("id", "2", "id", "1"))
		if resp.StatusCode != 200 || !strings.Contains(body, "Devices (2)") || !strings.Contains(body, "Will be skipped") ||
			!strings.Contains(body, "1 device run · 1 skipped") || !strings.Contains(body, "1 device is offline or unknown and will be skipped.") {
			t.Fatalf("%s: %d %s", kind, resp.StatusCode, body)
		}
		if strings.Index(body, ">attic<") > strings.Index(body, ">kitchen<") {
			t.Errorf("%s: the devices keep the order of the list they were selected in", kind)
		}
	}
	if len(fbk.ids) != 0 || len(fb.reqs) != 0 {
		t.Fatal("the scope page must submit nothing")
	}
}

// Why: the selection is the scope (no implicit "all devices"): a post without devices, or with
// only devices that are not online, runs nothing and gives no token; the run button is disabled.
func TestBulkActionsNeedOnlineSelectedDevices(t *testing.T) {
	e, fbk, fb := actionEnv(t)
	for _, kind := range []string{"backup", "restart"} {
		for name, form := range map[string]url.Values{
			"nothing selected":     bulkForm("run", "1"),
			"all is no scope":      bulkForm("scope", "all", "run", "1"),
			"only an offline one":  bulkForm("id", "2", "run", "1"),
			"an unknown device id": bulkForm("id", "99", "run", "1"),
		} {
			resp, body := e.do(t, "POST", "/devices/bulk/"+kind, "admin", form)
			if resp.StatusCode != http.StatusUnprocessableEntity || strings.Contains(body, `name="token"`) || !strings.Contains(body, "disabled") {
				t.Errorf("%s %s: %d %s", kind, name, resp.StatusCode, body)
			}
		}
	}
	if len(fbk.ids) != 0 || len(fb.reqs) != 0 {
		t.Fatal("nothing may be submitted")
	}
}

// Why: "×" on a row takes the device out of the scope; it only redraws the page, even on the
// backup form whose run button starts at once, and the options typed so far stay.
func TestBulkScopeRemoveRedrawsWithoutTheDevice(t *testing.T) {
	e, fbk, _ := actionEnv(t)
	resp, body := e.do(t, "POST", "/devices/bulk/backup", "admin", bulkForm("id", "1", "id", "2", "opts", "1", "run", "1", "label", "before winter", "drop", "2"))
	if resp.StatusCode != 200 || len(fbk.ids) != 0 {
		t.Fatalf("a removal must not start the backup: %d %v", resp.StatusCode, fbk.ids)
	}
	if !strings.Contains(body, "Devices (1)") || strings.Contains(body, ">attic<") || !strings.Contains(body, `value="before winter"`) {
		t.Fatalf("redrawn without the device, label kept: %s", body)
	}
}

// Why: backup needs no confirmation, so the scope form starts the transaction with the
// options (N1: keep, label). Only online devices are backed up; the result page lists the
// offline one as skipped, so nobody believes it was backed up.
func TestBulkBackupStartsAndOpensTheResult(t *testing.T) {
	e, fbk, _ := actionEnv(t)
	resp, _ := e.do(t, "POST", "/devices/bulk/backup", "admin", bulkForm("id", "1", "id", "2", "opts", "1", "run", "1", "keep", "1", "label", "before winter"))
	if resp.StatusCode != http.StatusSeeOther || len(fbk.ids) != 1 || len(fbk.ids[0]) != 1 || fbk.ids[0][0] != 1 {
		t.Fatalf("%d %v", resp.StatusCode, fbk.ids)
	}
	if loc := resp.Header.Get("Location"); loc != "/tf/devices/bulk/result/21?id=1&id=2" {
		t.Errorf("result page with the posted devices in their order: %q", loc)
	}
	if o := fbk.opts[0]; !o.Keep || o.Label != "before winter" {
		t.Errorf("options: %+v", o)
	}
}

// Why: devices the engine leaves out (busy) are listed with the reason; the result page
// would not know it.
func TestBulkBackupListsEngineRefusals(t *testing.T) {
	e, fbk, _ := actionEnv(t)
	fbk.skip = []ota.Refusal{{DeviceID: 1, Reason: "already has active job 4"}}
	_, body := e.do(t, "POST", "/devices/bulk/backup", "admin", bulkForm("id", "1", "run", "1"))
	for _, want := range []string{"left out", "kitchen", `<a href="/tf/transactions/21">Transaction #21</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q: %s", want, body)
		}
	}
}

// Why: a restart takes devices offline, so its scope page is the confirmation that names the
// consequence. It submits nothing; the run takes devices and command from the server-held
// token, so a forged form cannot add devices or change the command.
func TestBulkRestartScopeNamesTheConsequenceThenRuns(t *testing.T) {
	e, _, fb := actionEnv(t)
	resp, body := e.do(t, "POST", "/devices/bulk/restart", "admin", bulkForm("id", "1", "id", "2"))
	if resp.StatusCode != 200 || !strings.Contains(body, "Relays switch off for a few seconds") || !strings.Contains(body, "Restart 1 device") || !strings.Contains(body, "Restart 1 saves the settings") {
		t.Fatalf("scope: %d %s", resp.StatusCode, body)
	}
	if len(fb.reqs) != 0 {
		t.Fatal("the scope page must send nothing")
	}
	token := confirmToken(t, body)
	resp, _ = e.do(t, "POST", "/devices/bulk/restart/run", "admin", bulkForm("token", token, "id", "1", "id", "2", "id", "3", "command", "Reset 1", "parallel", "3"))
	if resp.StatusCode != http.StatusSeeOther || len(fb.reqs) != 1 || len(fb.reqs[0]) != 1 {
		t.Fatalf("run: %d %v", resp.StatusCode, fb.reqs)
	}
	if r := fb.reqs[0][0]; r.DeviceID != 1 || len(r.Commands) != 1 || r.Commands[0] != "Restart 1" {
		t.Fatalf("exactly Restart 1 to the online device: %+v", r)
	}
	if resp, _ := e.do(t, "POST", "/devices/bulk/restart/run", "admin", bulkForm("token", token)); resp.StatusCode != http.StatusConflict || len(fb.reqs) != 1 {
		t.Fatalf("a token works once: %d", resp.StatusCode)
	}
}

// Why: the flags switch the bulk bar's buttons; a server without the services
// must not offer them, and the routes do not exist then either.
func TestBulkActionFeatureFlags(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{} })
	if r, _ := e.do(t, "POST", "/devices/bulk/backup", "admin", bulkForm("id", "1", "run", "1")); r.StatusCode != 404 && r.StatusCode != 405 {
		t.Errorf("backup without a service: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/devices/bulk/restart", "admin", bulkForm("id", "1")); r.StatusCode != 404 && r.StatusCode != 405 {
		t.Errorf("restart without a service: %d", r.StatusCode)
	}
}

// Why: a backup transaction is shown like the others: it names "Back up" as its
// target (not an empty release) and says what the step does, and an operator can
// neither open the retry form nor retry it.
func TestBackupTransactionPagesAndRetryAreAdminOnly(t *testing.T) {
	e, o, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobFailed, CreatedAt: time.Now()}}
	_, body := e.do(t, "GET", "/transactions/7", "operator", nil)
	if !strings.Contains(body, "Back up") || strings.Contains(body, "Sending the command") {
		t.Errorf("transaction page: %s", body)
	}
	if r, _ := e.do(t, "GET", "/transactions/7/retry", "operator", nil); r.StatusCode != 403 {
		t.Errorf("operator form: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/transactions/7/retry", "operator", url.Values{}); r.StatusCode != 403 || len(o.retryCalls) != 0 {
		t.Errorf("operator retry: %d", r.StatusCode)
	}
	o.retryRes = ota.RetryResult{Batch: 10, Devices: []model.DeviceID{1}}
	if r, _ := e.do(t, "POST", "/transactions/7/retry", "admin", url.Values{}); r.StatusCode != 303 || len(o.retryCalls) != 1 {
		t.Errorf("admin retry needs no new confirmation: %d", r.StatusCode)
	}
}

// Why: Back up and Restart exist as routes, but an admin can only reach them from
// the bulk bar: both buttons post the checked ids.
// Operators never see them, and a server without the service does not offer them.
func TestBulkBarOffersBackupAndRestartToAdmins(t *testing.T) {
	e, _, _ := actionEnv(t)
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	for _, want := range []string{`formaction="/tf/devices/bulk/backup"`, `formaction="/tf/devices/bulk/restart"`} {
		if n := strings.Count(body, want); n != 1 { // once: the bar has no folded copy any more (design v2)
			t.Errorf("admin: %q appears %d times, want 1", want, n)
		}
	}
	if strings.Contains(body, `data-scope="later"`) {
		t.Error("the later placeholder must be gone")
	}
	_, body = e.do(t, "GET", "/devices", "operator", nil)
	if strings.Contains(body, "/devices/bulk/backup") || strings.Contains(body, "/devices/bulk/restart") {
		t.Error("operator must not be offered the bulk backup or restart")
	}
	e2 := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{} })
	_, body = e2.do(t, "GET", "/devices", "admin", nil)
	if strings.Contains(body, "/devices/bulk/backup") || strings.Contains(body, "/devices/bulk/restart") {
		t.Error("without the services the buttons must not be offered")
	}
}

// Why: while a backup runs the device page said "being updated ... does not
// answer while it downloads and reboots", which is wrong for a backup.
func TestDevicePageSaysABackupIsRunning(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobApplying, CreatedAt: time.Now()}}
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, "A backup is running") || strings.Contains(body, "being updated") {
		t.Errorf("device page: %s", body)
	}
}

// Why: the retry page promised "the same command ... if the command policy still
// allows it" for a backup, which sends no command at all.
func TestBackupRetryPageHasItsOwnText(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobFailed, LastError: "device unreachable", CreatedAt: time.Now()}}
	_, body := e.do(t, "GET", "/transactions/7/retry", "admin", nil)
	if !strings.Contains(body, "takes a new backup") || strings.Contains(body, "command policy") {
		t.Errorf("retry page: %s", body)
	}
	if !strings.Contains(body, `<small class="muted keep">`) {
		t.Error("the failure reason must stay visible on a phone (class keep)")
	}
}
