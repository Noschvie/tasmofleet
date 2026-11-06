package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/notify"
	"github.com/nofuturekid/tasmofleet/internal/schedule"
	"github.com/nofuturekid/tasmofleet/internal/snapshot"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// snapshotRuns adapts the snapshot run record (snapshot_runs, migration 0046) to the record the
// schedule loop reads, so a snapshot schedule is the loop of the fleet backup and not a second copy.
type snapshotRuns struct{ st *store.SQLite }

func (r snapshotRuns) BackupRunTimes(ctx context.Context) (settled, missed time.Time, err error) {
	return r.st.SnapshotRunTimes(ctx)
}

// SaveBackupRun stores one of the loop's markers; the loop writes nothing else.
func (r snapshotRuns) SaveBackupRun(ctx context.Context, run model.BackupRun) (int64, error) {
	trigger := store.SnapshotRunRescheduled
	if run.Trigger == model.BackupRunMissed {
		trigger = store.SnapshotRunMissed
	}
	return 0, r.st.SaveSnapshotRun(ctx, trigger, run.StartedAt)
}

// snapshotSchedule wires the schedule loop to the settings (SNAPSHOT_DAYS, SNAPSHOT_TIME), the run
// record and the notifier (#602). Like the fleet backup it never runs because TasmoFleet started or
// because the settings were saved; a run that was due while TasmoFleet was down starts within the
// grace, else it is skipped and the person is told.
func (a *runtime) snapshotSchedule() *scheduleLoop {
	b := &scheduleLoop{
		name: "snapshot", log: a.log, store: snapshotRuns{a.svc.Store}, now: time.Now, run: a.scheduledSnapshot,
		schedule: func() schedule.Schedule { return a.svc.Sys.Snapshot().SnapshotSchedule() },
	}
	if a.notifier != nil {
		b.missed = func(slot, at time.Time) {
			a.notifier.Emit(notify.SnapshotMissed(a.notifier.Config(), slot, at))
		}
	}
	return b
}

// scheduledSnapshot writes one snapshot of TasmoFleet's own database (the schedule says it is
// time), prunes to SNAPSHOT_KEEP and audits the outcome as "system". A failure is logged and
// audited with its reason; the stored snapshots stay as they were. The attempt is recorded either
// way (snapshot_runs): a failing disk is tried at the next scheduled time, not in a loop.
func (a *runtime) scheduledSnapshot(ctx context.Context) {
	if a.snapshots == nil {
		return
	}
	c := a.svc.Sys.Snapshot()
	defer func() {
		if ctx.Err() != nil {
			return // cut short by a shutdown: not handled, the next start may still run it
		}
		// One row, not cut by a shutdown that comes now.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := a.svc.Store.SaveSnapshotRun(sctx, store.SnapshotRunScheduled, time.Now()); err != nil {
			a.log.Warn("snapshot schedule: storing the run", "err", err)
		}
	}()
	res, err := a.snapshots.Run(ctx, c.SnapshotKeep)
	ev := model.AuditEvent{Actor: "system", Action: "snapshot.scheduled", Target: "database", Result: "ok"}
	switch {
	case err != nil:
		st := a.snapshots.Status()
		ev.Result = "failed: " + st.Reason
		ev.Detail = auditJSON(map[string]any{"file": st.File})
		a.log.Warn("scheduled database snapshot failed", "file", st.File, "err", err)
		if a.notifier != nil {
			a.notifier.Emit(notify.SnapshotFailed(a.notifier.Config(), st.Reason, time.Now()))
		}
	case res.PruneErr != nil:
		// The snapshot is good; only the retention is behind. The card keeps showing the good one.
		reason := snapshot.ReasonOf(res.PruneErr)
		ev.Result = "ok, prune failed: " + reason
		ev.Detail = auditJSON(map[string]any{"file": res.Info.Name, "bytes": res.Info.Size, "pruned": res.Pruned})
		a.log.Warn("database snapshot written, pruning old snapshots failed", "file", res.Info.Name, "err", res.PruneErr)
	default:
		ev.Detail = auditJSON(map[string]any{"file": res.Info.Name, "bytes": res.Info.Size, "pruned": res.Pruned})
	}
	a.svc.Audit.Record(ctx, ev)
}

func auditJSON(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "" // values are strings and numbers; cannot fail
	}
	return string(b)
}
