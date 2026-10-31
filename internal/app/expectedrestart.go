package app

import (
	"context"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// expectedGrace is how long after its last change a job of TasmoFleet still explains a restart: the
// device reboots after the job's last step, and its INFO3 comes when it is back on the broker.
const expectedGrace = 3 * time.Minute

// restartAsks remembers when TasmoFleet last sent a device a command that restarts it (the Restart
// button, a bulk restart, a typed Restart 1 or Upgrade): the restart that follows is its own doing.
type restartAsks struct {
	mu  sync.Mutex
	at  map[model.DeviceID]time.Time
	now func() time.Time
}

func newRestartAsks(now func() time.Time) *restartAsks {
	return &restartAsks{at: map[model.DeviceID]time.Time{}, now: now}
}

// Note records a restart request now.
func (r *restartAsks) Note(id model.DeviceID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.at[id] = r.now()
}

// Recent says whether a request was made within expectedGrace.
func (r *restartAsks) Recent(id model.DeviceID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.at[id]
	return ok && r.now().Sub(t) <= expectedGrace
}

// expectedRestart says whether TasmoFleet is working on the device (an update, a restore, a command or
// a policy fix that reboots it), so that a restart now is its own doing and no sign of a boot loop.
func (a *runtime) expectedRestart(id model.DeviceID) bool {
	if a.restartAsks != nil && a.restartAsks.Recent(id) {
		return true
	}
	ctx, cancel := context.WithTimeout(a.bgContext, 5*time.Second)
	defer cancel()
	jobs, err := a.svc.Store.ListJobs(ctx, store.JobFilter{DeviceID: id, Limit: 5})
	if err != nil {
		return false // unknown: the restart counts, which is the safe side for a boot loop warning
	}
	return jobsExplainRestart(jobs, time.Now())
}

// jobsExplainRestart: a job of the device that runs, or changed within expectedGrace. A backup does not
// restart the device and explains nothing.
func jobsExplainRestart(jobs []model.Job, now time.Time) bool {
	for _, j := range jobs {
		if j.IsBackup() {
			continue
		}
		if !j.State.Terminal() || now.Sub(j.UpdatedAt) <= expectedGrace {
			return true
		}
	}
	return false
}
