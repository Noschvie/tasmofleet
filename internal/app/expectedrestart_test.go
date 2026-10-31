package app

import (
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: an update restarts the device twice and a retry again; if those count towards the boot loop
// limit, every OTA of a device that needs the minimal image would show a boot loop. A job that runs,
// or ended a moment ago, explains the restart; an old one or a backup does not.
func TestJobsExplainRestartOnlyWhileTheyRunOrJustEnded(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	job := func(state model.JobState, ago time.Duration) model.Job {
		return model.Job{State: state, UpdatedAt: now.Add(-ago)}
	}
	backup := job(model.JobSucceeded, time.Second)
	backup.Kind = model.KindBackup
	for _, c := range []struct {
		name string
		jobs []model.Job
		want bool
	}{
		{"no jobs", nil, false},
		{"a running update", []model.Job{job(model.JobAwaitReboot, time.Hour)}, true},
		{"an update that ended a minute ago", []model.Job{job(model.JobSucceeded, time.Minute)}, true},
		{"the retry after a failed first try", []model.Job{job(model.JobFailed, 2*time.Minute), job(model.JobFailed, 20*time.Minute)}, true},
		{"an update that ended an hour ago", []model.Job{job(model.JobSucceeded, time.Hour)}, false},
		{"a backup that just ended", []model.Job{backup}, false},
	} {
		if got := jobsExplainRestart(c.jobs, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
