//go:build integration

package backup

import (
	"os"
	"testing"
	"time"
)

// Why: "Delete older than" must work on what a real (simulated) device
// produces through the real HTTP client, not only on canned replies: files of
// old runs go, the run at and after the boundary stays readable.
func TestDeleteOlderThanAgainstSimulator(t *testing.T) {
	e := newSimEnv(t, "15.6.0", nil, nil)
	day := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	var keep int64
	for i := 0; i < 3; i++ {
		e.clk.t = day.Add(time.Duration(i) * 24 * time.Hour)
		made, err := e.m.Backup(t.Context(), "tester", 7)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			keep = int64(made[0].ID)
		}
	}
	d, err := e.m.DeleteOlderThan(t.Context(), "admin", 7, day.Add(48*time.Hour))
	if err != nil || d.Runs != 2 || d.Newest || d.Remaining != 1 {
		t.Fatalf("%+v %v", d, err)
	}
	left, _ := e.m.List(t.Context(), 7)
	for _, b := range left {
		if _, _, err := e.m.Open(t.Context(), b.ID); err != nil {
			t.Errorf("kept backup unreadable: %v", err)
		}
		if int64(b.ID) < keep {
			t.Errorf("old backup %d survived", b.ID)
		}
	}
	if ents, _ := os.ReadDir(e.dir + "/backups/plug"); len(ents) != len(left) {
		t.Errorf("%d files on disk for %d index rows", len(ents), len(left))
	}
}
