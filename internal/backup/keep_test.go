package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

func ids(bs []model.Backup) []model.BackupID {
	var out []model.BackupID
	for _, b := range bs {
		out = append(out, b.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Why: "Pruned at next run" is a promise. The label must name exactly the
// files the pruning after the next backup removes, for every combination of
// keep, age limit and kept runs, or the user trusts a backup that is about to
// go (or worries about one that stays). The test asks the label first, then
// takes the backup and compares with what really went.
func TestPrunedAtNextRunIsWhatTheNextBackupPrunes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		keep   int
		maxAge time.Duration
		runs   int
		kept   []int // indexes of runs (oldest = 0) to keep
		want   int   // runs labelled; the new run takes one of the keep slots
	}{
		{"steady state drops the oldest", 3, 0, 3, nil, 1},
		{"under the limit drops nothing", 5, 0, 3, nil, 0},
		{"lowered retention drops several", 1, 0, 4, nil, 4},
		{"kept runs neither go nor count", 2, 0, 4, []int{0, 1}, 1},
		{"kept middle run is skipped", 2, 0, 4, []int{1}, 2},
		{"age limit protects recent runs", 1, 30 * time.Hour, 4, nil, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, func(o *Options) { o.Keep = 50 })
			start := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
			var dumps []model.Backup
			for i := 0; i < tc.runs; i++ {
				dumps = append(dumps, e.takeAt(t, start.Add(time.Duration(i)*24*time.Hour)))
			}
			for _, k := range tc.kept {
				if _, err := e.m.SetKept(t.Context(), "op", dumps[k].ID, true); err != nil {
					t.Fatal(err)
				}
			}
			e.m.SetRetention(tc.keep, tc.maxAge)
			e.clk.t = start.Add(time.Duration(tc.runs) * 24 * time.Hour)
			label, err := e.m.PrunedAtNextRun(t.Context(), 7)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := e.m.List(t.Context(), 7)
			if _, err := e.m.Backup(t.Context(), "op", 7); err != nil {
				t.Fatal(err)
			}
			after, _ := e.m.List(t.Context(), 7)
			left := map[model.BackupID]bool{}
			for _, b := range after {
				left[b.ID] = true
			}
			var gone []model.Backup
			for _, b := range before {
				if !left[b.ID] {
					gone = append(gone, b)
				}
			}
			if fmt.Sprint(ids(label)) != fmt.Sprint(ids(gone)) {
				t.Fatalf("label %v, pruned %v", ids(label), ids(gone))
			}
			if got := countRuns(label); got != tc.want {
				t.Errorf("labelled runs = %d, want %d", got, tc.want)
			}
		})
	}
}

// Why: a backup pinned before a risky change ("before moving to the garage")
// must survive any number of later backups, and it must not use up one of
// the retention slots, or pinning would silently shorten the history.
func TestKeptRunSurvivesRetentionAndDoesNotCount(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Keep = 2 })
	start := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	pinned := e.takeAt(t, start)
	got, err := e.m.SetKept(t.Context(), "op", pinned.ID, true)
	if err != nil || !got.Kept {
		t.Fatalf("SetKept: %v %+v", err, got)
	}
	for i := 1; i <= 4; i++ {
		e.takeAt(t, start.Add(time.Duration(i)*time.Hour))
	}
	list, _ := e.m.List(t.Context(), 7)
	if n := countRuns(list); n != 3 {
		t.Fatalf("runs = %d, want the pinned one plus 2", n)
	}
	for _, b := range runsOf(list, pinned.TakenAt) {
		if !b.Kept {
			t.Errorf("%s of the pinned run is not kept", b.Kind)
		}
	}
	if len(runsOf(list, pinned.TakenAt)) != 3 {
		t.Fatal("pinned run lost files")
	}
	if _, err := os.Stat(filepath.Join(e.dir, filepath.FromSlash(pinned.Path))); err != nil {
		t.Fatalf("pinned file gone from disk: %v", err)
	}
	// Released, it counts again and goes with the next pruning.
	if _, err := e.m.SetKept(t.Context(), "op", pinned.ID, false); err != nil {
		t.Fatal(err)
	}
	e.takeAt(t, start.Add(5*time.Hour))
	list, _ = e.m.List(t.Context(), 7)
	if len(runsOf(list, pinned.TakenAt)) != 0 {
		t.Error("released run survived the retention")
	}
	var actions []string
	for _, a := range e.audit.ev {
		if strings.HasPrefix(a.Action, "backup.keep") || strings.HasPrefix(a.Action, "backup.release") {
			actions = append(actions, a.Action+":"+a.Result)
		}
	}
	if fmt.Sprint(actions) != "[backup.keep:ok backup.release:ok]" {
		t.Errorf("audit = %v", actions)
	}
}

// Why: a failed backup stores nothing, so without its own record the list
// would look as if nothing had been tried. The stored reason is shown in the
// browser; device errors can carry the URL with the web password, so the
// reason is chosen from the error kind and never copies the error text.
func TestFailedBackupIsRecordedWithoutCredentials(t *testing.T) {
	e := newEnv(t, nil)
	e.dev.dlErr = fmt.Errorf("%w: GET http://admin:hunter2-fake@172.31.13.28/dl?user=admin&password=hunter2-fake", tasmota.ErrAuth)
	ctx := WithRun(t.Context(), Run{Trigger: model.TriggerScheduled})
	if _, err := e.m.Backup(ctx, "system", 7); !errors.Is(err, tasmota.ErrAuth) {
		t.Fatalf("got %v", err)
	}
	at, err := e.m.Attempts(t.Context(), 7, 0)
	if err != nil || len(at) != 1 {
		t.Fatalf("attempts: %v %+v", err, at)
	}
	a := at[0]
	if strings.Contains(a.Reason, "hunter2") || strings.Contains(a.Reason, "http") {
		t.Fatalf("reason leaks the request: %q", a.Reason)
	}
	if a.Reason != "HTTP 401: the device rejected the web password" || a.Trigger != model.TriggerScheduled || !a.At.Equal(e.clk.t) {
		t.Errorf("attempt = %+v", a)
	}
	// Shutdown is not a failure of the device.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, _ = e.m.Backup(cancelled, "system", 7) //nolint:errcheck // only the absence of a record matters
	if at, _ := e.m.Attempts(t.Context(), 7, 0); len(at) != 1 {
		t.Errorf("cancelled run recorded: %+v", at)
	}
}

func TestAttemptReasonNeverEchoesTheError(t *testing.T) {
	for _, err := range []error{
		tasmota.ErrUnreachable, tasmota.ErrTimeout, tasmota.ErrForbiddenAddr, tasmota.ErrBadResponse, errEmptyDump,
		errors.New("dial tcp 10.0.0.1: password=fake-secret"),
	} {
		wrapped := fmt.Errorf("%w (secret-marker)", err)
		if r := AttemptReason(wrapped); r == "" || strings.Contains(r, "secret") || strings.Contains(r, "10.0.0.1") {
			t.Errorf("%v -> %q", err, r)
		}
	}
}

// Why: a bulk backup with "Keep these backups" and a label must store the
// whole run kept and labelled, and the list must be able to say what started
// each backup ("Automatic, before update to 14.3.0").
func TestRunTriggerKeepAndLabelAreStored(t *testing.T) {
	e := newEnv(t, nil)
	ctx := WithRun(t.Context(), Run{Trigger: model.TriggerBulk, Keep: true, Label: "before\nmoving   to the garage\x00"})
	made, err := e.m.Backup(ctx, "op", 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range made {
		if !b.Kept || b.Trigger != model.TriggerBulk || b.DeviceName != "kitchen" {
			t.Errorf("%s: %+v", b.Kind, b)
		}
	}
	dump, _ := byKindUnit(made, model.BackupConfigDump)
	if dump.Note != "before moving to the garage" {
		t.Errorf("label stored as %q", dump.Note)
	}
	plain, err := e.m.Backup(WithRun(t.Context(), Run{Trigger: model.TriggerUpdate, Ref: "14.3.0"}), "op", 7)
	if err != nil {
		t.Fatal(err)
	}
	if plain[0].Kept || plain[0].Trigger != model.TriggerUpdate || plain[0].TriggerRef != "14.3.0" {
		t.Errorf("update run: %+v", plain[0])
	}
	manual, _ := e.m.Backup(t.Context(), "op", 7)
	if manual[0].Trigger != model.TriggerManual {
		t.Errorf("a backup without a run is manual, got %q", manual[0].Trigger)
	}
	// The label is user text at the start of the note; it must not pass for
	// the replacement-hardware marker, or Replace would skip this dump.
	fake, err := e.m.Backup(WithRun(t.Context(), Run{Trigger: model.TriggerBulk, Label: NoteReplacementHardware}), "op", 7)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := byKindUnit(fake, model.BackupConfigDump); IsReplacementHardware(d) {
		t.Errorf("a label faked the replacement-hardware marker: %q", d.Note)
	}
	if long := CleanLabel(strings.Repeat("x", 300)); len(long) != maxLabel {
		t.Errorf("label not cut: %d", len(long))
	}
}

// Why: "Delete selected" deletes whole runs and only those of the device on
// the page; a forged id of another device must delete nothing at all.
func TestDeleteRunsIsScopedAndWhole(t *testing.T) {
	e := newEnv(t, nil)
	day := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	a := e.takeAt(t, day)
	b := e.takeAt(t, day.Add(time.Hour))
	c := e.takeAt(t, day.Add(2*time.Hour))
	foreign, _ := e.st.PutBackup(t.Context(), model.Backup{DeviceID: 8, Kind: model.BackupConfigDump, TakenAt: day})
	if _, err := e.m.DeleteRuns(t.Context(), "adm", 7, []model.BackupID{a.ID, foreign}); !errors.Is(err, ErrForeignBackup) {
		t.Fatalf("foreign id: %v", err)
	}
	if _, err := e.m.DeleteRuns(t.Context(), "adm", 7, nil); !errors.Is(err, ErrNothingSelected) {
		t.Fatalf("empty selection: %v", err)
	}
	if n := e.files(t); n != 9 {
		t.Fatalf("refused delete removed files: %d left", n)
	}
	plan, err := e.m.PlanDeleteRuns(t.Context(), 7, []model.BackupID{a.ID, c.ID})
	if err != nil || plan.Runs != 2 || plan.Files != 6 || !plan.Newest || plan.Remaining != 1 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	d, err := e.m.DeleteRuns(t.Context(), "adm", 7, []model.BackupID{a.ID, c.ID})
	if err != nil || d != plan {
		t.Fatalf("delete %+v %v, plan was %+v", d, err, plan)
	}
	list, _ := e.m.List(t.Context(), 7)
	if len(list) != 3 || !list[0].TakenAt.Equal(b.TakenAt) {
		t.Fatalf("left: %+v", list)
	}
	if n := e.files(t); n != 3 {
		t.Errorf("files on disk: %d", n)
	}
}

// Why (N2): "Delete the backups too" must remove every file, kept ones
// included; "Keep" leaves orphans that can still be listed, opened and
// deleted one run at a time.
func TestDeleteAllAndOrphanRuns(t *testing.T) {
	e := newEnv(t, nil)
	day := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	first := e.takeAt(t, day)
	if _, err := e.m.SetKept(t.Context(), "op", first.ID, true); err != nil {
		t.Fatal(err)
	}
	e.takeAt(t, day.Add(time.Hour))
	d, err := e.m.DeleteAll(t.Context(), "adm", 7)
	if err != nil || d.Runs != 2 || d.Files != 6 {
		t.Fatalf("DeleteAll %+v %v", d, err)
	}
	if n := e.files(t); n != 0 {
		t.Fatalf("%d files left", n)
	}

	old := e.takeAt(t, day.Add(2*time.Hour))
	e.takeAt(t, day.Add(3*time.Hour))
	e.st.orphan(7)
	orphans, _ := e.m.Orphans(t.Context())
	if len(orphans) != 6 || orphans[0].DeviceName != "kitchen" {
		t.Fatalf("orphans: %+v", orphans)
	}
	if _, _, err := e.m.Open(t.Context(), old.ID); err != nil {
		t.Fatalf("orphan cannot be opened: %v", err)
	}
	del, err := e.m.DeleteRun(t.Context(), "adm", old.ID)
	if err != nil || del.Files != 3 || del.Newest || del.Remaining != 1 {
		t.Fatalf("orphan run delete: %+v %v", del, err)
	}
	if n := e.files(t); n != 3 {
		t.Errorf("files left: %d", n)
	}
}

// usageBackups is memBackups with the store's aggregate (N9).
type usageBackups struct {
	*memBackups
	rows []store.BackupUsage
}

func (u usageBackups) BackupUsage(context.Context) ([]store.BackupUsage, error) { return u.rows, nil }

// Why (N9): Settings -> Backups reads the storage summary through the manager.
// A store that cannot aggregate must fail loud: an empty answer would read as
// "no backups at all", which is false and hides the disk usage.
func TestUsagePassesTheStoreAggregateOrFailsLoud(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.m.Usage(t.Context()); !errors.Is(err, ErrNoUsage) {
		t.Fatalf("store without aggregate: %v", err)
	}
	want := []store.BackupUsage{{Device: 7, Name: "kitchen", Runs: 2, Bytes: 300}}
	e = newEnv(t, func(o *Options) { o.Store = usageBackups{memBackups: &memBackups{}, rows: want} })
	got, err := e.m.Usage(t.Context())
	if err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("usage %+v %v", got, err)
	}
}
