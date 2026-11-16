package app

import (
	"context"
	"time"

	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/notify"
	"github.com/hellwand/tasmofleet/internal/store"
)

// setupNotify builds the dispatcher of the notifications (#249). It reads the effective settings
// at the moment of each event, so a change on the Settings page applies at once. Web Push (#610) is on
// when the VAPID key could be loaded or made; without it the other channels still work.
func (a *runtime) setupNotify(ctx context.Context) {
	a.notifier = notify.New(notify.Options{Config: a.svc.Sys.Snapshot, Log: a.log, Browser: a.publishNotice, Push: a.setupPush(ctx)})
}

// setupPush loads (or makes) the VAPID key and returns the push channel; nil when the key cannot be
// had, with a warning. The owner of a subscription is checked with the same resolver that decides who
// a stored job creator is now (principalResolver), at the moment of every event.
func (a *runtime) setupPush(ctx context.Context) *notify.Push {
	key, err := notify.LoadVAPIDKey(ctx, a.svc.Store, a.log)
	if err != nil {
		a.log.Warn("web push is off: the VAPID key could not be loaded", "err", err)
		return nil
	}
	r := principalResolver{local: a.svc.Store, dir: a.svc.Store}
	p, err := notify.NewPush(a.svc.Store, key, r.isAdmin)
	if err != nil {
		a.log.Warn("web push is off", "err", err)
		return nil
	}
	a.push = p
	return p
}

// publishNotice hands an event the settings ask for to the open browser tabs (#540). The id comes
// from the dispatcher (Event.ID): equal in every tab, because the hub sends one event to every
// stream, and equal to the id of the event's Web Push (#610).
func (a *runtime) publishNotice(ev notify.Event) {
	if a.svc.Hub == nil {
		return
	}
	a.svc.Hub.Publish(events.Event{Topic: events.TopicNotice, Payload: events.Notice{
		ID: ev.ID, Name: ev.Name, Result: ev.Result, Title: ev.Title, Text: ev.Text, Path: ev.Path}})
}

// notifyBatchDone sends the "update finished" event when j was the last unfinished job of its
// transaction: one message per transaction, not per device. A transaction none of whose jobs started
// (TasmoFleet was down past the window of its scheduled start, #603) sends "scheduled update not started"
// instead. Policy, command and backup jobs are
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
	var ok, failed, cancelled, notStarted int
	target := ""
	for _, x := range jobs {
		if !x.State.Terminal() {
			return // still running or waiting: the last job to end sends it
		}
		if x.NotStarted {
			notStarted++
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
	if notStarted > 0 && ok == 0 && failed == 0 {
		// TasmoFleet was down at the planned start and too long (#603), and no job ran: every job ended
		// cancelled, some of them perhaps by the person before the start. Say so, counting only the devices
		// that were not started.
		a.notifier.Emit(notify.UpdateNotStarted(a.notifier.Config(), int64(j.BatchID), notStarted, target, j.StartAt, time.Now()))
		return
	}
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
