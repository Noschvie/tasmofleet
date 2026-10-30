package restore

import (
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/store"
)

// The four steps of a restore or replace as the progress page shows them (design v2 screen
// "restore"): back up the current state, upload the backup, restart and wait, verify.
const (
	StepBackup = 1 + iota
	StepUpload
	StepRestart
	StepVerify
)

// The failure texts that name the step they end. A failure text is stored as the job's
// Detail; the progress page reads the step back from it (FailedStep), so the run code uses
// these prefixes instead of free text.
const (
	failPreBackup        = "backup before the restore failed, nothing was changed: "
	failPreBackupReplace = "backup of the replacement device failed, nothing was changed: "
	failUnchanged        = "nothing was changed: "
	failUnreachable      = "device not reachable, nothing was changed: "
	failToken            = "token generation failed: "
	failWebGetConfig     = "WebGetConfig was not accepted: "
	failUpload           = "FileUpload was not accepted: "
	failAfterRestore     = "restored, but "
	failTakeOver         = "the replacement device was restored, but "
)

// Step is the step a job is at: the running step, or for a failed job the step that failed;
// a finished job is past the last step (StepVerify + 1).
func Step(j Job) int {
	switch j.State {
	case store.RestoreDone:
		return StepVerify + 1
	case store.RestoreFailed:
		return FailedStep(j.Detail)
	case store.RestoreSending:
		return StepUpload
	case store.RestoreWaiting:
		return StepRestart
	case store.RestoreVerifying:
		return StepVerify
	}
	return StepBackup // pending and backup
}

// FailedStep is the step a failure text (Detail of a failed job) belongs to. A text it does
// not know is the wait for the restart: waitReboot's errors are the only unprefixed ones.
func FailedStep(detail string) int {
	switch {
	case strings.HasPrefix(detail, failPreBackup), strings.HasPrefix(detail, failPreBackupReplace):
		return StepBackup
	case strings.HasPrefix(detail, failAfterRestore), strings.HasPrefix(detail, failTakeOver):
		return StepVerify
	case strings.Contains(detail, failUnchanged), strings.HasPrefix(detail, failToken), strings.HasPrefix(detail, failWebGetConfig), strings.HasPrefix(detail, failUpload):
		return StepUpload
	}
	return StepRestart
}
