package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/notify"
	"github.com/hellwand/tasmofleet/internal/schedule"
)

// backupGrace is how late a scheduled run still starts: a run whose time passed while TasmoFleet
// was down starts when it is back if that is within this long after the scheduled time, else it is
// skipped and the person is told (#601).
const backupGrace = notify.MissedGrace

// backupTimes is what the schedule reads and writes of the run record (*store.SQLite).
type backupTimes interface {
	BackupRunTimes(ctx context.Context) (settled, missed time.Time, err error)
	SaveBackupRun(ctx context.Context, r model.BackupRun) (int64, error)
}

// scheduleLoop runs a timed job on the weekdays and the time of day of the settings: the fleet backup
// (#601) and the database snapshot (#602, over snapshotRuns). name says which in the log.
//
// It never starts a run because it started or because the settings were saved: it asks
// schedule.Plan what is due, given what the database says settled the latest scheduled time. A run
// settles it, so does a skip, and so does a change of the schedule (reschedule): saving the page at
// 10:20 with a time of 10:00 must not back up the fleet. A restart settles nothing, which is the
// point: a run that was due while TasmoFleet was down still starts within the grace.
type scheduleLoop struct {
	name     string
	log      *slog.Logger
	schedule func() schedule.Schedule
	store    backupTimes
	now      func() time.Time
	// run is the job (scheduledBackup.Run, scheduledSnapshot).
	run func(ctx context.Context)
	// missed tells the person that the run of slot was skipped (nil: nobody is told).
	missed func(slot, at time.Time)
	// floor is the newest settled time this process knows: what it settled itself and what it read
	// from the database. The database is the memory across restarts; the floor keeps a failed write
	// from starting the same run again at once, and keeps a row the audit retention pruned between
	// two scheduled days from reading as a schedule that never ran.
	floor time.Time
}

// step looks at the schedule once and does what is due. It returns when to look again (zero:
// only after a change of the settings, because nothing is scheduled).
func (b *scheduleLoop) step(ctx context.Context) time.Time {
	s := b.schedule()
	if s.Off() {
		return time.Time{}
	}
	now := b.now()
	settled, _, err := b.store.BackupRunTimes(ctx)
	if err != nil {
		// Without the history a run could be repeated, so nothing runs; try again shortly.
		b.log.Warn(b.name+" schedule: read the run record", "err", err)
		return now.Add(time.Minute)
	}
	if b.floor.After(settled) {
		settled = b.floor
	} else {
		b.floor = settled // the retention may prune that row before the next scheduled time
	}
	if settled.IsZero() {
		// Neither the record nor this process knows a run, skip or schedule change: nothing says
		// this schedule ever existed (it came from the environment, or the record was pruned while
		// TasmoFleet was down). It begins now; it does not make up a time of day that passed before
		// anybody could know.
		b.reschedule(ctx)
		return s.Next(now)
	}
	d := schedule.Plan(s, now, settled, backupGrace)
	switch d.Action {
	case schedule.Wait:
	case schedule.Run:
		b.floor = now
		if late := now.Sub(d.Slot); late > time.Minute {
			b.log.Info(b.name+" schedule: starting a run late", "scheduled", d.Slot, "late", late.Round(time.Second))
		}
		b.run(ctx)
	case schedule.Missed:
		b.floor = now
		b.log.Warn(b.name+" schedule: a scheduled run was missed and is skipped", "scheduled", d.Slot, "now", now)
		b.mark(ctx, model.BackupRunMissed, now)
		if b.missed != nil {
			b.missed(d.Slot, now)
		}
	}
	if d.Action != schedule.Wait {
		return s.Next(b.now()) // the run took time; look at the schedule fresh
	}
	return d.Next
}

// reschedule records that the schedule was changed: every time of day before now is settled.
func (b *scheduleLoop) reschedule(ctx context.Context) {
	now := b.now()
	b.floor = now
	b.mark(ctx, model.BackupRunRescheduled, now)
}

// mark stores a marker row (no devices) that settles the scheduled times up to at.
func (b *scheduleLoop) mark(ctx context.Context, trigger model.BackupRunTrigger, at time.Time) {
	// Writing the marker is not cut by a shutdown: it is one row.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := b.store.SaveBackupRun(sctx, model.BackupRun{Trigger: trigger, StartedAt: at, FinishedAt: at, Completed: true}); err != nil {
		b.log.Warn(b.name+" schedule: storing the marker", "marker", string(trigger), "err", err)
	}
}

// loop runs until ctx ends: look, do what is due, sleep until the next scheduled time or until the
// settings change.
func (b *scheduleLoop) loop(ctx context.Context, changed <-chan struct{}) {
	for {
		next := b.step(ctx)
		var timer *time.Timer
		var wake <-chan time.Time
		if !next.IsZero() {
			// At least a second, so a clock that is a hair behind the timer cannot spin.
			timer = time.NewTimer(max(next.Sub(b.now()), time.Second))
			wake = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-changed:
			b.reschedule(ctx)
		case <-wake:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}
