package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/schedule"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

func berlinZone(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("tzdata: %v", err)
	}
	return loc
}

// schedEnv is the schedule loop on a real store and the real scheduled run, with a clock the test moves.
type schedEnv struct {
	*fleetEnv
	s      *backupSchedule
	loc    *time.Location
	clock  time.Time
	sched  schedule.Schedule
	mu     sync.Mutex
	missed []time.Time // slots a notification was sent for
}

func newSchedEnv(t *testing.T, days, clock string) *schedEnv {
	t.Helper()
	loc := berlinZone(t)
	e := &schedEnv{fleetEnv: newFleetEnv(t), loc: loc}
	sc, err := schedule.New(days, clock)
	if err != nil {
		t.Fatal(err)
	}
	e.sched = sc
	e.at(2026, 11, 3, 9, 0)
	e.b.now = func() time.Time { return e.clock }
	e.s = &backupSchedule{
		log: e.b.log, store: e.st, now: func() time.Time { return e.clock },
		schedule: func() schedule.Schedule { return e.sched },
		run:      e.b.Run,
		missed: func(slot, _ time.Time) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.missed = append(e.missed, slot)
		},
	}
	return e
}

func (e *schedEnv) at(y int, mo time.Month, d, h, m int) {
	e.clock = time.Date(y, mo, d, h, m, 0, 0, e.loc)
}

func (e *schedEnv) runs(t *testing.T) int {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.tried)
}

const everyDay = "mon,tue,wed,thu,fri,sat,sun"

// scheduledOnce records that the run of the given time happened, as the real run does.
func (e *schedEnv) ranAt(t *testing.T, at time.Time) {
	t.Helper()
	if _, err := e.st.SaveBackupRun(context.Background(), model.BackupRun{Trigger: model.BackupRunScheduled, StartedAt: at, FinishedAt: at.Add(time.Minute), Completed: true}); err != nil {
		t.Fatal(err)
	}
}

// Why (#612, #601): a container restart, an update or a Home Assistant app restart must not back up
// the whole fleet. TasmoFleet starts at 10:00, the 03:00 run of today happened; nothing may be sent
// to any device, and the schedule only waits for tomorrow's 03:00.
func TestNoRunWhenTasmoFleetStarts(t *testing.T) {
	e := newSchedEnv(t, everyDay, "03:00")
	e.dev(t, "plug", e.clock, model.Health{State: model.HealthOnline})
	e.ranAt(t, time.Date(2026, 11, 3, 3, 0, 4, 0, e.loc))
	e.at(2026, 11, 3, 10, 0) // the start
	next := e.s.step(context.Background())
	if e.runs(t) != 0 || len(e.missed) != 0 {
		t.Fatalf("a start sent %d requests and %d notifications; it must do neither", e.runs(t), len(e.missed))
	}
	if want := time.Date(2026, 11, 4, 3, 0, 0, 0, e.loc); !next.Equal(want) {
		t.Errorf("waits until %v, want %v", next, want)
	}
}

// Why (#601): the schedule is not a timer that counts from the start. The first look at a schedule
// that has no history at all (it came from the environment, or the record was pruned) starts
// nothing and tells nobody: no one can say it ever missed a run.
func TestScheduleWithoutHistoryStartsNothingAndNotifiesNobody(t *testing.T) {
	e := newSchedEnv(t, everyDay, "03:00")
	e.dev(t, "plug", e.clock, model.Health{State: model.HealthOnline})
	e.at(2026, 11, 3, 3, 20) // 20 minutes after a time of day that nobody ever handled
	e.s.step(context.Background())
	if e.runs(t) != 0 || len(e.missed) != 0 {
		t.Fatalf("runs %d, notifications %d; want none of either", e.runs(t), len(e.missed))
	}
	e.at(2026, 11, 4, 3, 0) // the next time of day does run
	e.s.step(context.Background())
	if e.runs(t) != 1 {
		t.Errorf("the next scheduled time must run once, got %d", e.runs(t))
	}
}

// Why (#601): the run happens at its time, once. The loop wakes at 03:00 and backs up the fleet; the
// next look (the run took minutes) must find it settled and wait for the next day. A second look
// that ran again would hit every device twice.
func TestRunAtTheScheduledTimeHappensOnce(t *testing.T) {
	e := newSchedEnv(t, "tue", "03:00")
	e.dev(t, "plug", e.clock, model.Health{State: model.HealthOnline})
	e.ranAt(t, time.Date(2026, 10, 27, 3, 0, 1, 0, e.loc)) // last Tuesday
	e.at(2026, 11, 3, 3, 0)                                // Tuesday 03:00
	next := e.s.step(context.Background())
	if e.runs(t) != 1 {
		t.Fatalf("the run at its time: %d backups, want 1", e.runs(t))
	}
	if want := time.Date(2026, 11, 10, 3, 0, 0, 0, e.loc); !next.Equal(want) {
		t.Errorf("next look at %v, want next Tuesday %v", next, want)
	}
	e.s.step(context.Background())
	e.at(2026, 11, 3, 3, 5)
	e.s.step(context.Background())
	if e.runs(t) != 1 {
		t.Errorf("looking again started another run: %d backups", e.runs(t))
	}
}

// Why (#601): a run missed because TasmoFleet was down starts late when it is back within 60 minutes,
// and only once.
func TestMissedRunStartsLateWithin60Minutes(t *testing.T) {
	e := newSchedEnv(t, everyDay, "03:00")
	e.dev(t, "plug", e.clock, model.Health{State: model.HealthOnline})
	e.ranAt(t, time.Date(2026, 11, 2, 3, 0, 4, 0, e.loc))
	e.at(2026, 11, 3, 3, 59) // down at 03:00, back at 03:59
	e.s.step(context.Background())
	if e.runs(t) != 1 {
		t.Fatalf("59 minutes late: %d backups, want the late start", e.runs(t))
	}
	if len(e.missed) != 0 {
		t.Errorf("a late start is not a skipped run, but %v was reported", e.missed)
	}
	e.at(2026, 11, 3, 4, 10)
	e.s.step(context.Background())
	if e.runs(t) != 1 {
		t.Errorf("a second start for the same time: %d backups", e.runs(t))
	}
}

// Why (#601): after more than 60 minutes the run is not made up (a backup at lunchtime is not what
// "03:00" meant), and the person hears of it once, not at every restart afterwards.
func TestMissedRunLaterThan60MinutesIsSkippedWithOneNotification(t *testing.T) {
	e := newSchedEnv(t, everyDay, "03:00")
	e.dev(t, "plug", e.clock, model.Health{State: model.HealthOnline})
	e.ranAt(t, time.Date(2026, 11, 2, 3, 0, 4, 0, e.loc))
	e.at(2026, 11, 3, 4, 1) // 61 minutes
	next := e.s.step(context.Background())
	if e.runs(t) != 0 {
		t.Fatalf("61 minutes late: %d backups, want none", e.runs(t))
	}
	if want := time.Date(2026, 11, 3, 3, 0, 0, 0, e.loc); len(e.missed) != 1 || !e.missed[0].Equal(want) {
		t.Fatalf("notifications for %v, want exactly one for %v", e.missed, want)
	}
	if want := time.Date(2026, 11, 4, 3, 0, 0, 0, e.loc); !next.Equal(want) {
		t.Errorf("next look %v, want %v", next, want)
	}
	// Restart after restart: the skip is remembered.
	for i := 0; i < 3; i++ {
		e.at(2026, 11, 3, 4+i, 30)
		e.s.step(context.Background())
	}
	if len(e.missed) != 1 || e.runs(t) != 0 {
		t.Errorf("after restarts: %d notifications, %d backups; want 1 and 0", len(e.missed), e.runs(t))
	}
	_, missed, err := e.st.BackupRunTimes(context.Background())
	if err != nil || !missed.Equal(time.Date(2026, 11, 3, 4, 1, 0, 0, e.loc)) {
		t.Errorf("the skip is not recorded for the page: %v %v", missed, err)
	}
	// The next day runs normally.
	e.at(2026, 11, 4, 3, 0)
	e.s.step(context.Background())
	if e.runs(t) != 1 {
		t.Errorf("the day after a skip: %d backups, want 1", e.runs(t))
	}
}

// Why (#601): after a long downtime one decision is made for the newest time of day, not one per
// missed day: three days down is one skip (or one late run), never three backups in a row.
func TestLongDowntimeIsOneSkipNotOnePerMissedDay(t *testing.T) {
	e := newSchedEnv(t, everyDay, "03:00")
	e.dev(t, "plug", e.clock, model.Health{State: model.HealthOnline})
	e.ranAt(t, time.Date(2026, 11, 1, 3, 0, 4, 0, e.loc))
	e.at(2026, 11, 4, 12, 0)
	e.s.step(context.Background())
	e.s.step(context.Background())
	if e.runs(t) != 0 || len(e.missed) != 1 {
		t.Fatalf("%d backups, %d notifications; want 0 and 1", e.runs(t), len(e.missed))
	}
}

// Why (#601, #612): saving the schedule must not start a run. At 10:20 the person changes the time
// to 10:00; the 10:00 of today has "just passed" and is within the 60 minutes, so a plain
// "is something due" would back up the fleet because a form was saved. The change settles every
// time before it.
func TestSavingTheScheduleStartsNoRunAndNotifiesNobody(t *testing.T) {
	e := newSchedEnv(t, everyDay, "03:00")
	e.dev(t, "plug", e.clock, model.Health{State: model.HealthOnline})
	e.ranAt(t, time.Date(2026, 11, 3, 3, 0, 4, 0, e.loc))
	e.at(2026, 11, 3, 10, 20)
	e.sched, _ = schedule.New(everyDay, "10:00") // saved at 10:20
	e.s.reschedule(context.Background())
	e.s.step(context.Background())
	if e.runs(t) != 0 || len(e.missed) != 0 {
		t.Fatalf("saving started %d backups and %d notifications", e.runs(t), len(e.missed))
	}
	// A restart right after the save does not undo that: the marker is in the database, not in memory.
	restarted := *e.s
	restarted.floor = time.Time{}
	e.at(2026, 11, 3, 10, 25)
	restarted.step(context.Background())
	if e.runs(t) != 0 || len(e.missed) != 0 {
		t.Fatalf("a restart after the save started %d backups and %d notifications", e.runs(t), len(e.missed))
	}
	// Turning on a schedule that was off, after its time of day: the same.
	e.sched, _ = schedule.New("tue", "03:00")
	e.s.reschedule(context.Background())
	e.s.step(context.Background())
	if e.runs(t) != 0 || len(e.missed) != 0 {
		t.Fatalf("switching on started %d backups and %d notifications", e.runs(t), len(e.missed))
	}
	// The changed schedule does run at its next time.
	e.at(2026, 11, 10, 3, 0) // the next Tuesday 03:00
	e.s.step(context.Background())
	if e.runs(t) != 1 {
		t.Errorf("the saved schedule did not run at its time: %d", e.runs(t))
	}
}

// Why (#601): the loop itself reacts to a change of the settings with the marker, so the wiring
// (not only the method) keeps a save from starting a run. It runs the real loop on a real channel.
func TestLoopSettlesTheScheduleWhenTheSettingsChange(t *testing.T) {
	e := newSchedEnv(t, everyDay, "03:00")
	e.dev(t, "plug", e.clock, model.Health{State: model.HealthOnline})
	e.ranAt(t, time.Date(2026, 11, 3, 3, 0, 4, 0, e.loc))
	var clock atomic.Pointer[time.Time]
	set := func(tm time.Time) { clock.Store(&tm); e.clock = tm }
	set(time.Date(2026, 11, 3, 9, 0, 0, 0, e.loc))
	var looks atomic.Int32 // the loop reads the clock once in step and once to set its timer
	e.s.now = func() time.Time { looks.Add(1); return *clock.Load() }
	var sched atomic.Pointer[schedule.Schedule]
	cur := e.sched
	sched.Store(&cur)
	e.s.schedule = func() schedule.Schedule { return *sched.Load() }
	changed := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.s.loop(ctx, changed); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return looks.Load() >= 2 }, "the loop did not start waiting")

	set(time.Date(2026, 11, 3, 10, 20, 0, 0, e.loc))
	ns, _ := schedule.New(everyDay, "10:00")
	sched.Store(&ns)
	changed <- struct{}{}
	waitFor(t, func() bool {
		settled, _, _ := e.st.BackupRunTimes(context.Background())
		return settled.Equal(clock.Load().UTC().Truncate(time.Millisecond))
	}, "the loop did not settle the schedule after a change of the settings")
	cancel()
	<-done
	if e.runs(t) != 0 {
		t.Fatalf("the save started %d backups", e.runs(t))
	}
}

// Why (#601): the schedule runs on the wall clock. The night the clocks go back, 03:00 is
// followed by the next 03:00 25 hours later, and a restart in between must not repeat the run.
func TestRunAcrossTheClockChange(t *testing.T) {
	e := newSchedEnv(t, everyDay, "03:00")
	e.dev(t, "plug", e.clock, model.Health{State: model.HealthOnline})
	e.ranAt(t, time.Date(2026, 10, 24, 3, 0, 4, 0, e.loc))
	e.at(2026, 10, 25, 3, 0) // 25 October, 03:00 CET, an hour after the clocks went back
	e.s.step(context.Background())
	if e.runs(t) != 1 {
		t.Fatalf("25 Oct: %d backups, want 1", e.runs(t))
	}
	e.at(2026, 10, 25, 3, 30)
	e.s.step(context.Background())
	e.at(2026, 10, 26, 3, 0)
	next := e.s.step(context.Background())
	if e.runs(t) != 2 {
		t.Errorf("26 Oct: %d backups, want 2 in all", e.runs(t))
	}
	if want := time.Date(2026, 10, 27, 3, 0, 0, 0, e.loc); !next.Equal(want) {
		t.Errorf("next %v, want %v", next, want)
	}
}

// Why (#601): a schedule with no weekday is manual only. Nothing runs, the loop does not even read the
// record, however long ago anything was due.
func TestNoWeekdayMeansManualOnly(t *testing.T) {
	e := newSchedEnv(t, "", "03:00")
	e.dev(t, "plug", e.clock, model.Health{State: model.HealthOnline})
	e.at(2026, 11, 3, 3, 10)
	if next := e.s.step(context.Background()); !next.IsZero() || e.runs(t) != 0 || len(e.missed) != 0 {
		t.Fatalf("manual only: next %v, %d backups, %d notifications", next, e.runs(t), len(e.missed))
	}
}

// Why (#601): the scheduler starts the run of scheduledBackup, which holds the per-device rules (#409):
// an offline device and a device that somebody else works on are skipped, not failed, and nothing
// reaches them; an answering one gets exactly one backup.
func TestScheduledTimeKeepsThePerDeviceRules(t *testing.T) {
	e := newSchedEnv(t, everyDay, "03:00")
	e.dev(t, "ok", e.clock, model.Health{State: model.HealthOnline})
	e.dev(t, "offline", e.clock.Add(-48*time.Hour), model.Health{State: model.HealthOffline, Since: e.clock.Add(-48 * time.Hour)})
	busy := e.dev(t, "busy", e.clock, model.Health{State: model.HealthOnline})
	release, _, _ := e.locks.TryAcquire(busy, "restore")
	defer release()
	e.ranAt(t, time.Date(2026, 11, 2, 3, 0, 4, 0, e.loc))
	e.at(2026, 11, 3, 3, 0)
	e.s.step(context.Background())
	if got := strings.Join(e.tried, ","); got != "ok" {
		t.Fatalf("backed up %q, want only ok", got)
	}
	r := e.last(t)
	if r.OK != 1 || r.Failed != 0 || r.Skipped != 2 {
		t.Errorf("run %+v, want 1 ok, 0 failed, 2 skipped", r)
	}
	if item(r, "busy").Reason != skipBusy || item(r, "offline").Reason != skipOffline {
		t.Errorf("reasons: busy %q, offline %q", item(r, "busy").Reason, item(r, "offline").Reason)
	}
}

// failingMarkers wraps the store so that nothing can be written to the run record.
type failingMarkers struct{ backupTimes }

func (failingMarkers) SaveBackupRun(context.Context, model.BackupRun) (int64, error) {
	return 0, errors.New("disk full")
}

// Why (#601): if the record cannot be written, the due run must still happen only once. Without the
// in-memory floor the loop would see the unsettled time again at once and spin the fleet.
func TestFailedRecordWriteDoesNotRepeatTheRun(t *testing.T) {
	e := newSchedEnv(t, everyDay, "03:00")
	e.ranAt(t, time.Date(2026, 11, 2, 3, 0, 4, 0, e.loc))
	var n int
	e.s.store = failingMarkers{e.st}
	e.s.run = func(context.Context) { n++ }
	e.at(2026, 11, 3, 3, 0)
	for i := 0; i < 4; i++ {
		e.s.step(context.Background())
	}
	if n != 1 {
		t.Fatalf("the run started %d times with an unwritable record, want 1", n)
	}
	// The same for a skipped run: one notification, not one per look.
	e.at(2026, 11, 4, 12, 0)
	for i := 0; i < 4; i++ {
		e.s.step(context.Background())
	}
	if len(e.missed) != 1 {
		t.Fatalf("a skipped run was reported %d times with an unwritable record, want 1", len(e.missed))
	}
}

// Why (#601): the skipped-run message goes through the existing scheduled-backup notification (the
// same event name and the same on/off setting as a failed run), so nobody has to learn a new
// switch and a person who turned scheduled-backup notifications off does not get it.
func TestMissedRunNotificationUsesTheScheduledBackupEvent(t *testing.T) {
	a, svc, sent := notifyRuntime(t, nil)
	a.backupRun = newScheduledRun(nil)
	if _, err := svc.Sys.Apply(t.Context(), sysconfig.Request{Actor: "t", Values: map[string]string{"BACKUP_DAYS": everyDay, "BACKUP_TIME": "03:00"}}); err != nil {
		t.Fatal(err)
	}
	loc := berlinZone(t)
	b := a.backupSchedule()
	if _, err := svc.Store.SaveBackupRun(t.Context(), model.BackupRun{Trigger: model.BackupRunScheduled, StartedAt: time.Date(2026, 11, 2, 3, 0, 4, 0, loc), Completed: true}); err != nil {
		t.Fatal(err)
	}
	b.now = func() time.Time { return time.Date(2026, 11, 3, 9, 0, 0, 0, loc) }
	b.step(t.Context())
	got := sent()
	if len(got) != 1 || !strings.Contains(got[0], `"event":"backup.scheduled_failed"`) || !strings.Contains(got[0], "skipped") {
		t.Fatalf("sent %v, want one backup.scheduled_failed message that says skipped", got)
	}

	// Off in the settings: the person asked not to hear about scheduled backups.
	a2, svc2, sent2 := notifyRuntime(t, map[string]string{"NOTIFY_BACKUP": "false"})
	a2.backupRun = newScheduledRun(nil)
	if _, err := svc2.Sys.Apply(t.Context(), sysconfig.Request{Actor: "t", Values: map[string]string{"BACKUP_DAYS": everyDay}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc2.Store.SaveBackupRun(t.Context(), model.BackupRun{Trigger: model.BackupRunScheduled, StartedAt: time.Date(2026, 11, 2, 3, 0, 4, 0, loc), Completed: true}); err != nil {
		t.Fatal(err)
	}
	b2 := a2.backupSchedule()
	b2.now = func() time.Time { return time.Date(2026, 11, 3, 9, 0, 0, 0, loc) }
	b2.step(t.Context())
	if got := sent2(); len(got) != 0 {
		t.Errorf("NOTIFY_BACKUP is off but sent %v", got)
	}
}
