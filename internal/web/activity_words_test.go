package web

import (
	"bytes"
	"context"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// #1107 (DS-004, A1-002): one object, one name. A person who reads "Transaction #141" on a device page cannot match it
// to "TX-0141" in the history, and "Transactions" is a word the sidebar's group "Updates" never needed explained. A job
// is named TX-0143 (in prose "job TX-0143") and the page that lists them is Activity, wherever the person meets them.

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style)\b.*?</(script|style)>`)
	labelAttrRe   = regexp.MustCompile(`(?i)\s(?:title|aria-label)="([^"]*)"`)
	// storedRe are values that are kept as they are and shown as they are: a URL path (the 404 page prints the one
	// asked for) and the audit log's action keys and targets (transaction.retry, transaction:7), which the log
	// shows next to the readable label.
	storedRe = regexp.MustCompile(`(?i)\S*/transactions\S*|\btransaction[.:][\w.*:-]*`)
)

// shownText is what a person reads or hears on a page: the text nodes plus the title and aria-label values (a tooltip
// and a screen reader name are read too, and the audit log's tooltip keeps the raw target). Scripts, styles and every other attribute (hrefs, data-*, hx-*: stored keys
// such as the route /transactions or the table key transaction_retry) are left out, so the check follows the words and
// not the plumbing.
func shownText(body string) string {
	body = scriptStyleRe.ReplaceAllString(body, " ")
	var labels []string
	for _, m := range labelAttrRe.FindAllStringSubmatch(body, -1) {
		labels = append(labels, m[1])
	}
	text := tagRe.ReplaceAllString(body, " ")
	all := html.UnescapeString(text + " " + strings.Join(labels, " "))
	return strings.Join(strings.Fields(storedRe.ReplaceAllString(all, " ")), " ")
}

// noTransactionWord fails for every "transaction" (any case) in what a person reads of page.
func noTransactionWord(t *testing.T, name, page string) {
	t.Helper()
	text := shownText(page)
	re := regexp.MustCompile(`(?i).{0,40}transaction.{0,40}`)
	for _, m := range re.FindAllString(text, 5) {
		t.Errorf("%s: a person reads %q; a job is named TX-0143, the page is Activity", name, m)
	}
}

// Why: DS-004 found the same job as "Transaction #141", "transaction #1", "TX-0143" and "Update job #41" on six
// surfaces. The pages below are the ones the audit named; none of them may show the old word.
func TestNoPageNamesAJobATransaction(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	now := time.Now()
	j.jobs = []model.Job{
		{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobAwaitReboot, TargetRelease: "16.1.0", CreatedAt: now},
		{ID: 2, BatchID: 8, DeviceID: 1, Kind: model.KindPolicy, Commands: "TelePeriod 300", State: model.JobFailed, CreatedBy: "adam", CreatedAt: now.Add(-time.Hour)},
	}
	e.aud.events = append(e.aud.events,
		model.AuditEvent{ID: 1, At: now, Actor: "adam", Action: "transaction.retry", Target: "transaction:7", Result: "ok", Detail: `{"devices":[1],"batch":8}`},
		model.AuditEvent{ID: 2, At: now, Actor: "adam", Action: "transaction.cancel", Target: "transaction:7", Result: "ok", Detail: `{"devices":1}`},
	)
	for _, p := range []struct{ name, path string }{
		{"Activity", "/transactions"},
		{"Activity of a device", "/transactions?device=1"},
		{"a job", "/transactions/7"},
		{"a Make compliant job", "/transactions/8"},
		{"cancel a job", "/transactions/7/cancel"},
		{"cancel a job that is not running", "/transactions/8/cancel"},
		{"cancel one device of a job", "/jobs/1/cancel"},
		{"retry a job", "/transactions/8/retry"},
		{"a job that does not exist", "/transactions/999"},
		{"device overview", "/devices/1"},
		{"device history", "/devices/1/history"},
		{"audit log", "/audit?since=all"},
		{"dashboard", "/"},
		{"new update", "/updates"},
	} {
		_, body := e.do(t, "GET", p.path, "admin", nil)
		noTransactionWord(t, p.name, body)
	}
}

// Why: the page names the area in its tab, its heading and the dashboard card, and says what is on it in words a person
// who never ran a "transaction" understands (A1-002, A4-016).
func TestActivityIsTheNameOfTheJobList(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	_, body := e.do(t, "GET", "/transactions", "admin", nil)
	text := shownText(body)
	for _, want := range []string{"Activity", "Updates and other changes sent to devices, with a log for each device."} {
		if !strings.Contains(text, want) {
			t.Errorf("Activity page: %q is missing in %.600s", want, text)
		}
	}
	if !strings.Contains(body, `<h1>Activity</h1>`) {
		t.Errorf("the heading is not Activity: %.400s", body)
	}
	_, dash := e.do(t, "GET", "/", "admin", nil)
	if !strings.Contains(shownText(dash), "Activity") {
		t.Errorf("the dashboard card is not Activity")
	}
}

// Why: a device page lists the jobs it took part in; its link text must be the same TX id the Activity page and the
// status bar show, or the person cannot find the job in the history.
func TestDeviceHistoryNamesJobsByTheirTxID(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(shownText(body), "Job TX-0007") {
		t.Errorf("the device overview does not link its job as \"Job TX-0007\": %s", shownText(body))
	}
}

// Why: the audit target and the summary of an event are the one place a stored number (transaction:7) turns into words;
// they must read like every other page: "job TX-0007".
func TestAuditNamesAJobByItsTxID(t *testing.T) {
	text, href := auditTarget("transaction:7", nil, "/tf")
	if text != "job TX-0007" || href != "/tf/transactions/7" {
		t.Errorf("auditTarget = %q %q, want %q and the unchanged link", text, href, "job TX-0007")
	}
	e, _, _, _ := setupOTA(t)
	e.aud.events = append(e.aud.events, model.AuditEvent{ID: 1, At: time.Now(), Actor: "adam", Action: "transaction.retry", Target: "transaction:7", Result: "ok", Detail: `{"devices":[1],"batch":8}`})
	_, body := e.do(t, "GET", "/audit?since=all", "admin", nil)
	got := shownText(body)
	for _, want := range []string{"Retry job", "job TX-0007", "as TX-0008"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit log: %q is missing in %s", want, got)
		}
	}
}

// Why: Make compliant and Apply GPIO template answer with a page that leads to the job; it must name it like the list.
func TestStartedPagesNameTheJob(t *testing.T) {
	for _, c := range []struct {
		name string
		page func() (string, error)
		want string
	}{
		{"Make compliant", func() (string, error) {
			var b bytes.Buffer
			err := fixStartedPage("/tf", fixStarted{Batch: 8, Skipped: []fixSkipView{{Label: "attic", Reason: "offline"}}}).Render(context.Background(), &b)
			return b.String(), err
		}, "Job TX-0008 was started."},
		{"Apply GPIO template", func() (string, error) {
			var b bytes.Buffer
			err := gpioStartedPage("/tf", fixStarted{Batch: 9, Skipped: []fixSkipView{{Label: "attic", Reason: "offline"}}}).Render(context.Background(), &b)
			return b.String(), err
		}, "Job TX-0009 was started."},
		{"bulk notice", func() (string, error) {
			var b bytes.Buffer
			err := noticePage("/tf", "Backup started", messageData{Text: "was started.", LinkLead: true, Link: "/transactions/7", LinkText: "Job TX-0007"}).Render(context.Background(), &b)
			return b.String(), err
		}, "Job TX-0007 was started."},
	} {
		body, err := c.page()
		if err != nil {
			t.Fatal(err)
		}
		noTransactionWord(t, c.name, body)
		if got := shownText(body); !strings.Contains(got, c.want) || !strings.Contains(got, "Open the job") {
			t.Errorf("%s: want %q and an Open the job button in %s", c.name, c.want, got)
		}
	}
}

// Why: a file the cache keeps for a running job is explained by the job's name; a bare "#3" or "update job 3" (a per-device row id
// nobody sees anywhere else) cannot be found on any page.
func TestNeededTextNamesJobsByTxID(t *testing.T) {
	for _, c := range []struct {
		batches []model.BatchID
		want    string
	}{
		{[]model.BatchID{3}, "job TX-0003"},
		{[]model.BatchID{3, 5}, "jobs TX-0003, TX-0005"},
		{nil, "a running update"},
	} {
		if got := (artifactView{Batches: c.batches}).NeededText(); got != c.want {
			t.Errorf("NeededText(%v) = %q, want %q", c.batches, got, c.want)
		}
	}
}

// Why: the retry page's heading, its crumb and its phone title are what a person sees after clicking "Retry failed" on
// TX-0008; they must say TX-0008, as the job page does, and not "Retry transaction 8" (A4-015).
func TestRetryPageNamesTheJob(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = append(j.jobs, model.Job{ID: 2, BatchID: 8, DeviceID: 1, Kind: model.KindPolicy, Commands: "TelePeriod 300", State: model.JobFailed, CreatedAt: time.Now()})
	_, body := e.do(t, "GET", "/transactions/8/retry", "admin", nil)
	if !strings.Contains(body, "<title>Retry job TX-0008") && !strings.Contains(shownText(body), "Retry job TX-0008") {
		t.Errorf("the retry page does not name TX-0008: %.500s", shownText(body))
	}
}
