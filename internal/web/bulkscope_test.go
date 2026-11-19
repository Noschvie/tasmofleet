package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/httpx"
	"github.com/hellwand/tasmofleet/internal/model"
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
	if strings.Contains(between(body, `<main id="main">`, "</main>"), "hx-trigger") { // the status bar outside <main> refreshes itself
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

// Why (#321): only the result page that follows the person's own send may add to the command
// history; opening the transaction later (bookmark, audit log, reload) must not bump an old command.
func TestBulkResultOffersCommandToHistoryOnlyAfterOwnSend(t *testing.T) {
	now := time.Now()
	e := resultEnv(t,
		model.Job{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindCommand, Commands: "Status 5", State: model.JobFailed, CreatedAt: now, UpdatedAt: now},
	)
	_, body := e.do(t, "GET", "/devices/bulk/result/7?id=1", "admin", nil)
	if strings.Contains(body, "data-history-add") {
		t.Error("a later visit offered the command to the history")
	}
	_, body = e.do(t, "GET", "/devices/bulk/result/7?id=1&sent=1", "admin", nil)
	if !strings.Contains(body, `data-history-add="Status 5"`) {
		t.Errorf("the page after the send did not offer the command: %s", body)
	}
}

// Why: a bulk action over many devices ends with a long list; the tiles say how many failed or
// were skipped, and a click on one shows just those devices, so the one failure among 40
// successes is found without scrolling. The counts stay the whole transaction's, a second click
// (or "Show all") shows every device again, and the filter survives the page's own refresh.
func TestBulkResultTilesFilterTheList(t *testing.T) {
	now := time.Now()
	e := resultEnv(t,
		model.Job{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobFailed, CreatedAt: now, UpdatedAt: now},
	)
	_, body := e.do(t, "GET", "/devices/bulk/result/7?id=1&id=2&sent=1", "admin", nil)
	for _, want := range []string{
		`href="/tf/devices/bulk/result/7?id=1&amp;id=2&amp;show=failed"`,
		`href="/tf/devices/bulk/result/7?id=1&amp;id=2&amp;show=skipped"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks tile link %q: %s", want, between(body, `class="bk-tiles"`, "</section>"))
		}
	}
	if strings.Contains(body, "show=ok") {
		t.Error("a tile counting 0 devices filters nothing and is no link")
	}

	_, body = e.do(t, "GET", "/devices/bulk/result/7?id=1&id=2&show=failed", "admin", nil)
	list := between(body, `aria-label="Result per device"`, "</section>")
	if !strings.Contains(list, ">kitchen<") || strings.Contains(list, ">attic<") {
		t.Errorf("show=failed lists only the failed device: %s", list)
	}
	if !strings.Contains(body, `<div class="bk-tile-v">1</div>`) || strings.Count(body, `<div class="bk-tile-v">1</div>`) != 2 {
		t.Errorf("the tiles keep counting the whole transaction: %s", between(body, `class="bk-tiles"`, `aria-label="Result per device"`))
	}
	if !strings.Contains(body, `aria-current="true"`) || !strings.Contains(body, `href="/tf/devices/bulk/result/7?id=1&amp;id=2"`) {
		t.Errorf("the active tile is marked and a second click shows all again: %s", between(body, `class="bk-tiles"`, `aria-label="Result per device"`))
	}

	_, body = e.do(t, "GET", "/devices/bulk/result/7?id=1&id=2&show=skipped", "admin", nil)
	list = between(body, `aria-label="Result per device"`, "</section>")
	if strings.Contains(list, ">kitchen<") || !strings.Contains(list, ">attic<") {
		t.Errorf("show=skipped lists only the skipped device: %s", list)
	}

	_, body = e.do(t, "GET", "/devices/bulk/result/7?id=1&id=2&show=ok", "admin", nil)
	if !strings.Contains(body, "No device in this group.") || !strings.Contains(body, "Show all") {
		t.Errorf("an empty group says so and offers the way back: %s", between(body, `aria-label="Result per device"`, "</section>"))
	}
	_, body = e.do(t, "GET", "/devices/bulk/result/7?id=1&id=2&show=bogus", "admin", nil)
	if list = between(body, `aria-label="Result per device"`, "</section>"); !strings.Contains(list, ">kitchen<") || !strings.Contains(list, ">attic<") {
		t.Errorf("an unknown filter shows every device: %s", list)
	}

	run := resultEnv(t, model.Job{ID: 1, BatchID: 8, DeviceID: 1, Kind: model.KindCommand, Commands: restartCommand, State: model.JobApplying, CreatedAt: now, UpdatedAt: now})
	_, body = run.do(t, "GET", "/devices/bulk/result/8?id=1&show=failed", "admin", nil)
	if !strings.Contains(body, `hx-get="/tf/devices/bulk/result/8?id=1&amp;show=failed"`) {
		t.Errorf("the refresh of a running page keeps the filter: %s", body)
	}
}

// Why: the result names the devices, and the next step after a failure is that device's own
// page (its log, its backups, a retry by hand); every listed device that still exists links there.
func TestBulkResultDeviceLinksToItsPage(t *testing.T) {
	now := time.Now()
	e := resultEnv(t,
		model.Job{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobSucceeded, CreatedAt: now, UpdatedAt: now},
	)
	_, body := e.do(t, "GET", "/devices/bulk/result/7?id=1&id=2", "admin", nil)
	list := between(body, `aria-label="Result per device"`, "</section>")
	for _, want := range []string{`href="/tf/devices/1"`, `href="/tf/devices/2"`} {
		if !strings.Contains(list, want) {
			t.Errorf("lacks device link %q: %s", want, list)
		}
	}
}

// Why: a bulk result of 40 devices is longer than the window. The tiles are the page's
// filter; scrolled down to find a row, they were out of sight, and changing the filter meant
// scrolling back up. The tile bar sticks below the sticky header instead, on the page's own
// background so the rows scrolling beneath do not show through.
func TestBulkResultTilesStayInView(t *testing.T) {
	e := resultEnv(t, model.Job{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobSucceeded, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	_, body := e.do(t, "GET", "/devices/bulk/result/7?id=1", "admin", nil)
	if !strings.Contains(body, `<div class="bk-tilebar"><div class="bk-tiles">`) {
		t.Errorf("the tiles sit in the sticky tile bar: %s", body)
	}
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	_, rule, ok := strings.Cut(string(raw), "\n.bk-tilebar {")
	if !ok {
		t.Fatal("app.css has no .bk-tilebar rule")
	}
	rule, _, _ = strings.Cut(rule, "}")
	for _, want := range []string{"position: sticky", "top: var(--header-h)", "z-index: var(--z-sticky)", "background: var(--color-bg)"} {
		if !strings.Contains(rule, want) {
			t.Errorf(".bk-tilebar lacks %q: %s", want, rule)
		}
	}
	// The header is 94 px on a wide window (top bar, section tabs, two hairlines) and 56 px on a
	// phone, where both rows give way to the phone bar. The height sits next to the header rules for
	// both widths, so the bar does not stick a gap below the phone header with rows showing through.
	css := string(raw)
	if !strings.Contains(css, "--header-h: calc(52px + 40px + 2 * var(--hair-w));") {
		t.Error("app.css lacks the wide header height --header-h")
	}
	hide := strings.Index(css, "  .tb,\n  .sn {\n    display: none;\n  }")
	if hide < 0 {
		t.Fatal("app.css lacks the phone block that hides .tb and .sn")
	}
	phone := css[strings.LastIndex(css[:hide], "@media (max-width: 44.99rem) {"):]
	phone, _, _ = strings.Cut(phone, "\n}\n")
	if !strings.Contains(phone, "--header-h: 56px;") {
		t.Errorf("the phone block sets no --header-h: %s", phone)
	}
}

// Why: the filter shows the rows of a tile's group, and the tile counts them. Both come from the
// same switch over the job's state, so a renamed label (or a new state) cannot make the list show
// other devices than the tile counted.
func TestBulkResultRowGroupIsSetWithTheCount(t *testing.T) {
	if got := (bulkResultRow{Label: "Succeeded"}).Group; got != "" {
		t.Fatalf("a row built without its group has none, got %q", got)
	}
	now := time.Now()
	e := resultEnv(t,
		model.Job{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobCancelled, CreatedAt: now, UpdatedAt: now},
	)
	_, body := e.do(t, "GET", "/devices/bulk/result/7?id=1&id=2&show=failed", "admin", nil)
	if list := between(body, `aria-label="Result per device"`, "</section>"); !strings.Contains(list, ">kitchen<") || strings.Contains(list, ">attic<") {
		t.Errorf("a cancelled job is in the Failed group, as in its tile: %s", list)
	}
}
