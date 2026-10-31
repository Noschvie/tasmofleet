package app

import (
	"context"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/notify"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// setupNotify builds the dispatcher of the notifications (#249). It reads the effective settings
// at the moment of each event, so a change on the Settings page applies at once.
func (a *runtime) setupNotify() {
	a.notifier = notify.New(notify.Options{Config: a.svc.Sys.Snapshot, Log: a.log})
}

// notifyBatchDone sends the "update finished" event when j was the last unfinished job of its
// transaction: one message per transaction, not per device. Policy, command and backup jobs are
// no update. The transaction number is remembered, so two jobs that end at the same moment (both
// see the batch finished) send one message.
func (a *runtime) notifyBatchDone(j model.Job) {
	if a.notifier == nil || j.Kind != "" || j.BatchID == 0 || !j.State.Terminal() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	jobs, err := a.svc.Store.ListJobs(ctx, store.JobFilter{BatchID: j.BatchID})
	if err != nil {
		a.log.Warn("notification: list the jobs of the transaction", "batch", j.BatchID, "err", err)
		return
	}
	var ok, failed, cancelled int
	target := ""
	for _, x := range jobs {
		if !x.State.Terminal() {
			return // still running or waiting: the last job to end sends it
		}
		if x.State == model.JobSucceeded {
			ok++
		}
		if x.State == model.JobFailed {
			failed++
		}
		if x.State == model.JobCancelled {
			cancelled++
		}
		if target == "" {
			target = x.TargetRelease
		} else if x.TargetRelease != "" && x.TargetRelease != target {
			target = "" // mixed targets: do not name one
		}
	}
	if _, dup := a.notifiedBatches.LoadOrStore(j.BatchID, struct{}{}); dup {
		return
	}
	// The number only has to outlive the moment two jobs of one transaction end together.
	time.AfterFunc(10*time.Minute, func() { a.notifiedBatches.Delete(j.BatchID) })
	a.notifier.Emit(notify.UpdateFinished(a.notifier.Config(), int64(j.BatchID), ok, failed, cancelled, target, time.Now()))
}

// drainNotifications gives the messages still on their way a few seconds at shutdown.
func (a *runtime) drainNotifications() {
	if a.notifier == nil {
		return
	}
	done := make(chan struct{})
	go func() { a.notifier.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		a.log.Warn("notifications still in flight at shutdown were dropped")
	}
}
