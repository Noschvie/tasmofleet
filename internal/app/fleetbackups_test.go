package app

import (
	"context"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// countingFleetStore counts how often each read happens: the page must not grow a query per device.
type countingFleetStore struct {
	usage []store.BackupUsage
	run   model.BackupRun
	hasRn bool
	seen  map[model.DeviceID]time.Time
	calls map[string]int
}

func (c *countingFleetStore) BackupUsage(context.Context) ([]store.BackupUsage, error) {
	c.calls["usage"]++
	return c.usage, nil
}
func (c *countingFleetStore) LastBackupRun(context.Context) (model.BackupRun, bool, error) {
	c.calls["run"]++
	return c.run, c.hasRn, nil
}
func (c *countingFleetStore) DevicesSeen(context.Context) (map[model.DeviceID]time.Time, error) {
	c.calls["seen"]++
	return c.seen, nil
}

// Why (#409): the page lists the whole fleet at once and must stay cheap however big it is: a
// fixed number of reads, none of them a device request, and every device gets its row, with its
// result of the last run and what the next run would do with it.
func TestFleetOverviewIsABoundedNumberOfReadsWithoutDeviceIO(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.Local)
	st := &countingFleetStore{calls: map[string]int{}, hasRn: true,
		usage: []store.BackupUsage{
			{Device: 0, Runs: 3, Bytes: 9}, // orphans: not a device row
			{Device: 1, Name: "kitchen", Runs: 4, Kept: 1, Bytes: 400, Last: now.Add(-2 * time.Hour)},
			{Device: 2, Name: "garage"},
			{Device: 3, Name: "attic", Runs: 1, Bytes: 10, Last: now.Add(-72 * time.Hour)},
		},
		run: model.BackupRun{Items: []model.BackupRunItem{
			{DeviceID: 1, DeviceName: "kitchen", Outcome: model.BackupOutcomeOK, At: now.Add(-2 * time.Hour)},
			{DeviceID: 3, DeviceName: "attic", Outcome: model.BackupOutcomeFailed, Reason: "The device did not answer", At: now.Add(-2 * time.Hour)},
		}},
		seen: map[model.DeviceID]time.Time{1: now, 3: now.Add(-24 * time.Hour)},
	}
	f := &fleetBackups{store: st, now: func() time.Time { return now },
		health: func() map[model.DeviceID]model.Health {
			return map[model.DeviceID]model.Health{3: {State: model.HealthOffline, Since: now.Add(-24 * time.Hour)}}
		}}
	rows, err := f.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for k, n := range st.calls {
		if n != 1 {
			t.Errorf("%s read %d times, want once however many devices there are", k, n)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want one per device (not the orphans)", len(rows))
	}
	k, g, a := rows[0], rows[1], rows[2]
	if k.Name != "kitchen" || k.Runs != 4 || k.Kept != 1 || k.Bytes != 400 || k.LastOutcome != model.BackupOutcomeOK || k.SkipReason != "" || !k.NewestAt.Equal(now.Add(-2*time.Hour)) {
		t.Errorf("kitchen = %+v", k)
	}
	if g.Name != "garage" || !g.NewestAt.IsZero() || g.LastOutcome != "" || g.SkipReason != "never answered" {
		t.Errorf("garage = %+v, want no backup, not in the last run, would be skipped as never answered", g)
	}
	if a.LastOutcome != model.BackupOutcomeFailed || a.LastReason != "The device did not answer" || a.SkipReason != "offline since 4 Nov 12:00" {
		t.Errorf("attic = %+v", a)
	}
}
