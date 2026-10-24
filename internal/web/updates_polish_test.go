package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
)

// Why: a refused device is blocked, not "left alone". Counting it as skipped hid from the
// summary that devices were refused (the red Refuse badge sat in the table below only).
func TestPreviewCountsRefusedApartFromSkipped(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	o.items[2] = ota.PreviewItem{DeviceID: 2, Decision: "refuse", Reason: "unknown hardware"}
	o.items[3] = ota.PreviewItem{DeviceID: 3, Decision: "skip", Reason: "already at target"}
	form := url.Values{"id": {"1", "2", "3"}, "release": {"16.1.0"}, "policy": {"only-newer"}}
	_, body := e.do(t, "POST", "/updates/preview", "operator", form)
	for _, want := range []string{"<dt>Devices</dt><dd><b>1</b></dd>", `<dt>Refused</dt><dd class="upd-bad">1 device</dd>`, "<dt>Skipped</dt><dd>1 device</dd>"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary lacks %q: %s", want, body)
		}
	}
	// without a refusal there is no refused line at all
	o.items = map[model.DeviceID]ota.PreviewItem{3: {DeviceID: 3, Decision: "skip", Reason: "already at target"}}
	_, body = e.do(t, "POST", "/updates/preview", "operator", url.Values{"id": {"1", "3"}, "release": {"16.1.0"}, "policy": {"only-newer"}})
	if strings.Contains(body, "<dt>Refused</dt>") || !strings.Contains(body, "<dt>Skipped</dt><dd>1 device</dd>") {
		t.Errorf("no refusal, no refused line: %s", body)
	}
}

// Why: a row says what happens to its device and nothing else. An updated device shows its
// versions and file; a device that is not updated shows why, and never a version, a file or a
// power restore line (the preview once printed a restore line for a device it would not touch).
func TestPreviewRowsShowWhatHappensAndNothingElse(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	o.items[1] = ota.PreviewItem{DeviceID: 1, Decision: "upgrade", Plan: model.PlanDirect, Filename: "tasmota.bin.gz", PowerRestore: "1"}
	o.items[2] = ota.PreviewItem{DeviceID: 2, Decision: "refuse", Reason: "unknown hardware", PowerRestore: "1"}
	form := url.Values{"id": {"1", "2"}, "release": {"16.1.0"}, "policy": {"only-newer"}}
	_, body := e.do(t, "POST", "/updates/preview", "operator", form)
	rows := strings.Split(body, `<div class="upd-prow">`)[1:]
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %s", len(rows), body)
	}
	up, refused := rows[0], rows[1]
	if !strings.Contains(up, "upd-prow-file") || !strings.Contains(up, "→ <b>16.1.0</b>") || !strings.Contains(strings.ToLower(up), "relay 1 on again") {
		t.Errorf("an updated device shows its versions, file and power restore: %s", up)
	}
	if strings.Contains(up, "upd-prow-why") {
		t.Errorf("an updated device has no reason line: %s", up)
	}
	refused, _, _ = strings.Cut(refused, "</section>")
	if !strings.Contains(refused, "upd-prow-why") || strings.Contains(refused, "upd-prow-file") || strings.Contains(refused, "upd-prow-ver") {
		t.Errorf("a refused device shows its reason only: %s", refused)
	}
	if strings.Contains(strings.ToLower(refused), "on again") {
		t.Errorf("a device that is not updated has no power restore: %s", refused)
	}
}

// Why: Push upload is ESP32 only. The picker knows each row's chip family, so it can say before
// the preview that selected ESP8266 devices download instead; the element is the script's anchor.
func TestUpdateFormHasThePushHint(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	_, body := e.do(t, "GET", "/updates/new", "operator", nil)
	if !strings.Contains(body, `data-push-hint hidden`) {
		t.Fatalf("no push hint anchor: %s", body)
	}
	if !strings.Contains(body, `data-family="esp8266"`) {
		t.Fatalf("the rows must carry their chip family for the hint: %s", body)
	}
}

// Why: the facts (state, who started it, the counts) sit in the region that refreshes itself; in
// a header drawn once they went stale while the jobs ran (the duration once stayed at 0s).
func TestTransactionFactsAreLive(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobAwaitReboot, TargetRelease: "16.1.0", CreatedBy: "adam", CreatedAt: time.Now().Add(-time.Minute)}}
	_, page := e.do(t, "GET", "/transactions/7", "operator", nil)
	if !strings.Contains(page, `<div id="txn" hx-get="/tf/transactions/7/live?job=1" hx-trigger="sse:job throttle:1s, sse:resync"`) {
		t.Errorf("the page body is not the live region: %s", page)
	}
	_, live := e.do(t, "GET", "/transactions/7/live?job=1", "operator", nil)
	for _, want := range []string{"Update to Tasmota 16.1.0", ">Running<", "started by adam", "<b>1</b> in progress", `id="batch-summary"`} {
		if !strings.Contains(live, want) {
			t.Errorf("the refreshed body lacks %q: %s", want, live)
		}
	}
	if strings.Contains(live, "<html") {
		t.Errorf("the refresh is a fragment, not a page")
	}
}

// Why: after "Cancel remaining" a job that is flashing cannot stop at once. It kept its Cancel
// button and said only "Flashing", so it looked as if the cancel had not taken effect.
func TestCancelRequestedJobShowsItAndHidesItsCancelButton(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{
		{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobUpgradeIssued, CancelRequested: true, TargetRelease: "16.1.0", CreatedAt: time.Now()},
		{ID: 2, BatchID: 7, DeviceID: 2, State: model.JobUpgradeIssued, TargetRelease: "16.1.0", CreatedAt: time.Now()},
	}
	_, body := e.do(t, "GET", "/transactions/7?job=1", "operator", nil)
	if strings.Contains(body, `data-dlg-action="/tf/jobs/`) {
		t.Errorf("a job with a pending cancel offers no Cancel: %s", body)
	}
	if !strings.Contains(body, ">Cancelling<") || !strings.Contains(body, "<b>1</b> cancel requested") {
		t.Errorf("the job and the counts say the cancel was requested: %s", body)
	}
	_, body = e.do(t, "GET", "/transactions/7?job=2", "operator", nil)
	if strings.Count(body, `data-dlg-action="/tf/jobs/`) != 1 || !strings.Contains(body, `/tf/jobs/2/cancel`) {
		t.Errorf("the job without a pending cancel keeps its Cancel: %s", body)
	}
}

// Why: the log line was "Cancel cancelled by adam": the event name and its detail said the
// same thing twice.
func TestCancelLogLineReadsAsOneSentence(t *testing.T) {
	for _, tc := range []struct {
		to   model.JobState
		want string
	}{
		{model.JobCancelled, "Cancelled by adam"},
		{model.JobUpgradeIssued, "Cancel requested by adam"},
	} {
		got := eventText(model.JobEvent{Event: "cancel", To: tc.to, Detail: "cancelled by adam"})
		if got != tc.want {
			t.Errorf("to %s: %q, want %q", tc.to, got, tc.want)
		}
	}
}

// Why: Restart 1 is an operator command, but the devices drop offline for a while. The preview
// gave the same page as Status 2: no consequence and the same neutral button.
func TestBulkCommandPreviewOfARestartNamesTheConsequence(t *testing.T) {
	e, _ := bulkEnv(t)
	_, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "command", "Restart 1"))
	if !strings.Contains(body, `class="banner tone-danger"`) || !strings.Contains(body, "offline for a few seconds") {
		t.Errorf("no consequence for a restart: %s", body)
	}
	if !strings.Contains(body, `class="btn btn-danger-solid">Restart 1 device</button>`) {
		t.Errorf("the confirming button repeats the verb and tone: %s", body)
	}
	_, body = e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "command", "Status 2"))
	if strings.Contains(body, `class="banner tone-danger"`) || !strings.Contains(body, `class="btn btn-primary">Send to 1 device</button>`) {
		t.Errorf("a read command stays neutral: %s", body)
	}
}

// Why: the retry card is wider than a .stack form (34rem), so its table and its footer rule
// stopped short of the card's right edge. The form must use the card's full width.
func TestRetryFormUsesTheFullCardWidth(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 2, BatchID: 7, DeviceID: 2, State: model.JobFailed, TargetRelease: "16.1.0", CreatedAt: time.Now()}}
	_, body := e.do(t, "GET", "/transactions/7/retry", "operator", nil)
	if !strings.Contains(body, `retry" class="stack wide"`) {
		t.Errorf("the retry form is capped at the narrow stack width: %s", body)
	}
}

// Why: a two-column table (Device, Result) has nothing to hide or move, so a Columns button
// on it only makes the toolbar jump between pages. Tables with more columns keep it.
func TestColumnsChooserIsOffTwoColumnTables(t *testing.T) {
	two := `<table class="dense" data-table="bulk_command"><thead><tr><th>Device</th><th>Result</th></tr></thead><tbody><tr><td>a</td><td>x</td></tr><tr><td>b</td><td>y</td></tr></tbody></table>`
	if out := string(applyTablePrefs([]byte(two), nil)); strings.Contains(out, "colmenu") {
		t.Errorf("a two-column table got a Columns chooser: %s", out)
	}
	three := `<table class="dense" data-table="transaction_retry"><thead><tr><th>Device</th><th>Target</th><th>Last result</th></tr></thead><tbody><tr><td>a</td><td>x</td><td>z</td></tr><tr><td>b</td><td>y</td><td>z</td></tr></tbody></table>`
	if out := string(applyTablePrefs([]byte(three), nil)); !strings.Contains(out, "colmenu") {
		t.Errorf("a three-column table lost its Columns chooser: %s", out)
	}
}

// Why: the header (state, "n of m done", counts, duration, Cancel) must follow the job cards. It
// refreshed on the throttled job event only, which can arrive before the state is committed or be
// dropped inside the throttle window, so it stayed on "0 of 3 done" while the cards had finished.
// An active transaction therefore re-reads its header itself, like an active job card does.
func TestTransactionHeaderRefreshesItselfWhileActive(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobAwaitReboot, CreatedAt: time.Now()}}
	_, live := e.do(t, "GET", "/transactions/7/live?job=1", "operator", nil)
	if !strings.Contains(live, `hx-get="/tf/transactions/7/live?job=1" hx-trigger="every 3s" hx-target="#txn"`) {
		t.Errorf("an active transaction does not re-read itself: %s", live)
	}
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobSucceeded, CreatedAt: time.Now()}}
	_, live = e.do(t, "GET", "/transactions/7/live?job=1", "operator", nil)
	if strings.Contains(live, "every 3s") {
		t.Errorf("a finished transaction keeps polling: %s", live)
	}
}

// Why: a retry page with nothing to retry was a dead end (a warning and no way on), and its
// breadcrumb said "Transactions / #7" while the transaction itself is "Updates / TX-0007".
func TestRetryWithNothingToRetryLinksBackToTheTransaction(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobSucceeded, TargetRelease: "16.1.0", CreatedAt: time.Now()}}
	_, body := e.do(t, "GET", "/transactions/7/retry", "operator", nil)
	if !strings.Contains(body, "No failed or cancelled device to retry.") {
		t.Fatalf("no empty state: %s", body)
	}
	if !strings.Contains(body, `href="/tf/transactions/7"`) || !strings.Contains(body, "Back to the transaction") {
		t.Errorf("the empty retry page offers no way back to the transaction: %s", body)
	}
	if !strings.Contains(body, "TX-0007") || strings.Contains(body, ">#7<") {
		t.Errorf("the breadcrumb does not name the transaction like its own page: %s", body)
	}
}
