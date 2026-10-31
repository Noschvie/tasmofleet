package app

import (
	"context"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// expectedGrace is how long after its last change a job of TasmoFleet still explains a restart: the
// device reboots after the job's last step, and its INFO3 comes when it is back on the broker.
const expectedGrace = 3 * time.Minute

// expectedRestart says whether TasmoFleet is working on the device (an update, a restore, a command or
// a policy fix that reboots it), so that a restart now is its own doing and no sign of a boot loop.
func (a *runtime) expectedRestart(id model.DeviceID) bool {
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
