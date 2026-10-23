package firmware

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// seedArtifact indexes a file the way Ensure leaves it, without a download.
func seedArtifact(t *testing.T, st *memStore, dir, rel, name string, size int64) model.Artifact {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, rel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rel, name), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	a := model.Artifact{Release: rel, Filename: name, Size: size}
	id, err := st.PutArtifact(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	a.ID = id
	return a
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func managerOf(t *testing.T, c Cache) Manager {
	t.Helper()
	m, ok := c.(Manager)
	if !ok {
		t.Fatal("cache does not implement Manager")
	}
	return m
}

// Why: deleting a file an active or queued OTA job still has to serve would
// leave a device mid-update with a 404. The refusal must name the jobs so the
// operator knows what to wait for or cancel, and must leave file and row alone.
func TestDeleteRefusedWhileInUseNamesTheJobs(t *testing.T) {
	u := newUpstream(t)
	var used map[model.ArtifactID][]model.JobID
	c, st, o := newTestCache(t, u, func(o *Options) {
		o.InUseBy = func(context.Context) (map[model.ArtifactID][]model.JobID, error) { return used, nil }
	})
	target := seedArtifact(t, st, o.Dir, "16.0.0", "tasmota.bin.gz", 10)
	minimal := seedArtifact(t, st, o.Dir, "16.0.0", "tasmota-minimal.bin.gz", 5)
	used = map[model.ArtifactID][]model.JobID{target.ID: {7}, minimal.ID: {9, 7}} // minimal artifact of a staged job counts too
	m := managerOf(t, c)

	for _, a := range []model.Artifact{target, minimal} {
		_, err := m.Delete(context.Background(), a.ID)
		var inUse *InUseError
		if !errors.Is(err, ErrInUse) || !errors.As(err, &inUse) || len(inUse.Jobs) == 0 {
			t.Fatalf("Delete(%s) = %v, want an InUseError with jobs", a.Filename, err)
		}
		if !fileExists(filepath.Join(o.Dir, a.Release, a.Filename)) {
			t.Errorf("%s was removed despite the refusal", a.Filename)
		}
		if _, err := st.GetArtifact(context.Background(), a.ID); err != nil {
			t.Errorf("index row of %s was removed despite the refusal", a.Filename)
		}
	}
	_, err := m.Delete(context.Background(), minimal.ID)
	if got := err.Error(); got != "firmware: 16.0.0/tasmota-minimal.bin.gz is in use by job 7, 9" {
		t.Errorf("message = %q", got)
	}

	used = nil // the jobs ended
	if _, err := m.Delete(context.Background(), target.ID); err != nil {
		t.Fatalf("delete after the job ended: %v", err)
	}
	if fileExists(filepath.Join(o.Dir, "16.0.0", "tasmota.bin.gz")) {
		t.Error("file still there after delete")
	}
	if _, err := st.GetArtifact(context.Background(), target.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("row still there after delete: %v", err)
	}
}

// Why: if we cannot tell whether a job needs the file, deleting it is a guess.
func TestDeleteFailsClosedWhenInUseLookupFails(t *testing.T) {
	u := newUpstream(t)
	c, st, o := newTestCache(t, u, func(o *Options) {
		o.InUseBy = func(context.Context) (map[model.ArtifactID][]model.JobID, error) { return nil, errors.New("db down") }
	})
	a := seedArtifact(t, st, o.Dir, "16.0.0", "tasmota.bin.gz", 10)
	m := managerOf(t, c)
	if _, err := m.Delete(context.Background(), a.ID); err == nil || errors.Is(err, ErrInUse) {
		t.Fatalf("Delete = %v, want a lookup error", err)
	}
	if !fileExists(filepath.Join(o.Dir, "16.0.0", "tasmota.bin.gz")) {
		t.Fatal("file removed although the lookup failed")
	}
	if _, err := m.DeleteUnused(context.Background()); err == nil {
		t.Fatal("DeleteUnused ignored the failed lookup")
	}
	if all, _ := st.ListArtifacts(context.Background()); len(all) != 1 {
		t.Fatal("DeleteUnused deleted something although the lookup failed")
	}
}

// Why: "delete all unused" is a bulk action, so it has to keep exactly what a
// job needs and remove the rest, including the then-empty release directory.
func TestDeleteUnusedKeepsWhatJobsNeed(t *testing.T) {
	u := newUpstream(t)
	var held model.ArtifactID
	c, st, o := newTestCache(t, u, func(o *Options) {
		o.InUseBy = func(context.Context) (map[model.ArtifactID][]model.JobID, error) {
			return map[model.ArtifactID][]model.JobID{held: {3}}, nil
		}
	})
	keep := seedArtifact(t, st, o.Dir, "16.0.0", "tasmota.bin.gz", 100)
	held = keep.ID
	seedArtifact(t, st, o.Dir, "16.0.0", "tasmota32.bin", 200)
	seedArtifact(t, st, o.Dir, "15.0.0", "tasmota.bin.gz", 50)

	res, err := managerOf(t, c).DeleteUnused(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 2 || res.Freed != 250 || res.Kept != 1 {
		t.Fatalf("result = %+v", res)
	}
	if !fileExists(filepath.Join(o.Dir, "16.0.0", "tasmota.bin.gz")) {
		t.Error("the file a job needs was deleted")
	}
	if fileExists(filepath.Join(o.Dir, "15.0.0")) {
		t.Error("empty release directory left behind")
	}
	if all, _ := st.ListArtifacts(context.Background()); len(all) != 1 {
		t.Errorf("%d rows left, want 1", len(all))
	}
}

// Why: the legacy InUse option (bool map) must keep protecting files; only the
// job names are missing then.
func TestDeleteHonoursLegacyInUse(t *testing.T) {
	u := newUpstream(t)
	var id model.ArtifactID
	c, st, o := newTestCache(t, u, func(o *Options) {
		o.InUse = func(context.Context) (map[model.ArtifactID]bool, error) {
			return map[model.ArtifactID]bool{id: true}, nil
		}
	})
	id = seedArtifact(t, st, o.Dir, "16.0.0", "tasmota.bin.gz", 1).ID
	if _, err := managerOf(t, c).Delete(context.Background(), id); !errors.Is(err, ErrInUse) {
		t.Fatalf("Delete = %v", err)
	}
}

func TestDeleteUnknownArtifact(t *testing.T) {
	u := newUpstream(t)
	c, _, _ := newTestCache(t, u, nil)
	if _, err := managerOf(t, c).Delete(context.Background(), 4711); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Delete = %v", err)
	}
}
