package firmware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hellwand/tasmofleet/internal/fwselect"
)

// stepUpstream is a loopback fake of both sources after the release that follows the settings-migration step
// (#892): the release directories of ota.tasmota.com and the newest GitHub releases list 16.1.0 only, and
// 16.0.0 is left in its archive directories and under its GitHub tag.
type stepUpstream struct {
	t       *testing.T
	gh, ota *httptest.Server
	mu      sync.Mutex
	hits    map[string]int
	// pinned overrides the archive listings by path; a missing key serves the real shape of 16.0.0.
	pinned map[string]string
	// tagDown makes the GitHub tag of the step release answer 404.
	tagDown bool
}

func listing(ver string, files ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<html><head><title>Tasmota Binaries</title></head><body><h2>Release binaries for Tasmota firmware %s Theo</h2><table>", ver)
	for _, f := range files {
		fmt.Fprintf(&b, "<tr><td><a href=%s>%s</a></td></tr>", f, f)
	}
	return b.String() + "</table></body></html>"
}

func ghRel(base, ver string, files ...string) string {
	var assets []string
	for _, f := range files {
		assets = append(assets, fmt.Sprintf(`{"name":%q,"browser_download_url":"%s/dl/v%s/%s"}`, f, base, ver, f))
	}
	return fmt.Sprintf(`{"tag_name":"v%s","draft":false,"prerelease":false,"assets":[%s]}`, ver, strings.Join(assets, ","))
}

func newStepUpstream(t *testing.T) *stepUpstream {
	t.Helper()
	u := &stepUpstream{t: t, hits: map[string]int{}, pinned: map[string]string{}}
	u.gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hit("gh" + r.URL.Path)
		switch {
		case r.URL.Path == "/repos/arendst/Tasmota/releases":
			_, _ = fmt.Fprintf(w, "[%s]", ghRel(u.gh.URL, "16.1.0", "tasmota.bin.gz", "tasmota32.bin"))
		case r.URL.Path == "/repos/arendst/Tasmota/releases/tags/v16.0.0" && u.tagDown:
			http.NotFound(w, r)
		case r.URL.Path == "/repos/arendst/Tasmota/releases/tags/v16.0.0":
			_, _ = w.Write([]byte(ghRel(u.gh.URL, "16.0.0", "tasmota.bin.gz", "tasmota-minimal.bin.gz", "tasmota32.bin", "tasmota32c3.bin")))
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			_, _ = w.Write(imageBytes(t, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], nil))
		default:
			http.NotFound(w, r)
		}
	}))
	u.ota = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hit("ota" + r.URL.Path)
		u.mu.Lock()
		over, ok := u.pinned[r.URL.Path]
		u.mu.Unlock()
		switch {
		case ok && over == "":
			http.NotFound(w, r)
		case ok:
			_, _ = w.Write([]byte(over))
		case r.URL.Path == "/tasmota/release/":
			_, _ = w.Write([]byte(listing("16.1.0", "tasmota.bin.gz")))
		case r.URL.Path == "/tasmota32/release/":
			_, _ = w.Write([]byte(listing("16.1.0", "tasmota32.bin")))
		case r.URL.Path == "/tasmota/release-16.0.0/":
			_, _ = w.Write([]byte(listing("16.0.0", "tasmota.bin.gz", "tasmota-minimal.bin.gz")))
		case r.URL.Path == "/tasmota32/release-16.0.0/":
			_, _ = w.Write([]byte(listing("16.0.0", "tasmota32.bin")))
		case strings.HasSuffix(r.URL.Path, ".bin"), strings.HasSuffix(r.URL.Path, ".bin.gz"):
			_, _ = w.Write(imageBytes(t, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], nil))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.gh.Close)
	t.Cleanup(u.ota.Close)
	return u
}

func (u *stepUpstream) hit(p string) {
	u.mu.Lock()
	u.hits[p]++
	u.mu.Unlock()
}

func (u *stepUpstream) count(p string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[p]
}

// cache is a cache over both fake sources with the default cross-check: a file only one source offers is refused.
func (u *stepUpstream) cache(t *testing.T) Cache {
	t.Helper()
	fake := &upstream{gh: u.gh, ota: u.ota}
	c, _, _ := newTestCache(t, fake, func(o *Options) {
		o.Sources = []Source{fake.githubSource(), fake.otaSource()}
		o.CrossCheck = true
	})
	return c
}

// Why (#892): an old device is offered 16.0.0 as its step once 16.1 is out. With the default sources and the
// cross-check, the step only works if both sources still list 16.0.0 after it left the release directory and
// the newest GitHub releases; otherwise Ensure refuses it (not in the index, or offered by one source only)
// exactly when the step becomes necessary.
func TestStepReleaseStaysInTheIndex(t *testing.T) {
	u := newStepUpstream(t)
	c := u.cache(t)
	ctx := context.Background()
	for _, name := range []string{"tasmota.bin.gz", "tasmota-minimal.bin.gz", "tasmota32.bin"} {
		a, err := c.Ensure(ctx, fwselect.StepRelease, name)
		if err != nil {
			t.Fatalf("Ensure %s/%s: %v", fwselect.StepRelease, name, err)
		}
		if a.Release != fwselect.StepRelease {
			t.Errorf("artifact release %q", a.Release)
		}
	}
	if _, err := c.Ensure(ctx, "16.1.0", "tasmota.bin.gz"); err != nil {
		t.Fatalf("the current release must stay available: %v", err)
	}
	if u.count("ota/tasmota/release-16.0.0/tasmota.bin.gz") != 1 || u.count("gh/dl/v16.0.0/tasmota.bin.gz") != 1 {
		t.Errorf("16.0.0 was not fetched from both sources: %v", u.hits)
	}
}

// Why: while the release directory and the GitHub list still carry the step release, nothing extra is asked
// for (no request per index refresh for nothing), and an archive directory that is down or lists another release
// must neither break the index of the source nor put files of another release under 16.0.0.
func TestStepReleaseArchiveFailures(t *testing.T) {
	t.Run("archive down keeps the current release", func(t *testing.T) {
		u := newStepUpstream(t)
		u.pinned["/tasmota/release-16.0.0/"] = ""
		u.pinned["/tasmota32/release-16.0.0/"] = ""
		c := u.cache(t)
		if _, err := c.Ensure(context.Background(), "16.1.0", "tasmota.bin.gz"); err != nil {
			t.Fatalf("a missing archive broke the source: %v", err)
		}
		if _, err := c.Ensure(context.Background(), fwselect.StepRelease, "tasmota.bin.gz"); !errors.Is(err, ErrNoCrossCheck) {
			t.Fatalf("step from GitHub alone: %v, want ErrNoCrossCheck (fail loud, no silent single source)", err)
		}
	})
	t.Run("github tag down keeps the current release", func(t *testing.T) {
		u := newStepUpstream(t)
		u.tagDown = true
		c := u.cache(t)
		if _, err := c.Ensure(context.Background(), "16.1.0", "tasmota.bin.gz"); err != nil {
			t.Fatalf("a missing tag broke the source: %v", err)
		}
		if _, err := c.Ensure(context.Background(), fwselect.StepRelease, "tasmota.bin.gz"); !errors.Is(err, ErrNoCrossCheck) {
			t.Fatalf("step from ota.tasmota.com alone: %v, want ErrNoCrossCheck", err)
		}
	})
	t.Run("archive listing another release", func(t *testing.T) {
		u := newStepUpstream(t)
		u.pinned["/tasmota/release-16.0.0/"] = listing("16.1.0", "tasmota.bin.gz")
		c := u.cache(t)
		if _, err := c.Ensure(context.Background(), fwselect.StepRelease, "tasmota.bin.gz"); !errors.Is(err, ErrNoCrossCheck) {
			t.Fatalf("Ensure: %v, want ErrNoCrossCheck: a listing of 16.1.0 must not count as 16.0.0", err)
		}
	})
	t.Run("esp32 file in the esp8266 archive", func(t *testing.T) {
		u := newStepUpstream(t)
		u.pinned["/tasmota/release-16.0.0/"] = listing("16.0.0", "tasmota.bin.gz", "tasmota32c3.bin")
		u.pinned["/tasmota32/release-16.0.0/"] = listing("16.0.0", "tasmota32.bin")
		c := u.cache(t)
		// GitHub offers it too, so only the directory rule keeps the cross-check from passing.
		if _, err := c.Ensure(context.Background(), fwselect.StepRelease, "tasmota32c3.bin"); !errors.Is(err, ErrNoCrossCheck) {
			t.Fatalf("Ensure: %v, want ErrNoCrossCheck: a tasmota32 file listed in the ESP8266 directory must not count", err)
		}
	})
	t.Run("step still current", func(t *testing.T) {
		u := newStepUpstream(t)
		u.pinned["/tasmota/release/"] = listing("16.0.0", "tasmota.bin.gz")
		u.pinned["/tasmota32/release/"] = listing("16.0.0", "tasmota32.bin")
		c := u.cache(t)
		if _, err := c.Ensure(context.Background(), fwselect.StepRelease, "tasmota.bin.gz"); err != nil {
			t.Fatal(err)
		}
		if n := u.count("ota/tasmota/release-16.0.0/") + u.count("ota/tasmota32/release-16.0.0/"); n != 0 {
			t.Errorf("archive read %d times while the release directory is the step release", n)
		}
	})
}

// Why: GitHub is asked for the step release's tag only when the newest releases no longer include it; until
// then every index refresh would cost one more API request (GitHub rate-limits anonymous clients).
func TestStepReleaseTagOnlyWhenNeeded(t *testing.T) {
	u := newUpstream(t) // its GitHub list still carries v16.0.0
	c, _, _ := newTestCache(t, u, nil)
	if _, err := c.Ensure(context.Background(), fwselect.StepRelease, "tasmota.bin.gz"); err != nil {
		t.Fatal(err)
	}
	if n := u.count("gh/repos/arendst/Tasmota/releases/tags/v" + fwselect.StepRelease); n != 0 {
		t.Errorf("tag requested %d times although the list has it", n)
	}
}
