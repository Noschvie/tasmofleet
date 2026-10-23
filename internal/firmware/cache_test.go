package firmware

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func versions(rs []Release) []string {
	var v []string
	for _, r := range rs {
		v = append(v, r.Version)
	}
	return v
}

func TestReleasesGitHub(t *testing.T) {
	u := newUpstream(t)
	c, _, _ := newTestCache(t, u, nil)
	rs, err := c.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Drafts and pre-releases must never be offered: a flash of an rc build to
	// the whole fleet is not something the update dialog may suggest.
	if got := strings.Join(versions(rs), ","); got != "16.0.0,15.3.0" {
		t.Fatalf("versions = %s", got)
	}
	var files []string
	files = append(files, rs[0].Files...)
	joined := strings.Join(files, " ")
	if !strings.Contains(joined, "tasmota-minimal.bin.gz") || !strings.Contains(joined, "tasmota32c3.bin") {
		t.Fatalf("files = %v", files)
	}
	if strings.Contains(joined, ".map.gz") {
		t.Fatalf("non-firmware assets must not be listed: %v", files)
	}
}

func TestReleasesOTAListing(t *testing.T) {
	u := newUpstream(t)
	c, _, _ := newTestCache(t, u, func(o *Options) { o.Sources = []Source{u.otaSource()} })
	rs, err := c.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Version != "16.0.0" {
		t.Fatalf("releases = %+v", rs)
	}
	joined := strings.Join(rs[0].Files, " ")
	// Both directories are merged, and only file names are taken from hrefs:
	// the external link and the sort/parent links in the listing are ignored.
	for _, want := range []string{"tasmota.bin.gz", "tasmota32c3.bin"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %v", want, rs[0].Files)
		}
	}
	for _, bad := range []string{"evil", "..", "?C="} {
		if strings.Contains(joined, bad) {
			t.Errorf("unexpected %q in %v", bad, rs[0].Files)
		}
	}
}

// Spec 8.1: the index is cached for 15 minutes and never fetched on render.
func TestReleasesAreCachedFor15Minutes(t *testing.T) {
	u := newUpstream(t)
	clk := newFakeClock()
	c, _, _ := newTestCache(t, u, func(o *Options) { o.Clock = clk })
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.Releases(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := u.count("gh/repos/arendst/Tasmota/releases"); n != 1 {
		t.Fatalf("index fetched %d times within TTL, want 1", n)
	}
	clk.Advance(14 * time.Minute)
	if _, err := c.Releases(ctx); err != nil {
		t.Fatal(err)
	}
	if n := u.count("gh/repos/arendst/Tasmota/releases"); n != 1 {
		t.Fatalf("index refetched before TTL: %d", n)
	}
	clk.Advance(2 * time.Minute)
	if _, err := c.Releases(ctx); err != nil {
		t.Fatal(err)
	}
	if n := u.count("gh/repos/arendst/Tasmota/releases"); n != 2 {
		t.Fatalf("index not refetched after TTL: %d", n)
	}
	// Explicit refresh bypasses the TTL.
	if err := c.(interface{ Refresh(context.Context) error }).Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if n := u.count("gh/repos/arendst/Tasmota/releases"); n != 3 {
		t.Fatalf("Refresh did not refetch: %d", n)
	}
}

// Why (design v2, N6/N7): the release page and the dashboard counts show the
// feed on every view. They read the index already in memory and must never
// cause a request upstream, not even when the index is past its TTL; before
// the first fetch there is simply nothing.
func TestCachedReleasesNeverFetch(t *testing.T) {
	u := newUpstream(t)
	clk := newFakeClock()
	c, _, _ := newTestCache(t, u, func(o *Options) { o.Clock = clk })
	cl := c.(CachedLister)
	if got := cl.CachedReleases(); got != nil {
		t.Fatalf("before the first fetch = %v", got)
	}
	if n := u.count("gh/repos/arendst/Tasmota/releases"); n != 0 {
		t.Fatalf("CachedReleases fetched the index %d times", n)
	}
	want, err := c.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour) // far past the TTL
	got := cl.CachedReleases()
	if len(got) == 0 || len(got) != len(want) || got[0].Version != want[0].Version || got[0].Notes != want[0].Notes {
		t.Fatalf("cached = %d releases, want the %d of the index", len(got), len(want))
	}
	if n := u.count("gh/repos/arendst/Tasmota/releases"); n != 1 {
		t.Fatalf("index fetched %d times, want only the one Releases call", n)
	}
}

func TestEnsureDownloadsIndexesAndReuses(t *testing.T) {
	u := newUpstream(t)
	c, st, o := newTestCache(t, u, nil)
	ctx := context.Background()
	a, err := c.Ensure(ctx, "16.0.0", "tasmota32c3.bin")
	if err != nil {
		t.Fatal(err)
	}
	if a.Family != model.ChipESP32C3 || a.Variant != "tasmota" || a.Release != "16.0.0" || a.Source != "github" || a.Size == 0 || len(a.SHA256) != 64 || a.ID == 0 || a.Safeboot {
		t.Fatalf("artifact = %+v", a)
	}
	p := filepath.Join(o.Dir, "16.0.0", "tasmota32c3.bin")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// The file is served to devices, so it must be world readable (spec 5).
	if fi.Mode().Perm() != 0o644 || fi.Size() != a.Size {
		t.Fatalf("mode %v size %d, artifact size %d", fi.Mode(), fi.Size(), a.Size)
	}
	entries, _ := os.ReadDir(filepath.Join(o.Dir, "16.0.0"))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	// Second call: no download.
	before := u.count("gh/dl/v16.0.0/tasmota32c3.bin")
	a2, err := c.Ensure(ctx, "16.0.0", "tasmota32c3.bin")
	if err != nil || a2.ID != a.ID {
		t.Fatalf("second Ensure: %+v %v", a2, err)
	}
	if u.count("gh/dl/v16.0.0/tasmota32c3.bin") != before {
		t.Fatal("cached artifact was downloaded again; artifacts are immutable")
	}
	if all, _ := st.ListArtifacts(ctx); len(all) != 1 {
		t.Fatalf("index rows = %d", len(all))
	}
}

// An index row whose file vanished must self-heal rather than serve a hole.
func TestEnsureRedownloadsWhenFileMissing(t *testing.T) {
	u := newUpstream(t)
	c, st, o := newTestCache(t, u, nil)
	ctx := context.Background()
	if _, err := c.Ensure(ctx, "16.0.0", "tasmota-minimal.bin.gz"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(o.Dir, "16.0.0", "tasmota-minimal.bin.gz")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ensure(ctx, "16.0.0", "tasmota-minimal.bin.gz"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "16.0.0", "tasmota-minimal.bin.gz")); err != nil {
		t.Fatal(err)
	}
	if all, _ := st.ListArtifacts(ctx); len(all) != 1 {
		t.Fatalf("stale row not replaced: %d rows", len(all))
	}
}

func TestEnsureSafebootIsFlagged(t *testing.T) {
	u := newUpstream(t)
	c, _, _ := newTestCache(t, u, nil)
	a, err := c.Ensure(context.Background(), "16.0.0", "tasmota32-safeboot.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Safeboot {
		t.Fatal("safeboot flag must be set so fwselect.Guard can refuse it")
	}
}

// Acceptance: a file whose header family differs from its name is rejected,
// deleted and not indexed (an ESP32 image behind an ESP8266 name would brick
// a plug).
func TestEnsureRejectsHeaderFamilyMismatch(t *testing.T) {
	cases := []struct{ name, headerOf string }{
		{"tasmota-sensors.bin.gz", "tasmota32.bin"},
		{"tasmota32c3.bin", "tasmota32.bin"},
		{"tasmota32.bin", "tasmota32c3.bin"},
		{"tasmota32.bin", "tasmota.bin.gz"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"<-"+tc.headerOf, func(t *testing.T) {
			u := newUpstream(t)
			hdr := headerFixture(t, tc.headerOf)
			u.files["16.0.0/"+tc.name] = imageBytes(t, tc.name, hdr)
			c, st, o := newTestCache(t, u, nil)
			_, err := c.Ensure(context.Background(), "16.0.0", tc.name)
			if !errors.Is(err, ErrImageMismatch) {
				t.Fatalf("err = %v, want ErrImageMismatch", err)
			}
			if all, _ := st.ListArtifacts(context.Background()); len(all) != 0 {
				t.Fatalf("rejected file was indexed: %+v", all)
			}
			assertNoFiles(t, o.Dir)
		})
	}
}

func assertNoFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("file left on disk: %s", p)
		}
		return nil
	})
}

func TestEnsureRejectsBadImages(t *testing.T) {
	u := newUpstream(t)
	u.files["16.0.0/tasmota.bin.gz"] = []byte("<html>not gzip, maybe a captive portal</html>")
	u.files["16.0.0/tasmota32.bin"] = []byte("garbage that is long enough to look like a header..........")
	c, st, o := newTestCache(t, u, nil)
	for _, f := range []string{"tasmota.bin.gz", "tasmota32.bin"} {
		if _, err := c.Ensure(context.Background(), "16.0.0", f); !errors.Is(err, ErrImageMismatch) {
			t.Errorf("%s: err = %v, want ErrImageMismatch", f, err)
		}
	}
	if all, _ := st.ListArtifacts(context.Background()); len(all) != 0 {
		t.Fatalf("indexed: %+v", all)
	}
	assertNoFiles(t, o.Dir)
}

func TestEnsureSizeLimit(t *testing.T) {
	u := newUpstream(t)
	c, st, o := newTestCache(t, u, func(o *Options) { o.MaxSize = 500 })
	_, err := c.Ensure(context.Background(), "16.0.0", "tasmota32.bin")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if all, _ := st.ListArtifacts(context.Background()); len(all) != 0 {
		t.Fatal("indexed oversized file")
	}
	assertNoFiles(t, o.Dir)
}

// A server that lies about (or omits) Content-Length must still be bounded.
func TestEnsureSizeLimitWithoutContentLength(t *testing.T) {
	u := newUpstream(t)
	big := imageBytes(t, "tasmota32.bin", nil)
	u.files["16.0.0/tasmota32.bin"] = append(big, make([]byte, 100000)...)
	u.gh.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			w.Header().Set("Transfer-Encoding", "chunked")
			_, _ = w.Write(u.files["16.0.0/tasmota32.bin"])
			return
		}
		u.serveGH(w, r)
	})
	c, _, o := newTestCache(t, u, func(o *Options) { o.MaxSize = 50000 })
	if _, err := c.Ensure(context.Background(), "16.0.0", "tasmota32.bin"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	assertNoFiles(t, o.Dir)
}

// Acceptance: a redirect to a non-allowlisted host aborts (SSRF).
func TestEnsureRedirectToForeignHostAborts(t *testing.T) {
	u := newUpstream(t)
	u.redir = u.ota.URL // a live host, but not on the github allowlist
	c, st, o := newTestCache(t, u, nil)
	_, err := c.Ensure(context.Background(), "16.0.0", "tasmota32.bin")
	if !errors.Is(err, ErrSourceNotAllowed) {
		t.Fatalf("err = %v, want ErrSourceNotAllowed", err)
	}
	if n := u.count("ota/dl/v16.0.0/tasmota32.bin"); n != 0 {
		t.Fatalf("redirect target was contacted %d times", n)
	}
	if all, _ := st.ListArtifacts(context.Background()); len(all) != 0 {
		t.Fatal("indexed")
	}
	assertNoFiles(t, o.Dir)
}

func TestRedirectWithinAllowlistIsFollowed(t *testing.T) {
	u := newUpstream(t)
	u.gh.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			http.Redirect(w, r, "/dl2"+strings.TrimPrefix(r.URL.Path, "/dl"), http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/dl2/"):
			u.serveFile(w, strings.TrimPrefix(r.URL.Path, "/dl2/v"), u.files)
		default:
			u.serveGH(w, r)
		}
	})
	c, _, _ := newTestCache(t, u, nil)
	if _, err := c.Ensure(context.Background(), "16.0.0", "tasmota32.bin"); err != nil {
		t.Fatal(err)
	}
}

// Download URLs come from upstream JSON; a poisoned asset URL pointing at an
// internal address must be refused before any request is made.
func TestEnsureRefusesAssetURLOutsideAllowlist(t *testing.T) {
	u := newUpstream(t)
	hit := false
	internal := httptestServer(t, func() { hit = true })
	u.gh.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/arendst/Tasmota/releases" {
			_, _ = w.Write([]byte(`[{"tag_name":"v16.0.0","assets":[{"name":"tasmota32.bin","browser_download_url":"` + internal + `/x"}]}]`))
			return
		}
		u.serveGH(w, r)
	})
	c, _, _ := newTestCache(t, u, nil)
	_, err := c.Ensure(context.Background(), "16.0.0", "tasmota32.bin")
	if !errors.Is(err, ErrSourceNotAllowed) || hit {
		t.Fatalf("err = %v, internal host contacted = %v", err, hit)
	}
}

// http is only for sources that are explicitly configured for it (ota.tasmota.com).
func TestHTTPOnlyWhereConfigured(t *testing.T) {
	u := newUpstream(t)
	c, _, _ := newTestCache(t, u, func(o *Options) {
		s := u.githubSource()
		s.AllowHTTP = false
		o.Sources = []Source{s}
	})
	_, err := c.Releases(context.Background())
	if !errors.Is(err, ErrSourceNotAllowed) {
		t.Fatalf("err = %v, want ErrSourceNotAllowed", err)
	}
}

// Spec 8.1: with both sources every file is fetched from both and the
// SHA-256 must match.
func TestCrossCheck(t *testing.T) {
	both := func(o *Options, u *upstream) {
		o.Sources = []Source{u.githubSource(), u.otaSource()}
		o.CrossCheck = true
	}
	t.Run("match", func(t *testing.T) {
		u := newUpstream(t)
		c, _, _ := newTestCache(t, u, func(o *Options) { both(o, u) })
		a, err := c.Ensure(context.Background(), "16.0.0", "tasmota32.bin")
		if err != nil {
			t.Fatal(err)
		}
		if u.count("gh/dl/v16.0.0/tasmota32.bin") != 1 || u.count("ota/tasmota32/release/tasmota32.bin") != 1 {
			t.Fatal("file was not fetched from both sources")
		}
		if a.Source != "github" {
			t.Fatalf("source = %q, want primary", a.Source)
		}
	})
	t.Run("mismatch rejects", func(t *testing.T) {
		u := newUpstream(t)
		tampered := append(imageBytes(t, "tasmota32.bin", nil), 'X')
		u.otaOnly["16.0.0/tasmota32.bin"] = tampered
		c, st, o := newTestCache(t, u, func(o *Options) { both(o, u) })
		_, err := c.Ensure(context.Background(), "16.0.0", "tasmota32.bin")
		if !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("err = %v, want ErrChecksumMismatch", err)
		}
		if all, _ := st.ListArtifacts(context.Background()); len(all) != 0 {
			t.Fatal("tampered file indexed")
		}
		assertNoFiles(t, o.Dir)
	})
	t.Run("release only on one source is refused by default", func(t *testing.T) {
		// 15.3.0 exists on GitHub only. Plain HTTP mirrors and DNS tricks make a
		// single unverifiable source the way a tampered image would arrive, so
		// without an explicit operator opt-in nothing is downloaded.
		u := newUpstream(t)
		c, st, o := newTestCache(t, u, func(o *Options) { both(o, u) })
		_, err := c.Ensure(context.Background(), "15.3.0", "tasmota32.bin")
		if !errors.Is(err, ErrNoCrossCheck) {
			t.Fatalf("err = %v, want ErrNoCrossCheck", err)
		}
		if all, _ := st.ListArtifacts(context.Background()); len(all) != 0 {
			t.Fatal("unverified file indexed")
		}
		assertNoFiles(t, o.Dir)
	})
	t.Run("release only on one source with explicit opt-in", func(t *testing.T) {
		u := newUpstream(t)
		c, _, _ := newTestCache(t, u, func(o *Options) { both(o, u); o.AllowSingleSource = true })
		a, err := c.Ensure(context.Background(), "15.3.0", "tasmota32.bin")
		if err != nil || a.Source != "github" {
			t.Fatalf("a=%+v err=%v", a, err)
		}
	})
}

func TestEnsureUnknownReleaseOrFile(t *testing.T) {
	u := newUpstream(t)
	c, _, _ := newTestCache(t, u, nil)
	ctx := context.Background()
	if _, err := c.Ensure(ctx, "9.9.9", "tasmota32.bin"); !errors.Is(err, ErrNotInIndex) {
		t.Errorf("unknown release: %v", err)
	}
	if _, err := c.Ensure(ctx, "16.0.0", "tasmota-nonexistent.bin.gz"); !errors.Is(err, ErrNotInIndex) {
		t.Errorf("unknown file: %v", err)
	}
	if _, err := c.Ensure(ctx, "16.0.0", "../../etc/passwd"); !errors.Is(err, ErrBadName) {
		t.Errorf("traversal file: %v", err)
	}
	if _, err := c.Ensure(ctx, "../x", "tasmota32.bin"); !errors.Is(err, ErrBadName) {
		t.Errorf("traversal release: %v", err)
	}
}

// Concurrent Ensure calls for one file (two jobs needing the same minimal
// image) must download once and not corrupt the file.
func TestEnsureSingleFlight(t *testing.T) {
	u := newUpstream(t)
	c, st, _ := newTestCache(t, u, nil)
	var wg sync.WaitGroup
	ids := make([]model.ArtifactID, 6)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := c.Ensure(context.Background(), "16.0.0", "tasmota-minimal.bin.gz")
			if err != nil {
				t.Error(err)
			}
			ids[i] = a.ID
		}()
	}
	wg.Wait()
	if n := u.count("gh/dl/v16.0.0/tasmota-minimal.bin.gz"); n != 1 {
		t.Fatalf("downloaded %d times", n)
	}
	if all, _ := st.ListArtifacts(context.Background()); len(all) != 1 {
		t.Fatalf("rows = %d", len(all))
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("ids differ: %v", ids)
		}
	}
}

func TestEnsureContextCancelStopsDownload(t *testing.T) {
	u := newUpstream(t)
	u.slow = make(chan struct{})
	defer close(u.slow)
	c, _, o := newTestCache(t, u, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := c.Ensure(ctx, "16.0.0", "tasmota32.bin"); err == nil {
		t.Fatal("expected error")
	}
	assertNoFiles(t, o.Dir)
}

func TestURLFor(t *testing.T) {
	u := newUpstream(t)
	c, _, _ := newTestCache(t, u, func(o *Options) { o.BaseURL = "http://172.31.12.61/" })
	got := c.URLFor(model.Artifact{Release: "16.0.0", Filename: "tasmota-minimal.bin.gz"})
	// The Guard compares this exact string with the OtaUrl, so a double slash
	// from a trailing slash in the config would make every job refuse.
	if got != "http://172.31.12.61/fw/16.0.0/tasmota-minimal.bin.gz" {
		t.Fatalf("URLFor = %q", got)
	}
}

func TestNewValidatesBaseURL(t *testing.T) {
	for _, bad := range []string{"", "https://172.31.12.61", "172.31.12.61", "http://", "ftp://x"} {
		_, err := New(Options{Dir: t.TempDir(), BaseURL: bad, Inspect: testInspect}, newMemStore())
		if err == nil {
			t.Errorf("BaseURL %q accepted; devices cannot do TLS OTA", bad)
		}
	}
	if _, err := New(Options{Dir: t.TempDir(), BaseURL: "http://x"}, newMemStore()); err == nil {
		t.Error("missing Inspect accepted; the header check must not be optional")
	}
}

func TestNewRemovesStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.Join(dir, "16.0.0")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{".tmp-123", "tasmota.bin.gz"} {
		if err := os.WriteFile(filepath.Join(rel, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(Options{Dir: dir, BaseURL: "http://x", Inspect: testInspect}, newMemStore()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rel, ".tmp-123")); !os.IsNotExist(err) {
		t.Fatal("crash leftover not removed")
	}
	if _, err := os.Stat(filepath.Join(rel, "tasmota.bin.gz")); err != nil {
		t.Fatal("real file removed")
	}
}

func TestPrune(t *testing.T) {
	u := newUpstream(t)
	inUse := map[model.ArtifactID]bool{}
	c, st, o := newTestCache(t, u, func(o *Options) {
		o.InUse = func(context.Context) (map[model.ArtifactID]bool, error) { return inUse, nil }
	})
	ctx := context.Background()
	// Seed releases directly (same shape Ensure produces) to avoid downloads.
	seed := func(rel, name string) model.Artifact {
		if err := os.MkdirAll(filepath.Join(o.Dir, rel), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(o.Dir, rel, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		a := model.Artifact{Release: rel, Filename: name, Size: 1}
		id, err := st.PutArtifact(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		a.ID = id
		return a
	}
	var old model.Artifact
	for _, rel := range []string{"15.9.0", "15.10.0", "16.0.0", "14.0.0"} {
		a := seed(rel, "tasmota.bin.gz")
		seed(rel, "tasmota-minimal.bin.gz")
		if rel == "14.0.0" {
			old = a
		}
	}
	inUse[old.ID] = true // an active job still needs 14.0.0
	n, err := c.Prune(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 { // 15.9.0: two artifacts; 14.0.0 is protected
		t.Fatalf("pruned %d, want 2", n)
	}
	for rel, want := range map[string]bool{"16.0.0": true, "15.10.0": true, "14.0.0": true, "15.9.0": false} {
		_, err := os.Stat(filepath.Join(o.Dir, rel, "tasmota.bin.gz"))
		if (err == nil) != want {
			t.Errorf("release %s present=%v want %v", rel, err == nil, want)
		}
		_, e2 := st.FindArtifact(ctx, rel, "tasmota-minimal.bin.gz")
		if (e2 == nil) != want {
			t.Errorf("release %s index present=%v want %v", rel, e2 == nil, want)
		}
	}
	// Once the job is gone the old release can go too.
	delete(inUse, old.ID)
	if n, err := c.Prune(ctx, 2); err != nil || n != 2 {
		t.Fatalf("second prune: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "14.0.0")); !os.IsNotExist(err) {
		t.Fatal("empty release directory not removed")
	}
	if _, err := c.Prune(ctx, -1); err == nil {
		t.Fatal("negative keep accepted")
	}
}

// If the in-use lookup fails, deleting anyway could pull a file out from
// under a running OTA, so Prune must delete nothing.
func TestPruneFailsClosedWhenInUseFails(t *testing.T) {
	u := newUpstream(t)
	c, st, o := newTestCache(t, u, func(o *Options) {
		o.InUse = func(context.Context) (map[model.ArtifactID]bool, error) { return nil, errors.New("db down") }
	})
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(o.Dir, "1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutArtifact(ctx, model.Artifact{Release: "1.0.0", Filename: "tasmota.bin.gz"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Prune(ctx, 0); err == nil {
		t.Fatal("expected error")
	}
	if all, _ := st.ListArtifacts(ctx); len(all) != 1 {
		t.Fatal("artifact deleted despite failed in-use lookup")
	}
}
