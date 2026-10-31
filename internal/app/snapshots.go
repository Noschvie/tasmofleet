package app

import (
	"context"
	"encoding/json"

	"github.com/nofuturekid/tasmofleet/internal/model"
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
	in, pruned, err := a.snapshots.Run(ctx, c.SnapshotKeep)
	ev := model.AuditEvent{Actor: "system", Action: "snapshot.scheduled", Target: "database", Result: "ok"}
	switch {
	case err != nil:
		st := a.snapshots.Status()
		ev.Result = "failed: " + st.Reason
		ev.Detail = auditJSON(map[string]any{"file": st.File})
		a.log.Warn("scheduled database snapshot failed", "file", st.File, "err", err)
	default:
		ev.Detail = auditJSON(map[string]any{"file": in.Name, "bytes": in.Size, "pruned": pruned})
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
