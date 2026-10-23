package firmware

import (
	"context"
	"strings"
	"testing"
	"time"
)

func waitRun(t *testing.T, p *Prefetcher, id string) PrefetchRun {
	t.Helper()
	done := make(chan struct{})
	go func() { p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("prefetch did not finish")
	}
	r, ok := p.Get(id)
	if !ok || r.Running() {
		t.Fatalf("run %s: ok=%v running=%v", id, ok, r.Running())
	}
	return r
}

// Why: Prefetch must go through Ensure, so every file is downloaded, verified
// and indexed exactly like one an OTA job would fetch. A second run must not
// download again.
func TestPrefetchDownloadsAndReportsPerFile(t *testing.T) {
	u := newUpstream(t)
	c, st, _ := newTestCache(t, u, nil)
	p := NewPrefetcher(c, nil)
	files := []string{"tasmota.bin.gz", "tasmota-minimal.bin.gz", "tasmota32c3.bin"}

	run, started, err := p.Start(context.Background(), "adam", "16.0.0", files)
	if err != nil || !started {
		t.Fatalf("start: %v started=%v", err, started)
	}
	r := waitRun(t, p, run.ID)
	if done, failed, total := r.Counts(); done != 3 || failed != 0 || total != 3 {
		t.Fatalf("counts %d/%d/%d: %+v", done, failed, total, r.Items)
	}
	if all, _ := st.ListArtifacts(context.Background()); len(all) != 3 {
		t.Fatalf("%d artifacts indexed", len(all))
	}
	hits := u.count("gh/dl/v16.0.0/tasmota.bin.gz")

	run2, _, _ := p.Start(context.Background(), "adam", "16.0.0", files)
	r2 := waitRun(t, p, run2.ID)
	for _, it := range r2.Items {
		if !it.Already || it.State != PrefetchCached {
			t.Errorf("second run item %+v, want already cached", it)
		}
	}
	if u.count("gh/dl/v16.0.0/tasmota.bin.gz") != hits {
		t.Error("a cached file was downloaded again")
	}
}

// Why: the cross-check is a security rule; prefetching must not be a way around
// it. A tampered mirror yields a failed item with the reason, and the other
// files still arrive.
func TestPrefetchKeepsCrossCheckAndContinuesAfterAFailure(t *testing.T) {
	u := newUpstream(t)
	u.otaOnly["16.0.0/tasmota32.bin"] = append(imageBytes(t, "tasmota32.bin", nil), 'X')
	c, st, _ := newTestCache(t, u, func(o *Options) { o.Sources = []Source{u.githubSource(), u.otaSource()}; o.CrossCheck = true })
	p := NewPrefetcher(c, nil)
	run, _, _ := p.Start(context.Background(), "adam", "16.0.0", []string{"tasmota32.bin", "tasmota.bin.gz"})
	r := waitRun(t, p, run.ID)
	if r.Items[0].State != PrefetchFailed || !strings.Contains(r.Items[0].Err, "SHA-256") {
		t.Errorf("tampered file: %+v", r.Items[0])
	}
	if r.Items[1].State != PrefetchCached {
		t.Errorf("good file after the failure: %+v", r.Items[1])
	}
	if all, _ := st.ListArtifacts(context.Background()); len(all) != 1 {
		t.Errorf("%d artifacts indexed, want only the good one", len(all))
	}
}

// Why: two clicks (or two admins) must not start two parallel downloads of the
// same set; the second caller attaches to the running one.
func TestPrefetchAllowsOneActiveRun(t *testing.T) {
	u := newUpstream(t)
	u.slow = make(chan struct{})
	c, _, _ := newTestCache(t, u, nil)
	p := NewPrefetcher(c, nil)
	first, started, err := p.Start(context.Background(), "a", "16.0.0", []string{"tasmota.bin.gz"})
	if err != nil || !started {
		t.Fatal(err, started)
	}
	again, started, err := p.Start(context.Background(), "b", "16.0.0", []string{"tasmota32.bin"})
	if err != nil || started || again.ID != first.ID {
		t.Fatalf("second start: id %s started=%v err=%v, want the running %s", again.ID, started, err, first.ID)
	}
	if a, ok := p.Active(); !ok || a.ID != first.ID || !a.Running() {
		t.Fatalf("active = %+v", a)
	}
	close(u.slow)
	waitRun(t, p, first.ID)
	if _, ok := p.Active(); ok {
		t.Error("still active after the run ended")
	}
	if _, _, err := p.Start(context.Background(), "a", "16.0.0", nil); err == nil {
		t.Error("an empty file list was accepted")
	}
}

// Why: a file that is not in the release index is reported per file, never
// silently skipped or invented.
func TestPrefetchReportsFileNotInIndex(t *testing.T) {
	u := newUpstream(t)
	c, _, _ := newTestCache(t, u, nil)
	p := NewPrefetcher(c, nil)
	run, _, _ := p.Start(context.Background(), "a", "16.0.0", []string{"tasmota-doesnotexist.bin.gz", "../../etc/passwd"})
	r := waitRun(t, p, run.ID)
	for _, it := range r.Items {
		if it.State != PrefetchFailed || it.Err == "" {
			t.Errorf("%+v", it)
		}
	}
}

// Why: release notes are shown on the firmware page; they must arrive with the
// index, be bounded, and carry only a safe release link.
func TestReleasesCarryNotesFromTheGitHubIndex(t *testing.T) {
	u := newUpstream(t)
	u.bodies = map[string]string{"v16.0.0": "## Added\n- thing\n", "v15.3.0": strings.Repeat("x", MaxNotesBytes+500)}
	c, _, _ := newTestCache(t, u, nil)
	rs, err := c.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Version != "16.0.0" || rs[0].Notes != "## Added\n- thing\n" || rs[0].NotesTruncated {
		t.Errorf("16.0.0: %+v", rs[0])
	}
	if !strings.HasPrefix(rs[0].URL, "https://github.com/") {
		t.Errorf("release url = %q", rs[0].URL)
	}
	if len(rs[1].Notes) != MaxNotesBytes || !rs[1].NotesTruncated {
		t.Errorf("15.3.0 notes: len %d truncated %v", len(rs[1].Notes), rs[1].NotesTruncated)
	}
}

func TestReleasesWithoutNotesSourceHaveNone(t *testing.T) {
	u := newUpstream(t)
	c, _, _ := newTestCache(t, u, func(o *Options) { o.Sources = []Source{u.otaSource()} })
	rs, err := c.Releases(context.Background())
	if err != nil || len(rs) == 0 || rs[0].Notes != "" || rs[0].URL != "" {
		t.Fatalf("%v %+v", err, rs)
	}
}
