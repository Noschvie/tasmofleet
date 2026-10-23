package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// resultEnv serves the bulk result page: the engine (OTA, jobs) and the bulk services.
// Devices: 1 kitchen (online), 2 attic (offline).
func resultEnv(t *testing.T, jobs ...model.Job) *env {
	t.Helper()
	e := setup(t)
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOffline, LastSeen: time.Now()}
	h, err := Handler(Deps{Guard: fakeGuard{}, Inventory: e.inv, Health: e.health, Backups: e.bk, Prober: e.pr, Audit: e.aud, Recorder: e.aud,
		Hub: e.hub, OTA: &fakeOTA{}, Jobs: &fakeJobs{jobs: jobs}, Batches: fakeBatches{},
		DeviceCmd: &fakeCmd{}, CommandBatch: &fakeBatch{}, BackupBatch: &fakeBackupBatch{}})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Close()
	e.srv = httptest.NewServer(http.NewServeMux())
	mux := http.NewServeMux()
	mux.Handle("/tf/", httpx.Proxy{BasePath: "/tf"}.Middleware(h))
	e.srv.Config.Handler = mux
	t.Cleanup(e.srv.Close)
	return e
}

// Why: the result page is where a bulk action ends: it counts Succeeded / Failed / Skipped,
// lists every device with its own answer (the skipped ones too, which the transaction does not
// hold), names the state beside the title and offers "Retry failed" to admins. The command
// line is shown only in its redacted form: the stored text may hold a secret.
func TestBulkResultCountsAndListsEveryDevice(t *testing.T) {
	now := time.Now()
	e := resultEnv(t,
		model.Job{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindCommand, Commands: "Rule1 ON x DO Publish a hunter2 ENDON", State: model.JobFailed, CreatedAt: now, UpdatedAt: now},
	)
	resp, body := e.do(t, "GET", "/devices/bulk/result/7?id=2&id=2&id=99&id=1", "admin", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"Command finished", "Finished · 1 failed", "› Rule1 ***", "Retry failed (1)", `href="/tf/transactions/7/retry"`,
		">kitchen<", ">attic<", "Offline. Not attempted.", `<div class="bk-tile-v">1</div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(body, "hunter2") {
		t.Error("the raw command must never reach the page")
	}
	if n := strings.Count(body, ">attic<"); n != 1 {
		t.Errorf("a skipped device is listed once, an unknown id not at all: %d", n)
	}
	if strings.Index(body, ">attic<") > strings.Index(body, ">kitchen<") {
		t.Error("the devices keep the order they were posted in")
	}
	if strings.Contains(body, "hx-trigger") {
		t.Error("a finished result does not reload itself")
	}
}

// Why: a bulk transaction runs in the background; its result page follows it (job events, and a
// timer as a fallback) until every device is done, and names the kind of the action.
func TestBulkResultFollowsARunningTransaction(t *testing.T) {
	now := time.Now()
	e := resultEnv(t,
		model.Job{ID: 1, BatchID: 8, DeviceID: 1, Kind: model.KindCommand, Commands: restartCommand, State: model.JobApplying, CreatedAt: now, UpdatedAt: now},
	)
	_, body := e.do(t, "GET", "/devices/bulk/result/8", "admin", nil)
	// a running page does not claim it "took" a time: it has been running that long
	if strings.Contains(body, "took") || !strings.Contains(body, "running for") {
		t.Errorf("running page says took: %s", body)
	}
	for _, want := range []string{"Restart running", ">Running<", `hx-get="/tf/devices/bulk/result/8"`, `hx-select="#bk-result"`} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q: %s", want, body)
		}
	}
	e = resultEnv(t, model.Job{ID: 1, BatchID: 9, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobSucceeded, CreatedAt: now, UpdatedAt: now})
	_, body = e.do(t, "GET", "/devices/bulk/result/9", "admin", nil)
	if !strings.Contains(body, "Backups finished") || !strings.Contains(body, "Completed") || strings.Contains(body, "Retry failed") {
		t.Errorf("backup result: %s", body)
	}
}

// Why: only bulk transactions have this page (an update has its own transaction page), and it is
// an admin page like the actions that start them.
func TestBulkResultIsForBulkTransactionsAndAdmins(t *testing.T) {
	now := time.Now()
	e := resultEnv(t,
		model.Job{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobSucceeded, TargetRelease: "16.1.0", CreatedAt: now},
		model.Job{ID: 2, BatchID: 8, DeviceID: 1, Kind: model.KindCommand, Commands: "Status 2", State: model.JobSucceeded, CreatedAt: now},
	)
	if r, _ := e.do(t, "GET", "/devices/bulk/result/7", "admin", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("an update transaction: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "GET", "/devices/bulk/result/8", "operator", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("operator: %d", r.StatusCode)
	}
}

// Why: Enter in a field of the scope form (the backup label) submits with the form's default
// button, the first submit button in tree order. Every row has a "×" submit button; if one of
// them came first, Enter would silently take a device out of the selection instead of running.
func TestBulkScopeDefaultButtonRunsTheAction(t *testing.T) {
	e, _, _ := actionEnv(t)
	for _, kind := range []string{"backup", "restart", "command"} {
		_, body := e.do(t, "POST", "/devices/bulk/"+kind, "admin", bulkForm("id", "1", "id", "2"))
		first := strings.Index(body, `type="submit"`)
		drop := strings.Index(body, `name="drop"`)
		if first < 0 || drop < 0 || first > drop {
			t.Fatalf("%s: the first submit button must not be a row's remove button: %s", kind, body)
		}
		if tag := body[strings.LastIndex(body[:first], "<"):first]; strings.Contains(tag, "name=") {
			t.Errorf("%s: the default button posts no field of its own: %s", kind, tag)
		}
	}
}

// Why: "×" redraws the restart page; the admin's "Devices at a time" must survive it, or the
// restart would run with a parallelism they did not choose.
func TestBulkRestartRedrawKeepsDevicesAtATime(t *testing.T) {
	e, _, _ := actionEnv(t)
	_, body := e.do(t, "POST", "/devices/bulk/restart", "admin", bulkForm("id", "1", "id", "2", "opts", "1", "parallel", "1", "drop", "2"))
	if !strings.Contains(body, `<option value="1" selected>`) {
		t.Errorf("the chosen parallelism is lost: %s", body)
	}
	_, body = e.do(t, "POST", "/devices/bulk/restart", "admin", bulkForm("id", "1", "parallel", "7"))
	if !strings.Contains(body, `<option value="3" selected>`) {
		t.Errorf("an invalid value falls back to the default 3: %s", body)
	}
}
