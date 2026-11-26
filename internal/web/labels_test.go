package web

import (
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// Why: the UI must never show raw enum names such as "auth_failed" (a
// reported usability problem), and a status must never be colour only: every
// state has a human label and a tone.
func TestLabelsAreHumanAndEveryHealthStateHasATone(t *testing.T) {
	for state, want := range map[model.HealthState]string{
		model.HealthOnline: "Online", model.HealthStale: "Stale", model.HealthOffline: "Offline",
		model.HealthAuthFailed: "Auth failed", model.HealthUnknown: "Unknown",
	} {
		if got := label(state); got != want {
			t.Errorf("label(%q) = %q, want %q", state, got, want)
		}
		if _, ok := tones[string(state)]; !ok {
			t.Errorf("health state %q has no tone", state)
		}
	}
}

// Why: a job state added later must still read as words before anyone adds it
// to the label map.
func TestLabelFallbackHumanisesUnknownValues(t *testing.T) {
	if got := label("some_new-state"); got != "Some new state" {
		t.Fatalf("got %q", got)
	}
	if got := tone("some_new-state"); got != ui.ToneNeutral {
		t.Fatalf("tone %q", got)
	}
}

// Why: every OTA job state is shown in the jobs table; none may fall through
// to the raw name.
func TestEveryJobStateHasLabelAndTone(t *testing.T) {
	for _, s := range []model.JobState{model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL,
		model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL,
		model.JobSucceeded, model.JobFailed, model.JobCancelled} {
		if _, ok := labels[string(s)]; !ok {
			t.Errorf("job state %q has no label", s)
		}
		if _, ok := tones[string(s)]; !ok {
			t.Errorf("job state %q has no tone", s)
		}
	}
}

// Why: a running update must never make the progress bar jump backwards, in
// the staged ESP8266 flow (minimal image first) or the direct flow.
func TestJobStepNeverGoesBackwards(t *testing.T) {
	staged := []struct {
		s  model.JobState
		st model.Stage
	}{
		{model.JobPreflight, model.StageFinal}, {model.JobBackup, model.StageFinal},
		{model.JobSetOtaURL, model.StageMinimal}, {model.JobUpgradeIssued, model.StageMinimal}, {model.JobAwaitReboot, model.StageMinimal},
		{model.JobSetOtaURL, model.StageFinal}, {model.JobUpgradeIssued, model.StageFinal}, {model.JobAwaitReboot, model.StageFinal},
		{model.JobVerify, model.StageFinal}, {model.JobRestoreOtaURL, model.StageFinal}, {model.JobSucceeded, model.StageFinal},
	}
	prev := -1
	for _, x := range staged {
		n, text := jobStep(x.s, x.st)
		if n < prev || text == "" {
			t.Fatalf("%s/%s: step %d after %d (%q)", x.s, x.st, n, prev, text)
		}
		prev = n
	}
	if prev != jobSteps {
		t.Fatalf("done is step %d, want %d", prev, jobSteps)
	}
}

// Why (#603, #604): a job that waits for its start time has not started; reading "Queued" or
// "Waiting for a free slot" would say it is about to run now, and any progress would say it touched the
// device. A job whose start passed while TasmoFleet was down was not cancelled by anyone and nothing was
// attempted: it reads "Not started" in the warn tone, not "Cancelled" and not "Failed".
func TestScheduledAndNotStartedJobsReadAsSuch(t *testing.T) {
	if n, text := jobStep(model.JobScheduled, model.StageFinal); n != 0 || text != "Scheduled" {
		t.Errorf("scheduled job on the progress bar: step %d %q, want 0 \"Scheduled\"", n, text)
	}
	for _, c := range []struct {
		j    model.Job
		word string
		tone ui.Tone
	}{
		{model.Job{State: model.JobScheduled}, "Planned", ui.ToneNeutral},
		{model.Job{State: model.JobCancelled, NotStarted: true}, "Not started", ui.ToneWarn},
		{model.Job{State: model.JobCancelled}, "Cancelled", ui.ToneNeutral},
	} {
		if w, tn := jobBadge(c.j); w != c.word || tn != c.tone {
			t.Errorf("%s (not started %v): badge %q %s, want %q %s", c.j.State, c.j.NotStarted, w, tn, c.word, c.tone)
		}
	}
}

// Why: a device that is downloading firmware does not answer; the log must say
// so in words, not show "timeout: no answer" as if something broke.
func TestEventTextDoesNotShowExpectedSilenceAsError(t *testing.T) {
	e := model.JobEvent{Event: "probe_offline", Detail: "status 2: tasmota: timeout: no answer from 10.0.0.5", To: model.JobUpgradeIssued}
	if got := eventText(e, model.Job{}, newPrincipalDir(nil), false); got != "Downloading firmware..." {
		t.Fatalf("got %q", got)
	}
	e.To = model.JobAwaitReboot
	if got := eventText(e, model.Job{}, newPrincipalDir(nil), false); got != "Rebooting..." {
		t.Fatalf("got %q", got)
	}
	f := model.JobEvent{Event: "failed", Detail: "wrong chip family"}
	if got := eventText(f, model.Job{}, newPrincipalDir(nil), false); got != "Failed wrong chip family" {
		t.Fatalf("real failures keep their detail: %q", got)
	}
}

// Why: a transaction is not atomic and nothing is rolled back, so its outcome
// must say how many devices worked instead of one pass/fail word.
func TestTransactionOutcome(t *testing.T) {
	for _, tc := range []struct {
		total, done, running, queued, failed, canceled int
		want                                           string
		tone                                           ui.Tone
	}{
		{3, 3, 0, 0, 0, 0, "Succeeded", ui.ToneOK},
		{3, 2, 0, 0, 1, 0, "Succeeded on 2 of 3", ui.ToneWarn},
		{3, 1, 1, 1, 0, 0, "Running", ui.ToneAccent},
		{3, 0, 0, 0, 3, 0, "Failed", ui.ToneDanger},
		{2, 0, 0, 0, 1, 1, "Failed", ui.ToneDanger},
		{2, 0, 0, 0, 0, 2, "Cancelled", ui.ToneNeutral},
		{2, 1, 0, 0, 0, 1, "Succeeded on 1 of 2", ui.ToneWarn},
	} {
		got, tone := txOutcome(tc.total, tc.done, tc.running, tc.queued, tc.failed, tc.canceled)
		if got != tc.want || tone != tc.tone {
			t.Errorf("%+v: got %q/%q", tc, got, tone)
		}
	}
}

// Why: Tone.class() draws a tone it does not know as neutral, silently, so a name that is not one of the design
// system's five ("bad" and "info" were the old ones) would turn a failure grey with no error anywhere. Every value of
// the tone map is one of the five; the check is on the set, not on the type, because ui.Tone("bad") compiles.
func TestBadgeTonesAreDesignSystemTones(t *testing.T) {
	five := map[ui.Tone]bool{ui.ToneOK: true, ui.ToneWarn: true, ui.ToneDanger: true, ui.ToneNeutral: true, ui.ToneAccent: true}
	for state, tn := range tones {
		if !five[tn] {
			t.Errorf("tones[%q] = %q, which is not ok, warn, danger, neutral or accent", state, tn)
		}
	}
	// a failure is the danger tone and a state in progress the accent tone, wherever they are looked up
	if got := tone("failed"); got != ui.ToneDanger {
		t.Errorf("tone(failed) = %q, want danger", got)
	}
	if got := tone("sending"); got != ui.ToneAccent {
		t.Errorf("tone(sending) = %q, want accent", got)
	}
	// the status badge keeps a state in progress neutral (it always did); a failure stays danger
	if got := stateTone("sending"); got != ui.ToneNeutral {
		t.Errorf("stateTone(sending) = %q, want neutral", got)
	}
	if got := stateTone("offline"); got != ui.ToneDanger {
		t.Errorf("stateTone(offline) = %q, want danger", got)
	}
}
