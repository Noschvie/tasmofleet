package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/snapshot"
)

// scheduledSnapshot writes one snapshot of TasmoFleet's own database when one is due
// (SNAPSHOT_INTERVAL), prunes to SNAPSHOT_KEEP and audits the outcome as "system". A failure is
// logged and audited with its reason; the stored snapshots stay as they were and the next round
// tries again. "Due" keeps a restart from taking an extra snapshot and pushing the daily ones out
// of the retention.
func (a *runtime) scheduledSnapshot(ctx context.Context) {
	if a.snapshots == nil {
		return
	}
	c := a.svc.Sys.Snapshot()
	if c.SnapshotSchedule <= 0 || !a.snapshots.Due(c.SnapshotSchedule) {
		return
	}
	res, err := a.snapshots.Run(ctx, c.SnapshotKeep)
	ev := model.AuditEvent{Actor: "system", Action: "snapshot.scheduled", Target: "database", Result: "ok"}
	switch {
	case err != nil:
		st := a.snapshots.Status()
		ev.Result = "failed: " + st.Reason
		ev.Detail = auditJSON(map[string]any{"file": st.File})
		a.log.Warn("scheduled database snapshot failed", "file", st.File, "err", err)
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

// snapshotWait is how long the schedule sleeps before it looks again: until the next snapshot is
// due, counted from the newest stored one (so a restart does not move the schedule), at least a
// minute so an overdue or failing run cannot spin.
func (a *runtime) snapshotWait() time.Duration {
	every := a.svc.Sys.Snapshot().SnapshotSchedule
	if every <= 0 || a.snapshots == nil {
		return every
	}
	return max(time.Until(a.snapshots.NextRun(every)), time.Minute)
}

func auditJSON(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "" // values are strings and numbers; cannot fail
	}
	return string(b)
}
