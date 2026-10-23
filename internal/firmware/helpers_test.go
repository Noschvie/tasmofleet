package firmware

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// memStore is a consumer-side fake of store.ArtifactStore.
type memStore struct {
	mu     sync.Mutex
	next   model.ArtifactID
	byID   map[model.ArtifactID]model.Artifact
	putErr error
}

func newMemStore() *memStore { return &memStore{byID: map[model.ArtifactID]model.Artifact{}} }

func (m *memStore) PutArtifact(_ context.Context, a model.Artifact) (model.ArtifactID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		return 0, m.putErr
	}
	m.next++
	a.ID = m.next
	m.byID[a.ID] = a
	return a.ID, nil
}

func (m *memStore) GetArtifact(_ context.Context, id model.ArtifactID) (model.Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.byID[id]
	if !ok {
		return model.Artifact{}, store.ErrNotFound
	}
	return a, nil
}

func (m *memStore) FindArtifact(_ context.Context, release, filename string) (model.Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.byID {
		if a.Release == release && a.Filename == filename && a.Custom == "" {
			return a, nil
		}
	}
	return model.Artifact{}, store.ErrNotFound
}

func (m *memStore) ListArtifacts(context.Context) ([]model.Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.Artifact
	for _, a := range m.byID {
		out = append(out, a)
	}
	return out, nil
}

func (m *memStore) DeleteArtifact(_ context.Context, id model.ArtifactID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byID[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.byID, id)
	return nil
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) After(time.Duration) <-chan time.Time { return nil }

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testInspect mirrors the documented header rules of fwselect.InspectImage
// (spec 7.4) so this package's tests do not depend on fwselect's progress.
func testInspect(h []byte) (model.ChipFamily, error) {
	if len(h) < 16 || h[0] != 0xE9 {
		return model.ChipUnknown, fmt.Errorf("bad magic")
	}
	if h[8] != 0xEE {
		return model.ChipESP8266, nil
	}
	switch id := int(h[12]) | int(h[13])<<8; id {
	case 0x0000:
		return model.ChipESP32, nil
	case 0x0002:
		return model.ChipESP32S2, nil
	case 0x0005:
		return model.ChipESP32C3, nil
	case 0x0009:
		return model.ChipESP32S3, nil
	default:
		return model.ChipUnknown, fmt.Errorf("unknown chip id %#x", id)
	}
}

// headerFixture maps an upstream file name to the captured real header of
// the same family (testdata/fixtures/firmware-headers).
func headerFixture(t testing.TB, name string) []byte {
	t.Helper()
	base := name
	switch {
	case strings.HasPrefix(name, "tasmota32"):
		base = name
	case strings.HasSuffix(name, ".bin.gz"):
		base = "tasmota.bin.gz" // all ESP8266 variants share the header
	}
	if name == "tasmota32-sensors.bin" {
		base = "tasmota32.bin"
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "firmware-headers", base+".hdr"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// imageBytes builds a file as upstream would serve it for name, with the
// header taken from headerFixture unless override is given.
func imageBytes(t testing.TB, name string, override []byte) []byte {
	t.Helper()
	hdr := override
	if hdr == nil {
		hdr = headerFixture(t, name)
	}
	body := append(append([]byte{}, hdr...), bytes.Repeat([]byte{0xAA}, 2000)...)
	if !strings.HasSuffix(name, ".gz") {
		return body
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// upstream is a loopback fake of the GitHub API and ota.tasmota.com. Files
// are served from files[release+"/"+name]; absent keys are synthesised from
// the header fixtures. Hits counts requests per path.
type upstream struct {
	t       *testing.T
	gh, ota *httptest.Server
	mu      sync.Mutex
	hits    map[string]int
	files   map[string][]byte // key "<version>/<name>", github and ota both
	otaOnly map[string][]byte // overrides for the ota host only
	slow    chan struct{}     // when non-nil, file downloads block until closed
	redir   string            // when set, github downloads redirect here
	down    bool              // when set, every request answers 503
	bodies  map[string]string // release body by tag name, injected into the GitHub index
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{t: t, hits: map[string]int{}, files: map[string][]byte{}, otaOnly: map[string][]byte{}}
	u.gh = httptest.NewServer(http.HandlerFunc(u.serveGH))
	u.ota = httptest.NewServer(http.HandlerFunc(u.serveOTA))
	t.Cleanup(u.gh.Close)
	t.Cleanup(u.ota.Close)
	return u
}

func (u *upstream) setDown(v bool) {
	u.mu.Lock()
	u.down = v
	u.mu.Unlock()
}

func (u *upstream) isDown() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.down
}

func (u *upstream) hit(p string) {
	u.mu.Lock()
	u.hits[p]++
	u.mu.Unlock()
}

func (u *upstream) count(p string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[p]
}

func readTestdata(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (u *upstream) serveGH(w http.ResponseWriter, r *http.Request) {
	u.hit("gh" + r.URL.Path)
	if u.isDown() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.URL.Path == "/repos/arendst/Tasmota/releases":
		body := strings.ReplaceAll(readTestdata(u.t, "github-releases.json"), "{{BASE}}", u.gh.URL)
		if len(u.bodies) > 0 {
			var rels []map[string]any
			if err := json.Unmarshal([]byte(body), &rels); err != nil {
				u.t.Fatal(err)
			}
			for _, r := range rels {
				if b, ok := u.bodies[r["tag_name"].(string)]; ok {
					r["body"], r["html_url"] = b, "https://github.com/arendst/Tasmota/releases/tag/"+r["tag_name"].(string)
				}
			}
			out, err := json.Marshal(rels)
			if err != nil {
				u.t.Fatal(err)
			}
			body = string(out)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		if u.redir != "" {
			http.Redirect(w, r, u.redir+r.URL.Path, http.StatusFound)
			return
		}
		u.serveFile(w, strings.TrimPrefix(r.URL.Path, "/dl/v"), u.files)
	default:
		http.NotFound(w, r)
	}
}

func (u *upstream) serveOTA(w http.ResponseWriter, r *http.Request) {
	u.hit("ota" + r.URL.Path)
	switch {
	case r.URL.Path == "/tasmota/release/":
		_, _ = w.Write([]byte(readTestdata(u.t, "ota-tasmota-release.html")))
	case r.URL.Path == "/tasmota32/release/":
		_, _ = w.Write([]byte(readTestdata(u.t, "ota-tasmota32-release.html")))
	case strings.HasPrefix(r.URL.Path, "/tasmota/release/"), strings.HasPrefix(r.URL.Path, "/tasmota32/release/"):
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if b, ok := u.otaOnly["16.0.0/"+name]; ok {
			_, _ = w.Write(b)
			return
		}
		u.serveFile(w, "16.0.0/"+name, u.files)
	default:
		http.NotFound(w, r)
	}
}

func (u *upstream) serveFile(w http.ResponseWriter, key string, files map[string][]byte) {
	if u.slow != nil {
		<-u.slow
	}
	b, ok := files[key]
	if !ok {
		name := key[strings.Index(key, "/")+1:]
		b = imageBytes(u.t, name, nil)
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	_, _ = w.Write(b)
}

func (u *upstream) githubSource() Source {
	return Source{Name: "github", Kind: "github-releases", BaseURL: u.gh.URL + "/repos/arendst/Tasmota",
		AllowedHosts: []string{hostOf(u.gh.URL)}, AllowHTTP: true}
}

func (u *upstream) otaSource() Source {
	return Source{Name: "ota.tasmota.com", Kind: "ota-tasmota", BaseURL: u.ota.URL,
		AllowedHosts: []string{hostOf(u.ota.URL)}, AllowHTTP: true}
}

func hostOf(rawURL string) string { return strings.TrimPrefix(rawURL, "http://") }

// newTestCache builds a cache over the fake upstream.
func newTestCache(t *testing.T, u *upstream, mutate func(*Options)) (Cache, *memStore, Options) {
	t.Helper()
	st := newMemStore()
	o := Options{
		Dir:        t.TempDir(),
		Sources:    []Source{u.githubSource()},
		BaseURL:    "http://172.31.12.61",
		Inspect:    testInspect,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Logger:     quietLog(),
	}
	if mutate != nil {
		mutate(&o)
	}
	c, err := New(o, st)
	if err != nil {
		t.Fatal(err)
	}
	return c, st, o
}

// httptestServer starts a loopback server that calls onHit for any request.
func httptestServer(t *testing.T, onHit func()) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { onHit() }))
	t.Cleanup(s.Close)
	return s.URL
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
