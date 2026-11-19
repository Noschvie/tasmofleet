package app

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hellwand/tasmofleet/internal/clock"
	"github.com/hellwand/tasmofleet/internal/devlock"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/ota"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
)

// Why (#993): "Back up all now" posts the devices in device ID order and "Back up selected" in the list's sort, yet a
// person reading the run expects A to Z, the way the scheduled run goes. The jobs of a bulk backup must therefore be
// created (and, by job ID, started) in the order of the device store's own list, case-insensitive and then by ID,
// whatever order they were posted in. Real SQLite store: the ordering rule is the store's, not a copy of it.
func TestBulkBackupStartsDevicesByNameWhateverOrderTheyWerePosted(t *testing.T) {
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	ctx := context.Background()
	byName := map[string]model.DeviceID{}
	for i, name := range []string{"Zeta", "alpha", "Beta", "beta"} { // IDs 1 to 4: the order the devices were added in
		id, err := st.CreateDevice(ctx, model.Device{Name: name, Address: "192.0.2." + string(rune('1'+i))})
		if err != nil {
			t.Fatal(err)
		}
		byName[name] = id
	}
	scheduled, err := st.ListDevices(ctx) // what the scheduled run walks
	if err != nil {
		t.Fatal(err)
	}
	var want []model.DeviceID
	for _, d := range scheduled {
		want = append(want, d.ID)
	}
	if !slices.Equal(want, []model.DeviceID{byName["alpha"], byName["Beta"], byName["beta"], byName["Zeta"]}) {
		t.Fatalf("the store's own order is %v; the test's premise (name, case-insensitive, then ID) changed", want)
	}

	bulk := lockedBackupBatch{e: ota.NewEngine(ota.Options{}, st, st, nil, nil, nil, clock.Real{}, nil), g: deviceGuard{locks: devlock.New(), jobs: st}}
	posted := []model.DeviceID{byName["Zeta"], byName["beta"], byName["alpha"], byName["Beta"]}
	batch, skipped, err := bulk.Submit(ctx, "alice", posted, ota.BackupOptions{})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("submit: %v %v", err, skipped)
	}
	jobs, err := st.ListJobs(ctx, store.JobFilter{BatchID: batch})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(jobs, func(a, b model.Job) int { return int(a.ID - b.ID) })
	var got []model.DeviceID
	for _, j := range jobs {
		got = append(got, j.DeviceID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("jobs by ID are for %v, want %v (the scheduled run's order); the posted order was %v", got, want, posted)
	}
	active, err := st.ActiveJobs(ctx) // what the engine and a restart read: oldest first
	if err != nil {
		t.Fatal(err)
	}
	var started []model.DeviceID
	for _, j := range active {
		started = append(started, j.DeviceID)
	}
	if !slices.Equal(started, want) {
		t.Fatalf("the engine would start %v, want %v", started, want)
	}
}
