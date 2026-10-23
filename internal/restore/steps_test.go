package restore

import (
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Why: the progress page ticks the steps a job has passed and marks the one it is at or failed
// at; a wrong step would tell the user the device was changed when nothing was (or the
// reverse). The failure texts are the ones the run code writes (see the flow tests, which
// check Step on real failed jobs).
func TestStepOfRunningDoneAndFailedJobs(t *testing.T) {
	for _, c := range []struct {
		state, detail string
		want          int
	}{
		{store.RestorePending, "", StepBackup},
		{store.RestoreBackup, "taking a backup", StepBackup},
		{store.RestoreSending, "", StepUpload},
		{store.RestoreWaiting, "", StepRestart},
		{store.RestoreVerifying, "", StepVerify},
		{store.RestoreDone, "restored", StepVerify + 1},
		{store.RestoreFailed, failPreBackup + "timeout", StepBackup},
		{store.RestoreFailed, failPreBackupReplace + "timeout", StepBackup},
		{store.RestoreFailed, "replacement " + failUnreachable + "x", StepUpload},
		{store.RestoreFailed, failToken + "x", StepUpload},
		{store.RestoreFailed, "the device downloaded the configuration but did not come back in time", StepRestart},
		{store.RestoreFailed, failAfterRestore + "verification failed: x", StepVerify},
		{store.RestoreFailed, failTakeOver + "the inventory entry could not take it over", StepVerify},
	} {
		if got := Step(Job{State: c.state, Detail: c.detail}); got != c.want {
			t.Errorf("%s %q: step %d, want %d", c.state, c.detail, got, c.want)
		}
	}
}
