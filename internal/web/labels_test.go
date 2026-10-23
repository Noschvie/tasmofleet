package web

import (
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
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
	if got := tone("some_new-state"); got != "neutral" {
		t.Fatalf("tone %q", got)
	}
}

// Why: every OTA job state is shown in the jobs table; none may fall through
// to the raw name.
func TestEveryJobStateHasLabelAndTone(t *testing.T) {
	for _, s := range []model.JobState{model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL,
		model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobRestorePower,
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

// Why: a device that is downloading firmware does not answer; the log must say
// so in words, not show "timeout: no answer" as if something broke.
func TestEventTextDoesNotShowExpectedSilenceAsError(t *testing.T) {
	e := model.JobEvent{Event: "probe_offline", Detail: "status 2: tasmota: timeout: no answer from 10.0.0.5", To: model.JobUpgradeIssued}
	if got := eventText(e); got != "Downloading firmware..." {
		t.Fatalf("got %q", got)
	}
	e.To = model.JobAwaitReboot
	if got := eventText(e); got != "Rebooting..." {
		t.Fatalf("got %q", got)
	}
	f := model.JobEvent{Event: "failed", Detail: "wrong chip family"}
	if got := eventText(f); got != "Failed wrong chip family" {
		t.Fatalf("real failures keep their detail: %q", got)
	}
}

// Why: a transaction is not atomic and nothing is rolled back, so its outcome
// must say how many devices worked instead of one pass/fail word.
func TestTransactionOutcome(t *testing.T) {
	for _, tc := range []struct {
		total, done, running, queued, failed, canceled int
		want, tone                                     string
	}{
		{3, 3, 0, 0, 0, 0, "Completed", "ok"},
		{3, 2, 0, 0, 1, 0, "Partially completed (2 of 3)", "warn"},
		{3, 1, 1, 1, 0, 0, "Running", "info"},
		{3, 0, 0, 0, 3, 0, "Failed", "bad"},
		{2, 0, 0, 0, 1, 1, "Failed", "bad"},
		{2, 0, 0, 0, 0, 2, "Cancelled", "neutral"},
		{2, 1, 0, 0, 0, 1, "Partially completed (1 of 2)", "warn"},
	} {
		got, tone := txOutcome(tc.total, tc.done, tc.running, tc.queued, tc.failed, tc.canceled)
		if got != tc.want || tone != tc.tone {
			t.Errorf("%+v: got %q/%q", tc, got, tone)
		}
	}
}
