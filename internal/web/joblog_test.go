package web

import (
	"context"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

type logEv struct {
	from, to model.JobState
	event    string
	detail   string
	want     string
}

func runLog(t *testing.T, j model.Job, pr *principalDir, seq []logEv) {
	t.Helper()
	evs := make([]model.JobEvent, len(seq))
	for i, s := range seq {
		evs[i] = model.JobEvent{From: s.from, To: s.to, Event: s.event, Detail: s.detail}
	}
	for i, s := range seq {
		if got := eventText(evs[i], j, pr, eventStageMinimal(evs, i, j)); got != s.want {
			t.Errorf("#%d %s %s->%s: %q, want %q", i, s.event, s.from, s.to, got, s.want)
		}
	}
}

// Why: job 8 (an ESP32 with the safeboot layout, updated to 16.0.0) read like a failed
// run: a timeout during flashing was shown as "Command failed", the cancel line showed a
// 64-character id, and the success was worded "Identity". The log of a finished job must
// show what happened to the device, in words and names people use.
func TestJobLogOfADirectSafebootUpdateReadsAsItHappened(t *testing.T) {
	st := &memPrincipals{}
	if err := st.UpsertPrincipal(context.Background(), store.PrincipalRecord{Subject: testSub, DisplayName: "Thomas", Kind: "sso"}); err != nil {
		t.Fatal(err)
	}
	const timeout = "otaurl: tasmota: timeout: no answer from office-monitor-thomas.home.internal"
	const offline = "status 2: tasmota: timeout: no answer from office-monitor-thomas.home.internal"
	j := model.Job{TargetRelease: "16.0.0", Plan: model.PlanDirect, CancelRequested: true, State: model.JobSucceeded}
	const q, pf, bk, so, ui, ar, vf, ok = model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL, model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobSucceeded
	runLog(t, j, newPrincipalDir(st), []logEv{
		{q, pf, "start", "", "Start"},
		{pf, pf, "identity", "", "Identity"}, // preflight identity stays
		{pf, bk, "planned", "", "Planned"},
		{bk, so, "backup_done", "", "Backup done"},
		{so, ui, "command_ok", "", "Command ok"},
		{ui, ar, "probe_offline", offline, "Rebooting into 16.0.0..."},
		{ar, vf, "probe_online", "", "Waiting for 16.0.0 to answer..."},
		// a timeout while the device flashes is not a failure
		{vf, ar, "command_failed", timeout, "Device busy, still flashing..."},
		{ar, vf, "probe_online", "", "Waiting for 16.0.0 to answer..."},
		// people need names, not ids
		{vf, vf, "cancel", "cancelled by " + testSub, "Cancel requested by Thomas"},
		{vf, ar, "identity", "", "Not on the new version yet"},
		{ar, ar, "probe_offline", offline, "Rebooting into 16.0.0..."},
		{ar, vf, "probe_online", "", "Waiting for 16.0.0 to answer..."},
		// a finished job must show its outcome
		{vf, ok, "identity", "", "Updated to 16.0.0. Cancel came too late: the update had already been written"},
	})
}

// Why: on an ESP8266 with 1 MB the log runs through two images; "Rebooting..." twice does not
// say which one the update waits for, and the bare "Identity" hid both the stage change and the
// success. The stage is not stored on events, so it is replayed from the log.
func TestJobLogOfAStagedUpdateNamesTheStage(t *testing.T) {
	j := model.Job{TargetRelease: "16.0.0", Plan: model.PlanStaged, State: model.JobSucceeded}
	const q, pf, bk, so, ui, ar, vf, ro, ok = model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL, model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobSucceeded
	runLog(t, j, newPrincipalDir(nil), []logEv{
		{q, pf, "start", "", "Start"},
		{pf, pf, "identity", "", "Identity"},
		{pf, bk, "planned", "", "Planned"},
		{bk, so, "backup_done", "", "Backup done"},
		{so, ui, "command_ok", "", "Command ok"},
		{ui, ar, "probe_offline", "", "Rebooting into the minimal firmware..."},
		{ar, vf, "probe_online", "", "Waiting for the minimal firmware to answer..."},
		{vf, so, "identity", "", "Minimal firmware installed, installing 16.0.0"},
		{so, ui, "command_ok", "", "Command ok"},
		{ui, ar, "probe_offline", "", "Rebooting into 16.0.0..."},
		{ar, vf, "probe_online", "", "Waiting for 16.0.0 to answer..."},
		{vf, ar, "identity", "", "Not on the new version yet"},
		{ar, ar, "probe_offline", "", "Rebooting into 16.0.0..."},
		{ar, vf, "probe_online", "", "Waiting for 16.0.0 to answer..."},
		{vf, ro, "identity", "", "New version verified, restoring the device's OtaUrl"},
		{ro, ok, "command_ok", "", "Updated to 16.0.0"},
	})
}

// Why: the download lines name the image, so a staged log says whether the small or the final
// image is coming.
func TestJobLogDownloadNamesTheImage(t *testing.T) {
	staged := model.Job{TargetRelease: "16.0.0", Plan: model.PlanStaged}
	direct := model.Job{TargetRelease: "16.0.0", Plan: model.PlanDirect}
	e := model.JobEvent{Event: "probe_offline", To: model.JobUpgradeIssued}
	for _, tc := range []struct {
		j       model.Job
		minimal bool
		want    string
	}{
		{staged, true, "Downloading the minimal firmware..."},
		{staged, false, "Downloading the final firmware (16.0.0)..."},
		{direct, false, "Downloading firmware 16.0.0..."},
	} {
		if got := eventText(e, tc.j, newPrincipalDir(nil), tc.minimal); got != tc.want {
			t.Errorf("%q, want %q", got, tc.want)
		}
	}
}

// Why: a command that really failed (the job ends failed) must keep the engine's text.
func TestJobLogRealCommandFailureKeepsEngineText(t *testing.T) {
	e := model.JobEvent{From: model.JobSetOtaURL, To: model.JobFailed, Event: "command_failed", Detail: "otaurl: refused"}
	if got := eventText(e, model.Job{}, newPrincipalDir(nil), false); got != "Command failed otaurl: refused" {
		t.Errorf("got %q", got)
	}
	// and without a known release a finished job still says it worked
	done := model.JobEvent{From: model.JobVerify, To: model.JobSucceeded, Event: "identity"}
	if got := eventText(done, model.Job{}, newPrincipalDir(nil), false); got != "Update verified" {
		t.Errorf("got %q", got)
	}
}
