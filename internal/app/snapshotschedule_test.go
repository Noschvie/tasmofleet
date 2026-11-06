package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/snapshot"
	"github.com/hellwand/tasmofleet/internal/store"
)

// snapSched is the snapshot schedule on a real store and the real scheduled snapshot, with a clock
// the test moves (#602). The loop is the one the fleet backup uses; these tests pin the wiring: the
// run record, the notification and what a snapshot is.
type snapSched struct {
	a    *runtime
	svc  *Services
	s    *scheduleLoop
	sent func() []string
	loc  *time.Location
	now  time.Time
}

func newSnapSched(t *testing.T, days, clock string) *snapSched {
	t.Helper()
	a, svc, sent := notifyRuntime(t, map[string]string{"SNAPSHOT_DAYS": days, "SNAPSHOT_TIME": clock})
	a.snapshots = snapshot.New(svc.Store, svc.Cfg.DataDir)
	e := &snapSched{a: a, svc: svc, sent: sent, loc: berlinZone(t), s: a.snapshotSchedule()}
	e.at(2026, 11, 3, 9, 0)
	e.s.now = func() time.Time { return e.now }
	return e
}

func (e *snapSched) at(y int, mo time.Month, d, h, m int) {
	e.now = time.Date(y, mo, d, h, m, 0, 0, e.loc)
}

// ran records that the snapshot of a time of day was handled, as the real run does.
func (e *snapSched) ran(t *testing.T, at time.Time) {
	t.Helper()
	if err := e.svc.Store.SaveSnapshotRun(t.Context(), store.SnapshotRunScheduled, at); err != nil {
		t.Fatal(err)
	}
}

// taken is how many scheduled snapshots were written (each is audited).
func (e *snapSched) taken(t *testing.T) int {
	t.Helper()
	return len(snapshotAudit(t, e.svc))
}

func (e *snapSched) step(t *testing.T) time.Time { return e.s.step(t.Context()) }

// Why (#602, #612 for the fleet backup): a container restart, an update or a Home Assistant app
// restart must not take a snapshot, or a container that restarts often would push the daily copies
// out of the retention. TasmoFleet starts at 10:00, today's 03:00 snapshot was taken: nothing is
// written and the schedule waits for tomorrow's 03:00.
func TestNoSnapshotWhenTasmoFleetStarts(t *testing.T) {
	e := newSnapSched(t, everyDay, "03:00")
	e.ran(t, time.Date(2026, 11, 3, 3, 0, 4, 0, e.loc))
	next := e.step(t)
	e.step(t)
	if n := e.taken(t); n != 0 || len(e.sent()) != 0 {
		t.Fatalf("a start took %d snapshots and sent %v; it must do neither", n, e.sent())
	}
	if want := time.Date(2026, 11, 4, 3, 0, 0, 0, e.loc); !next.Equal(want) {
		t.Errorf("waits until %v, want %v", next, want)
	}
}

// Why (#602): the snapshot is taken at its time, once, and recorded: the next look finds the time
// settled. A schedule that took it again would fill the retention in an afternoon.
func TestSnapshotIsTakenAtTheScheduledTimeOnce(t *testing.T) {
	e := newSnapSched(t, "tue", "03:00")
	e.ran(t, time.Date(2026, 10, 27, 3, 0, 1, 0, e.loc)) // last Tuesday
	e.at(2026, 11, 3, 3, 0)                              // Tuesday 03:00
	next := e.step(t)
	if n := e.taken(t); n != 1 {
		t.Fatalf("at the scheduled time: %d snapshots, want 1", n)
	}
	if want := time.Date(2026, 11, 10, 3, 0, 0, 0, e.loc); !next.Equal(want) {
		t.Errorf("next look at %v, want next Tuesday %v", next, want)
	}
	e.at(2026, 11, 3, 3, 5)
	e.step(t)
	if n := e.taken(t); n != 1 {
		t.Errorf("looking again took another snapshot: %d", n)
	}
}

// Why (#602): a snapshot missed because TasmoFleet was down is taken late when it is back within 60
// minutes (a copy of the database is cheap and the person wants the daily one), and not after that:
// then it is skipped and the person is told through the snapshot failure notification, with the
// page to look at.
func TestMissedSnapshotIsTakenLateWithin60MinutesElseSkippedAndNotified(t *testing.T) {
	e := newSnapSched(t, everyDay, "03:00")
	e.ran(t, time.Date(2026, 11, 2, 3, 0, 4, 0, e.loc))
	e.at(2026, 11, 3, 3, 40) // back 40 minutes late
	e.step(t)
	if n := e.taken(t); n != 1 || len(e.sent()) != 0 {
		t.Fatalf("40 minutes late: %d snapshots, sent %v; want it taken and nobody told", n, e.sent())
	}

	l := newSnapSched(t, everyDay, "03:00")
	l.ran(t, time.Date(2026, 11, 2, 3, 0, 4, 0, l.loc))
	l.at(2026, 11, 3, 4, 1) // 61 minutes late
	l.step(t)
	l.step(t)
	got := l.sent()
	if n := l.taken(t); n != 0 || len(got) != 1 {
		t.Fatalf("61 minutes late: %d snapshots, sent %v; want none taken and one notification", n, got)
	}
	if !strings.Contains(got[0], `"event":"snapshot.failed"`) || !strings.Contains(got[0], "skipped") {
		t.Errorf("the notification is %s; want the snapshot failure event, saying skipped", got[0])
	}
	_, missed, err := l.svc.Store.SnapshotRunTimes(t.Context())
	if err != nil || !missed.Equal(time.Date(2026, 11, 3, 4, 1, 0, 0, l.loc)) {
		t.Errorf("the skip is not recorded for the page: %v %v", missed, err)
	}
	l.at(2026, 11, 4, 3, 0) // the next day is normal
	l.step(t)
	if n := l.taken(t); n != 1 {
		t.Errorf("the day after a skip: %d snapshots, want 1", n)
	}
}

// Why (#602): "I do not want to hear about failed snapshots" must also silence the skipped ones; they
// share the setting (NOTIFY_SNAPSHOT) so nobody has to learn a new switch.
func TestSkippedSnapshotFollowsTheSnapshotNotificationSetting(t *testing.T) {
	a, svc, sent := notifyRuntime(t, map[string]string{"SNAPSHOT_DAYS": everyDay, "NOTIFY_SNAPSHOT": "false"})
	a.snapshots = snapshot.New(svc.Store, svc.Cfg.DataDir)
	loc := berlinZone(t)
	if err := svc.Store.SaveSnapshotRun(t.Context(), store.SnapshotRunScheduled, time.Date(2026, 11, 2, 3, 0, 4, 0, loc)); err != nil {
		t.Fatal(err)
	}
	b := a.snapshotSchedule()
	b.now = func() time.Time { return time.Date(2026, 11, 3, 9, 0, 0, 0, loc) }
	b.step(t.Context())
	if got := sent(); len(got) != 0 {
		t.Errorf("NOTIFY_SNAPSHOT is off but sent %v", got)
	}
}

// Why (#602): saving the schedule must not take a snapshot, and neither must a restart right after
// saving. At 10:20 the person changes the time to 10:00; that 10:00 has "just passed" and is within
// the 60 minutes, so a plain "is something due" would take a snapshot because a form was saved. The
// change settles every time before it, in the database, so a restart does not undo it.
func TestSavingTheSnapshotScheduleTakesNoSnapshotNotEvenAfterARestart(t *testing.T) {
	e := newSnapSched(t, everyDay, "10:00")
	e.ran(t, time.Date(2026, 11, 3, 3, 0, 4, 0, e.loc))
	e.at(2026, 11, 3, 10, 20)
	e.s.reschedule(t.Context()) // what the loop does when SNAPSHOT_DAYS or SNAPSHOT_TIME is saved
	e.step(t)
	if n := e.taken(t); n != 0 || len(e.sent()) != 0 {
		t.Fatalf("saving took %d snapshots and sent %v", n, e.sent())
	}
	restarted := e.a.snapshotSchedule() // a new process: nothing in memory
	restarted.now = func() time.Time { return time.Date(2026, 11, 3, 10, 25, 0, 0, e.loc) }
	restarted.step(t.Context())
	if n := e.taken(t); n != 0 || len(e.sent()) != 0 {
		t.Fatalf("a restart after the save took %d snapshots and sent %v", n, e.sent())
	}
}

// Why (#602): the first start after the update must run nothing even though 03:00 passed a few minutes
// ago. The migration's marker is what says so; this is the schedule reading it.
func TestFirstStartAfterTheMigrationTakesNoSnapshot(t *testing.T) {
	e := newSnapSched(t, everyDay, "03:00")
	e.at(2026, 11, 3, 3, 20)
	if err := e.svc.Store.SaveSnapshotRun(t.Context(), store.SnapshotRunRescheduled, time.Date(2026, 11, 3, 3, 10, 0, 0, e.loc)); err != nil {
		t.Fatal(err)
	}
	e.step(t)
	if n := e.taken(t); n != 0 || len(e.sent()) != 0 {
		t.Fatalf("the first start took %d snapshots and sent %v", n, e.sent())
	}
}

// Why (#602): neither pruning protects a snapshot from being taken twice nor loses one. (1) SNAPSHOT_KEEP
// and an admin's Delete remove snapshot files, even the newest: the schedule must not read that as "no
// snapshot yet today" and take another. (2) The run record follows the audit retention, which can be
// shorter than the wait between two scheduled days: a process that lives past the prune of the last row
// must still take Monday's snapshot, not skip it without a record or a notification.
func TestPruningNeitherRepeatsNorLosesAScheduledSnapshot(t *testing.T) {
	e := newSnapSched(t, "mon,wed,fri", "03:00")
	e.ran(t, time.Date(2026, 11, 4, 3, 0, 4, 0, e.loc)) // Wednesday's
	e.at(2026, 11, 6, 3, 0)                             // Friday: taken now
	e.step(t)
	list, err := e.a.snapshots.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("stored %v %v, want Friday's snapshot", list, err)
	}
	if err := e.a.snapshots.Delete(list[0].Name); err != nil { // the admin deletes the only file
		t.Fatal(err)
	}
	e.at(2026, 11, 6, 3, 30)
	e.step(t)
	if n := e.taken(t); n != 1 {
		t.Fatalf("after the file was deleted: %d snapshots, want still 1 (the time of day is handled)", n)
	}
	// A restart on Saturday: the row is still there.
	restarted := e.a.snapshotSchedule()
	restarted.now = func() time.Time { return time.Date(2026, 11, 7, 10, 0, 0, 0, e.loc) }
	restarted.step(t.Context())
	// AUDIT_RETENTION 48h: the daily retention run on Sunday deletes Friday's row.
	if n, err := e.svc.Store.PruneSnapshotRuns(t.Context(), time.Date(2026, 11, 7, 3, 30, 0, 0, e.loc)); err != nil || n < 1 {
		t.Fatalf("pruned %d rows (%v), want Friday's", n, err)
	}
	restarted.now = func() time.Time { return time.Date(2026, 11, 9, 3, 0, 0, 0, e.loc) } // Monday 03:00, the same process
	restarted.step(t.Context())
	if n := e.taken(t); n != 2 || len(e.sent()) != 0 {
		t.Fatalf("Monday 03:00: %d snapshots (want 2 with Friday's), sent %v; want the scheduled snapshot", n, e.sent())
	}
}

// Why (#602): a snapshot that cannot be written settles its time of day like a good one. The person
// is told once (the failure notification); the schedule does not try again every minute against a
// disk that is full, and does not take a late "make-up" snapshot after the next look either, not even
// after a restart within the 60 minutes: the attempt is in the run record, not only in memory.
func TestFailedScheduledSnapshotSettlesItsTimeAndIsNotRetriedInALoop(t *testing.T) {
	e := newSnapSched(t, everyDay, "03:00")
	e.ran(t, time.Date(2026, 11, 2, 3, 0, 4, 0, e.loc))
	if err := os.WriteFile(filepath.Join(e.svc.Cfg.DataDir, snapshot.DirName), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.at(2026, 11, 3, 3, 0)
	for range 3 {
		e.step(t)
	}
	restarted := e.a.snapshotSchedule() // a new process 20 minutes later: nothing in memory
	restarted.now = func() time.Time { return time.Date(2026, 11, 3, 3, 20, 0, 0, e.loc) }
	restarted.step(t.Context())
	got := e.sent()
	if len(got) != 1 || !strings.Contains(got[0], `"event":"snapshot.failed"`) || strings.Contains(got[0], "skipped") {
		t.Fatalf("sent %v, want exactly one failure message", got)
	}
	if n := len(snapshotAudit(t, e.svc)); n != 1 {
		t.Errorf("%d attempts audited, want 1", n)
	}
}

// Why (#602): a cut by shutdown is not a handled time of day. If the context is gone while the
// snapshot is written, the run must not be recorded, or the next start would skip it (the schedule
// would find the time settled and wait for tomorrow).
func TestSnapshotCutByShutdownIsNotRecordedAsHandled(t *testing.T) {
	a, svc := snapshotRuntime(t, everyDay, "7")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a.scheduledSnapshot(ctx, time.Now())
	if settled, _, err := svc.Store.SnapshotRunTimes(t.Context()); err != nil || !settled.IsZero() {
		t.Fatalf("a run cut by shutdown was recorded: %v %v", settled, err)
	}
}

// Why (#602): the run record follows the audit retention like the backup runs, so the daily retention
// run keeps it from growing for ever; a row inside the retention stays.
func TestRetentionPrunesSnapshotRuns(t *testing.T) {
	_, svc := snapshotRuntime(t, everyDay, "7")
	old, recent := time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour)
	for _, at := range []time.Time{old, recent} {
		if err := svc.Store.SaveSnapshotRun(t.Context(), store.SnapshotRunScheduled, at); err != nil {
			t.Fatal(err)
		}
	}
	a := &runtime{svc: &Services{Sys: auditKeepSys(t, 24*time.Hour), Store: svc.Store}, log: svc.Log}
	a.prune(t.Context())
	if n, err := svc.Store.PruneSnapshotRuns(t.Context(), old.Add(time.Minute)); err != nil || n != 0 {
		t.Errorf("%d rows older than the audit retention are still stored (%v)", n, err)
	}
	if settled, _, err := svc.Store.SnapshotRunTimes(t.Context()); err != nil || !settled.Equal(recent.Truncate(time.Millisecond)) {
		t.Errorf("the row inside the retention is gone: settled %v (%v)", settled, err)
	}
}
