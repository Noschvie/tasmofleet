package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/snapshot"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
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
// keeps the same time of day from being taken twice (the schedule tests) across a restart.
func TestScheduledSnapshotIsWrittenAuditedAndRecorded(t *testing.T) {
	a, svc := snapshotRuntime(t, everyDay, "7")
	a.scheduledSnapshot(t.Context())
	if settled, _, err := svc.Store.SnapshotRunTimes(t.Context()); err != nil || settled.IsZero() {
		t.Fatalf("the run was not recorded: %v %v", settled, err)
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
	a.scheduledSnapshot(t.Context())
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
