package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/snapshot"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
)

func snapshotRuntime(t *testing.T, days, keep string) (*runtime, *Services) {
	t.Helper()
	svc := importServices(t)
	if _, err := svc.Sys.Apply(t.Context(), sysconfig.Request{Actor: "test", Values: map[string]string{"SNAPSHOT_DAYS": days, "SNAPSHOT_KEEP": keep}}); err != nil {
		t.Fatal(err)
	}
	a := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log, bgContext: t.Context()}
	a.snapshots = snapshot.New(svc.Store, svc.Cfg.DataDir)
	return a, svc
}

func snapshotAudit(t *testing.T, svc *Services) []string {
	t.Helper()
	evs, err := svc.Store.ListAudit(t.Context(), store.AuditFilter{ActionPrefix: "snapshot."})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Actor+" "+e.Action+" "+e.Result+" "+e.Detail)
	}
	return out
}

// Why: when the schedule says it is time, a real snapshot of the real database lands in the data
// directory, the audit log says so, and the attempt is recorded in snapshot_runs: that record is what
// keeps the same time of day from being taken twice (the schedule tests) across a restart. It is
// recorded at the time the schedule started it, the clock the schedule decides by (a row stamped with
// another clock settles the wrong times), and as a run, not a skip, or the page would say "skipped"
// after every good snapshot.
func TestScheduledSnapshotIsWrittenAuditedAndRecorded(t *testing.T) {
	a, svc := snapshotRuntime(t, everyDay, "7")
	started := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)
	a.scheduledSnapshot(t.Context(), started)
	if settled, missed, err := svc.Store.SnapshotRunTimes(t.Context()); err != nil || !settled.Equal(started) || !missed.IsZero() {
		t.Fatalf("recorded: settled %v, missed %v (%v); want the start of the attempt and no skip", settled, missed, err)
	}
	list, err := a.snapshots.List()
	if err != nil || len(list) != 1 || list[0].Schema == 0 {
		t.Fatalf("stored: %v %v (a real database has a schema version)", list, err)
	}
	audit := snapshotAudit(t, svc)
	if len(audit) != 1 || !strings.HasPrefix(audit[0], "system snapshot.scheduled ok ") || !strings.Contains(audit[0], list[0].Name) {
		t.Fatalf("audit: %v", audit)
	}
	if st := a.snapshots.Status(); st.Failed || st.LastGood.IsZero() {
		t.Fatalf("status: %+v", st)
	}
}

// Why: a snapshot that cannot be written must be reported with its reason, in the audit log and
// for the card, instead of silently leaving the operator without the protection they switched on.
func TestScheduledSnapshotFailureIsAuditedWithItsReason(t *testing.T) {
	a, svc := snapshotRuntime(t, everyDay, "7")
	if err := os.WriteFile(filepath.Join(svc.Cfg.DataDir, snapshot.DirName), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.scheduledSnapshot(t.Context(), time.Now())
	st := a.snapshots.Status()
	if !st.Failed || st.Reason == "" || !strings.HasPrefix(st.File, "tasmofleet-") {
		t.Fatalf("status: %+v", st)
	}
	audit := snapshotAudit(t, svc)
	if len(audit) != 1 || !strings.HasPrefix(audit[0], "system snapshot.scheduled failed: "+st.Reason) {
		t.Fatalf("audit: %v", audit)
	}
}

// Why: the schedule is off by default; nothing may be written until an admin chooses days.
func TestScheduledSnapshotIsOffByDefault(t *testing.T) {
	svc := importServices(t)
	c := svc.Sys.Snapshot()
	if !c.SnapshotSchedule().Off() || c.SnapshotKeep != 7 || c.SnapshotTime != "03:00" {
		t.Fatalf("defaults: %+v / %d / %q", c.SnapshotSchedule(), c.SnapshotKeep, c.SnapshotTime)
	}
}
