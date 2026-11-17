package web

import (
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
)

func TestRunDetailSaysTheFilesReasonOnce(t *testing.T) {
	// Why (#324): the run's dump note ends with "files not backed up: ..." as data; the row prints
	// that reason in words, once, not the raw sentence next to its own wording.
	r := backupRun{Backup: model.Backup{Version: "16.0.0", Size: 10, Note: "Nightly; " + backup.NoteFilesPrefix + "no filesystem"}}
	got := runDetail(r)
	if strings.Count(got, "settings only: this build has no filesystem") != 1 || strings.Contains(got, "files not backed up") || !strings.Contains(got, "Nightly") {
		t.Errorf("detail %q", got)
	}
}

// Why (#887): a failed attempt shows the device's free heap and uptime read after the failure, so a repeat of
// the failure can be judged from its own row (a low heap, a long uptime: a restart helps). The heap is labelled
// as read afterwards: the transfer had freed its buffer by then. An attempt without them reads as before.
func TestAttemptDetailShowsTheEvidenceOfTheFailure(t *testing.T) {
	a := model.BackupAttempt{Trigger: model.TriggerScheduled, Version: "16.0.0", HeapKB: 17, UptimeSec: 10*24*3600 + 3*3600}
	got := attemptDetail(a)
	if !strings.Contains(got, "free heap afterwards 17 kB") || !strings.Contains(got, "up 10d 3h") || !strings.Contains(got, "Tasmota 16.0.0") {
		t.Errorf("detail %q", got)
	}
	a.HeapKB, a.UptimeSec = 0, 0
	if got := attemptDetail(a); strings.Contains(got, "heap") || strings.Contains(got, "up ") || !strings.HasSuffix(got, "Tasmota 16.0.0") {
		t.Errorf("without evidence: %q", got)
	}
}
