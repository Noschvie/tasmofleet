package web

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/model"
)

// The words the design system's Wording table (docs/spec/design-system.md) allows for the result of a
// thing. They are kept here as literals on purpose, not read from the labels map: a test that takes its
// words from the code it checks cannot fail when the code drifts.
var (
	resultWords = []string{"Succeeded", "Failed", "Skipped", "Cancelled", "Running", "Waiting"}

	// stepWords are the words of a step of a job: what it is doing right now. "Done" (this step is over) is
	// a step word only on a job's own row (the transaction page), see requireResultWords.
	stepWords = []string{"Checking", "Backing up", "Downloading", "Flashing", "Rebooting", "Verifying", "Restoring",
		"Applying", "Applied", "Preparing", "Cleaning up", "Switching back", "Restarting into safeboot"}

	// namedExceptions are states none of the six words covers (decisions 5 and 15 of the 0.48.0 plan).
	namedExceptions = []string{
		"Refused",     // the permission check said no; that is not a failure of the action
		"Not started", // the planned start passed while TasmoFleet was down
		"Scheduled",   // a transaction that waits for its start time
		"Planned",     // a job that waits for its start time
		"Cancelling",  // a flash cannot be stopped: the job ends after this step
	}

	// healthWords are the connection states of a device (Online, Offline, ...), shown in the same Activity box.
	healthWords = []string{"Online", "Stale", "Offline", "Auth failed", "Unknown"}
)

// resultCountList is "30 succeeded · 1 failed": the counts of a mixed result, in the lower-cased result words.
var resultCountList = regexp.MustCompile(`^\d+ (succeeded|failed|skipped|cancelled)( · \d+ (succeeded|failed|skipped|cancelled))*$`)

func allowedResultLabel(l string, extra ...string) bool {
	return slices.Contains(resultWords, l) || slices.Contains(extra, l) || slices.Contains(stepWords, l) || slices.Contains(namedExceptions, l) ||
		slices.Contains(healthWords, l) || resultCountList.MatchString(l)
}

var badgeLabelRe = regexp.MustCompile(`<span class="bdg[ "][^>]*>([^<]*)</span>`)

func badgeLabels(body string) []string {
	var out []string
	for _, m := range badgeLabelRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func requireResultWords(t *testing.T, page, region string, extra ...string) {
	t.Helper()
	got := badgeLabels(region)
	if len(got) == 0 {
		t.Fatalf("%s: no badge in the region, the test checks nothing: %s", page, region)
	}
	for _, l := range got {
		if !allowedResultLabel(l, extra...) {
			t.Errorf("%s: the badge %q is not one of the result words (%v), a step word, a named exception or a count list", page, l, resultWords)
		}
	}
}

// Why (#1105, audit DS-003): the same outcome was called Completed on one page, Done on a second, OK on a
// third and Succeeded on a fourth, and "Queued" while the design system says Waiting. A person who reads two
// names for one outcome takes them for two things that happened. Every badge of a finished or waiting thing
// on the pages that show results is one of six words, a step of a job, a named exception, or a count list.
func TestResultBadgesUseOneWordPerState(t *testing.T) {
	now := time.Now()
	job := func(id model.JobID, batch model.BatchID, st model.JobState) model.Job {
		return model.Job{ID: id, BatchID: batch, DeviceID: 1, State: st, TargetRelease: "16.1.0", CreatedBy: "adam", CreatedAt: now, UpdatedAt: now}
	}
	jobs := []model.Job{ // newest first
		job(12, 6, model.JobQueued), job(11, 6, model.JobAwaitReboot),
		job(10, 5, model.JobSucceeded), job(9, 5, model.JobFailed), // mixed, finished
		job(8, 4, model.JobSucceeded), job(7, 4, model.JobSucceeded), // all succeeded
		job(6, 3, model.JobFailed),    // all failed
		job(5, 2, model.JobCancelled), // all cancelled
		job(4, 1, model.JobSucceeded),
	}
	e := setupWith(t, func(d *Deps) { d.Jobs, d.OTA = &fakeJobs{jobs: jobs}, &fakeOTA{} })

	_, list := e.do(t, "GET", "/transactions", "operator", nil)
	requireResultWords(t, "transactions list", between(list, `<main id="main" tabindex="-1">`, "</main>"))
	for batch := range 6 {
		_, page := e.do(t, "GET", "/transactions/"+string(rune('1'+batch)), "operator", nil)
		// the per-device row of a job is the one place where "Done" ends a step of that job
		main := between(page, `<main id="main" tabindex="-1">`, "</main>")
		requireResultWords(t, "transaction "+string(rune('1'+batch)), main, "Done")
		// the progress line and the cancel text name the state by the same word as its badge
		if strings.Contains(strings.ToLower(main), "queued") {
			t.Errorf("transaction %d: the page says queued where its badge says Waiting", batch+1)
		}
	}

	_, dash := e.do(t, "GET", "/", "operator", nil)
	requireResultWords(t, "dashboard", between(dash, `<h2>Activity</h2>`, `</section>`))

	_, dev := e.do(t, "GET", "/devices/1", "operator", nil)
	requireResultWords(t, "device activity", between(dev, `<ul class="dev-act">`, `</ul>`))

	for i, r := range []string{"ok", "failed: timeout", "refused: not allowed", "role changed", ""} {
		e.aud.events = append(e.aud.events, model.AuditEvent{ID: int64(i), At: now.Add(-time.Duration(i) * time.Minute), Actor: "adam",
			Action: "device.update", Target: "device:1", Result: r, Detail: `{}`})
	}
	_, audit := e.do(t, "GET", "/audit?since=all", "admin", nil)
	var results []string
	for _, m := range regexp.MustCompile(`<td data-label="Result"[^>]*>(.*?)</td>`).FindAllStringSubmatch(audit, -1) {
		results = append(results, m[1])
	}
	requireResultWords(t, "audit log", strings.Join(results, "\n"))

	body := userDetailBody(t, auth.UserDetailData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin",
		User:   auth.UserRow{Subject: "local:ops", Name: "ops", Method: auth.MethodLocal, Role: "operator"},
		Guards: allowed,
		Audit: []model.AuditEvent{
			{ID: 1, At: now, Actor: "ops", Action: "device.update", Target: "device:1", Result: "ok"},
			{ID: 2, At: now, Actor: "ops", Action: "device.update", Target: "device:1", Result: "refused: not allowed"},
			{ID: 3, At: now, Actor: "ops", Action: "device.update", Target: "device:1", Result: "failed: timeout"},
		}})
	requireResultWords(t, "user detail", between(body, `id="ud-activity-h"`, "</section>"))
}

// Why (#1105): the same spelling rule as the design system's Wording table, enforced where the labels are
// written. A badge that starts lower case ("cached", "failed") is a raw enum value on its way to the page;
// the next state added to that enum would show up unworded. Badges are sentence case.
func TestBadgeLabelsAreSentenceCase(t *testing.T) {
	// ui.Badge(tone, "label"), ui.BadgeMark(tone, "mark", "label") and Label: "label" of a BadgeData.
	callRe := regexp.MustCompile(`ui\.Badge\([^,()]+,\s*"([^"]*)"\)`)
	markRe := regexp.MustCompile(`ui\.BadgeMark\([^,()]+,\s*"[^"]*",\s*"([^"]*)"\)`)
	dataRe := regexp.MustCompile(`BadgeData\{[^}]*\bLabel:\s*"([^"]*)"`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	more, err := filepath.Glob("*.templ")
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, f := range append(files, more...) {
		if strings.HasSuffix(f, "_test.go") || strings.HasSuffix(f, "_templ.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range []*regexp.Regexp{callRe, markRe, dataRe} {
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				if l := m[1]; l != "" && l[0] >= 'a' && l[0] <= 'z' {
					bad = append(bad, f+": "+l)
				}
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("badge labels that start lower case (write them in sentence case, or word them through label()):\n%s", strings.Join(bad, "\n"))
	}
}

// Why (#1105): the labels map is the one place that words a job state, and the counts of a mixed result
// are built from it. A state that waits for a free slot is Waiting (the design system's word), not the
// stored name Queued; counts leave out what no device did and keep the order succeeded, failed, cancelled.
func TestLabelsAndCountsSpeakTheSixWords(t *testing.T) {
	for state, want := range map[model.JobState]string{
		model.JobQueued: "Waiting", model.JobSucceeded: "Succeeded", model.JobFailed: "Failed", model.JobCancelled: "Cancelled",
	} {
		if got := label(state); got != want {
			t.Errorf("label(%s) = %q, want %q", state, got, want)
		}
	}
	for _, c := range []struct {
		ok, failed, cancelled int
		want                  string
	}{
		{30, 1, 0, "30 succeeded · 1 failed"},
		{0, 2, 1, "2 failed · 1 cancelled"},
		{3, 0, 0, "3 succeeded"},
		{0, 0, 0, ""},
	} {
		if got := resultCounts(c.ok, c.failed, c.cancelled); got != c.want {
			t.Errorf("resultCounts(%d, %d, %d) = %q, want %q", c.ok, c.failed, c.cancelled, got, c.want)
		}
	}
}

// Why (#1105): the header of a bulk result says what the devices did, in the result words. A device whose
// job was cancelled did not fail; counting it as failed made "2 succeeded · 1 cancelled" read "2 succeeded
// · 1 failed" while its own row said Cancelled, and an all-cancelled run read Failed.
func TestBulkResultHeaderNamesCancelledApartFromFailed(t *testing.T) {
	for _, c := range []struct {
		v    bulkResultView
		want string
	}{
		{bulkResultView{OK: 2, Failed: 1, Cancelled: 1}, "2 succeeded · 1 cancelled"},
		{bulkResultView{OK: 2, Failed: 3, Cancelled: 1}, "2 succeeded · 2 failed · 1 cancelled"},
		{bulkResultView{Failed: 2, Cancelled: 2}, "Cancelled"},
		{bulkResultView{Failed: 2, Cancelled: 1}, "1 failed · 1 cancelled"},
		{bulkResultView{Failed: 2}, "Failed"},
		{bulkResultView{OK: 3}, "Succeeded"},
	} {
		if _, got := c.v.State(); got != c.want {
			t.Errorf("State() of %+v = %q, want %q", c.v, got, c.want)
		}
	}
}
