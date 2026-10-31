package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
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

// Why: the Restart button, a bulk restart and a typed Restart 1 all go through the console service;
// the restart that follows must count as TasmoFleet's own, for 3 minutes and for that device only.
func TestRestartRequestedByTasmoFleetIsExpectedForThreeMinutes(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	asks := newRestartAsks(func() time.Time { return now })
	if asks.Recent(1) {
		t.Fatal("a device nobody restarted is expected to restart")
	}
	asks.Note(1)
	now = now.Add(expectedGrace)
	if !asks.Recent(1) || asks.Recent(2) {
		t.Fatalf("at the edge: device 1 %v (want true), device 2 %v (want false)", asks.Recent(1), asks.Recent(2))
	}
	now = now.Add(time.Second)
	if asks.Recent(1) {
		t.Fatal("a restart long after the request is not expected")
	}
}

type fakeCmdService struct {
	devicecmd.Service
	err error
}

func (f fakeCmdService) Run(context.Context, model.Principal, model.DeviceID, string) (json.RawMessage, error) {
	return nil, f.err
}

// Why: only a command that reached (or may have reached) the device and restarts it explains a restart:
// a refused command, a Power switch or Restart 2 (halt) must not hide a real boot loop.
func TestHoldingCmdNotesOnlyRestartsThatWereSent(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  string
		err  error
		want bool
	}{
		{"restart button", "Restart 1", nil, true},
		{"restart in a backlog", "Backlog Delay 5; Restart 1", nil, true},
		{"reply lost, may have reached it", "Restart 1", errors.New("timeout"), true},
		{"refused by the policy", "Restart 1", cmdpolicy.ErrForbidden, false},
		{"device busy", "Restart 1", devlock.BusyError{Holder: "ota"}, false},
		{"power switch", "Power1 ON", nil, false},
		{"restart 2 halts", "Restart 2", nil, false},
	} {
		asks := newRestartAsks(time.Now)
		h := holdingCmd{Service: fakeCmdService{err: c.err}, holds: devlock.NewFirmwareHolds(time.Now, time.Minute), asks: asks}
		_, _ = h.Run(context.Background(), model.Principal{}, 7, c.cmd)
		if got := asks.Recent(7); got != c.want {
			t.Errorf("%s: noted %v, want %v", c.name, got, c.want)
		}
	}
}
