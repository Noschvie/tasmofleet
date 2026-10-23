package firmware

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// memCustomStore adds the custom metadata of store.CustomFirmwareStore to the
// consumer-side artifact fake.
type memCustomStore struct {
	*memStore
	cmu   sync.Mutex
	metas []store.CustomMeta
}

func (m *memCustomStore) PutCustomMeta(_ context.Context, c store.CustomMeta) error {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	m.metas = append([]store.CustomMeta{c}, m.metas...)
	return nil
}

func (m *memCustomStore) ListCustomMeta(context.Context) ([]store.CustomMeta, error) {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	return append([]store.CustomMeta(nil), m.metas...), nil
}

func (m *memCustomStore) FindCustomArtifact(_ context.Context, key, release, filename string) (model.Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.byID {
		if a.Custom == key && a.Release == release && a.Filename == filename {
			return a, nil
		}
	}
	return model.Artifact{}, store.ErrNotFound
}

func newCustomCache(t *testing.T, u *upstream) (Cache, *memCustomStore, Options) {
	t.Helper()
	st := &memCustomStore{memStore: newMemStore()}
	o := Options{Dir: t.TempDir(), BaseURL: "http://172.31.12.61", Inspect: testInspect,
		HTTPClient: &http.Client{Timeout: 5 * time.Second}, Logger: quietLog(), Clock: newFakeClock()}
	if u != nil {
		o.Sources = []Source{u.githubSource()}
	}
	c, err := New(o, st)
	if err != nil {
		t.Fatal(err)
	}
	return c, st, o
}

func customOf(t *testing.T, c Cache) Custom {
	t.Helper()
	cu, ok := c.(Custom)
	if !ok {
		t.Fatal("cache does not implement Custom")
	}
	return cu
}

// rawImage is an uncompressed image: a real captured header plus filler.
func rawImage(t testing.TB, hdrFile string, extra int) []byte {
	t.Helper()
	return append(append([]byte{}, headerFixture(t, hdrFile)...), bytes.Repeat([]byte{0x55}, extra)...)
}

func gz(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gunzip(t testing.TB, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("stored file is not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func upload(body []byte, name, variant string) CustomInput {
	return CustomInput{Body: bytes.NewReader(body), FileName: name, Label: "test build", Version: "16.0.0.9", Variant: variant, Actor: "admin"}
}

// Why: the family decides which devices may ever receive the image, so it must
// come from the bytes (never the name), and the stored name and container must
// be exactly what fwselect.Guard expects for that family (ESP8266 .bin.gz,
// ESP32 .bin) whatever container the admin uploaded.
func TestAddCustomDetectsFamilyAndNormalisesContainer(t *testing.T) {
	esp8266 := rawImage(t, "tasmota.bin.gz", 3000)
	tests := []struct {
		name     string
		body     []byte
		fileName string
		variant  string
		single   bool
		family   model.ChipFamily
		stored   string
		wantRaw  []byte
	}{
		{"esp8266 raw", esp8266, "mine.bin", "", false, model.ChipESP8266, "tasmota.bin.gz", esp8266},
		{"esp8266 gz", gz(t, esp8266), "mine.bin.gz", "", false, model.ChipESP8266, "tasmota.bin.gz", esp8266},
		{"esp8266 gz named .bin", gz(t, esp8266), "mine.bin", "", false, model.ChipESP8266, "tasmota.bin.gz", esp8266},
		{"esp8266 sensors", esp8266, "x.bin", "sensors", false, model.ChipESP8266, "tasmota-sensors.bin.gz", esp8266},
		{"esp32", rawImage(t, "tasmota32.bin", 3000), "x.bin", "", false, model.ChipESP32, "tasmota32.bin", rawImage(t, "tasmota32.bin", 3000)},
		{"esp32 gz is unpacked", gz(t, rawImage(t, "tasmota32.bin", 3000)), "x.bin.gz", "", false, model.ChipESP32, "tasmota32.bin", rawImage(t, "tasmota32.bin", 3000)},
		{"esp32 bluetooth", rawImage(t, "tasmota32.bin", 3000), "x.bin", "bluetooth", false, model.ChipESP32, "tasmota32-bluetooth.bin", rawImage(t, "tasmota32.bin", 3000)},
		{"esp32 solo1", rawImage(t, "tasmota32.bin", 3000), "x.bin", "", true, model.ChipESP32, "tasmota32solo1.bin", rawImage(t, "tasmota32.bin", 3000)},
		{"esp32c3", rawImage(t, "tasmota32c3.bin", 3000), "x.bin", "", false, model.ChipESP32C3, "tasmota32c3.bin", rawImage(t, "tasmota32c3.bin", 3000)},
		{"esp32s3", rawImage(t, "tasmota32s3.bin", 3000), "x.bin", "", false, model.ChipESP32S3, "tasmota32s3.bin", rawImage(t, "tasmota32s3.bin", 3000)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, st, o := newCustomCache(t, nil)
			in := upload(tc.body, tc.fileName, tc.variant)
			in.SingleCore = tc.single
			got, err := customOf(t, c).AddCustom(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			a := got.Artifact
			if a.Family != tc.family || a.Filename != tc.stored || a.Source != CustomSource || a.Release != "16.0.0.9" {
				t.Fatalf("artifact = %+v", a)
			}
			disk, err := os.ReadFile(filepath.Join(o.Dir, "custom", a.Custom, "16.0.0.9", tc.stored))
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(disk)) != a.Size {
				t.Errorf("size %d, file has %d bytes", a.Size, len(disk))
			}
			content := disk
			if strings.HasSuffix(tc.stored, ".gz") {
				content = gunzip(t, disk)
			}
			if !bytes.Equal(content, tc.wantRaw) {
				t.Error("stored content differs from the uploaded image")
			}
			if len(st.metas) != 1 || st.metas[0].Label != "test build" || st.metas[0].UploadedBy != "admin" || st.metas[0].ArtifactID != a.ID {
				t.Errorf("metadata = %+v", st.metas)
			}
		})
	}
}

// Why: a wrong or hostile upload must be refused before anything is stored:
// no artifact row, no metadata, no file, no leftover directory or temp file.
func TestAddCustomRefusals(t *testing.T) {
	good := rawImage(t, "tasmota.bin.gz", 3000)
	fullFlash := append(rawImage(t, "tasmota32c3.bin", 0), make([]byte, 0x8000)...)
	fullFlash[0x8000], fullFlash[0x8001] = 0xAA, 0x50
	fullFlash = append(fullFlash, make([]byte, 2000)...)
	badChip := rawImage(t, "tasmota32.bin", 3000)
	badChip[12], badChip[13] = 0x77, 0 // chip_id the spec does not know
	tests := []struct {
		name    string
		in      CustomInput
		wantErr error
	}{
		{"safeboot by file name", upload(rawImage(t, "tasmota32-safeboot.bin", 3000), "tasmota32-safeboot.bin", ""), ErrCustomImage},
		{"factory image by file name", upload(rawImage(t, "tasmota32.bin", 3000), "tasmota32.factory.bin", ""), ErrCustomImage},
		{"safeboot as variant", upload(rawImage(t, "tasmota32.bin", 3000), "x.bin", "safeboot"), ErrCustomImage},
		{"full flash image (partition table)", upload(fullFlash, "x.bin", ""), ErrCustomImage},
		{"unknown chip id", upload(badChip, "x.bin", ""), ErrCustomImage},
		{"garbage", upload(bytes.Repeat([]byte("not firmware "), 400), "x.bin", ""), ErrCustomImage},
		{"garbage in gzip", upload(gz(t, bytes.Repeat([]byte("not firmware "), 400)), "x.bin.gz", ""), ErrCustomImage},
		{"renamed text file", upload([]byte("hello"), "tasmota.bin.gz", ""), ErrCustomImage},
		{"empty", upload(nil, "x.bin", ""), ErrCustomImage},
		{"truncated gzip", upload(gz(t, good)[:60], "x.bin.gz", ""), ErrCustomImage},
		{"too small", upload(headerFixture(t, "tasmota.bin.gz"), "x.bin", ""), ErrCustomImage},
		{"esp8266 oversize (gzip bomb)", upload(gz(t, append(rawImage(t, "tasmota.bin.gz", 0), make([]byte, MaxCustomESP8266)...)), "x.bin.gz", ""), ErrTooLarge},
		{"esp32 oversize", upload(append(rawImage(t, "tasmota32.bin", 0), make([]byte, MaxCustomESP32)...), "x.bin", ""), ErrTooLarge},
		{"esp32 variant on esp8266 build name", upload(good, "x.bin", "bluetooth"), ErrCustomImage},
		{"esp8266 variant on esp32 image", upload(rawImage(t, "tasmota32.bin", 3000), "x.bin", "sensors"), ErrCustomImage},
		{"path in variant", upload(good, "x.bin", "../../etc/passwd"), ErrCustomImage},
		{"empty label", func() CustomInput { i := upload(good, "x.bin", ""); i.Label = " "; return i }(), ErrCustomImage},
		{"control character in label", func() CustomInput { i := upload(good, "x.bin", ""); i.Label = "a\nb"; return i }(), ErrCustomImage},
		{"non numeric version", func() CustomInput { i := upload(good, "x.bin", ""); i.Version = "16.0.0-dev"; return i }(), ErrCustomImage},
		{"path as version", func() CustomInput { i := upload(good, "x.bin", ""); i.Version = "../1"; return i }(), ErrCustomImage},
		{"empty version", func() CustomInput { i := upload(good, "x.bin", ""); i.Version = ""; return i }(), ErrCustomImage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, st, o := newCustomCache(t, nil)
			_, err := customOf(t, c).AddCustom(context.Background(), tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			arts, _ := st.ListArtifacts(context.Background())
			if len(arts) != 0 || len(st.metas) != 0 {
				t.Errorf("a refused upload left rows: %v %v", arts, st.metas)
			}
			var left []string
			_ = filepath.WalkDir(o.Dir, func(p string, d os.DirEntry, _ error) error {
				if !d.IsDir() {
					left = append(left, p)
				}
				return nil
			})
			if len(left) != 0 {
				t.Errorf("a refused upload left files: %v", left)
			}
		})
	}
}

// Why: the uploaded file name is attacker-controlled text. It must never
// reach the file system: the stored path comes from the family and variant only.
func TestAddCustomIgnoresTheUploadedFileNameForThePath(t *testing.T) {
	c, _, o := newCustomCache(t, nil)
	got, err := customOf(t, c).AddCustom(context.Background(), upload(rawImage(t, "tasmota.bin.gz", 3000), "../../../etc/cron.d/evil\x00.bin", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got.OriginalName != "evil.bin" || strings.ContainsAny(got.OriginalName, "/\\\x00") {
		t.Errorf("display name = %q", got.OriginalName)
	}
	if got.Artifact.Filename != "tasmota.bin.gz" {
		t.Errorf("stored as %q", got.Artifact.Filename)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "custom", got.Artifact.Custom, "16.0.0.9", "tasmota.bin.gz")); err != nil {
		t.Fatal(err)
	}
}

// Why: a self-built image usually reports the version of the release it was
// built from. It must therefore coexist with the upstream file of the same
// version and name: separate rows, separate files, and each is served only at
// its own URL. Only the very same bytes are refused (a double click).
func TestCustomImageCoexistsWithUpstreamOfTheSameVersion(t *testing.T) {
	u := newUpstream(t)
	c, st, o := newCustomCache(t, u)
	ctx := context.Background()
	up, err := c.Ensure(ctx, "16.0.0", "tasmota.bin.gz")
	if err != nil {
		t.Fatal(err)
	}
	mine := rawImage(t, "tasmota.bin.gz", 3000)
	in := upload(mine, "x.bin", "")
	in.Version = "16.0.0"
	cu := customOf(t, c)
	got, err := cu.AddCustom(ctx, in)
	if err != nil {
		t.Fatalf("same version as upstream refused: %v", err)
	}
	cust := got.Artifact
	if cust.ID == up.ID || cust.Release != "16.0.0" || cust.Filename != up.Filename || cust.Custom == "" || cust.SHA256 == up.SHA256 {
		t.Fatalf("upstream %+v, custom %+v", up, cust)
	}
	if c.URLFor(cust) == c.URLFor(up) {
		t.Fatalf("both files share the URL %s", c.URLFor(up))
	}
	if !strings.Contains(c.URLFor(cust), "/c/"+cust.Custom+"/fw/16.0.0/tasmota.bin.gz") {
		t.Errorf("custom URL = %s", c.URLFor(cust))
	}
	// The upstream lookups never return the upload, and the other way round.
	if a, err := c.Ensure(ctx, "16.0.0", "tasmota.bin.gz"); err != nil || a.ID != up.ID {
		t.Errorf("upstream Ensure returned %+v, %v", a, err)
	}
	if a, err := customOf(t, c).EnsureCustom(ctx, cust.Custom, "16.0.0", "tasmota.bin.gz"); err != nil || a.ID != cust.ID {
		t.Errorf("EnsureCustom returned %+v, %v", a, err)
	}
	if _, err := customOf(t, c).EnsureCustom(ctx, "0000000000", "16.0.0", "tasmota.bin.gz"); !errors.Is(err, ErrNotInIndex) {
		t.Errorf("an unknown key must not fall back to upstream: %v", err)
	}
	// What a device downloads at each URL.
	body := func(url string) []byte {
		rec := httptest.NewRecorder()
		Server(c).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", url, rec.Code)
		}
		return rec.Body.Bytes()
	}
	diskUp, _ := os.ReadFile(filepath.Join(o.Dir, "16.0.0", "tasmota.bin.gz"))
	if !bytes.Equal(body("/fw/16.0.0/tasmota.bin.gz"), diskUp) {
		t.Error("the upstream URL does not serve the upstream file")
	}
	if !bytes.Equal(gunzip(t, body("/c/"+cust.Custom+"/fw/16.0.0/tasmota.bin.gz")), mine) {
		t.Error("the custom URL does not serve the upload")
	}
	// A wrong key, or a custom key on the plain path, serves nothing.
	for _, url := range []string{"/c/0000000000/fw/16.0.0/tasmota.bin.gz", "/c/" + cust.Custom + "/fw/16.0.0/tasmota-sensors.bin.gz", "/c/" + cust.Custom + "/fw/16.0.1/tasmota.bin.gz", "/c/../fw/16.0.0/tasmota.bin.gz"} {
		rec := httptest.NewRecorder()
		Server(c).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", url, rec.Code)
		}
	}
	// The same bytes again are refused; the same version with other bytes is not.
	if _, err := cu.AddCustom(ctx, in2(mine, "16.0.0")); !errors.Is(err, ErrCustomImage) {
		t.Errorf("identical upload: %v", err)
	}
	if _, err := cu.AddCustom(ctx, in2(rawImage(t, "tasmota.bin.gz", 3500), "16.0.0")); err != nil {
		t.Errorf("same version, other bytes: %v", err)
	}
	if arts, _ := st.ListArtifacts(ctx); len(arts) != 3 {
		t.Errorf("%d artifacts", len(arts))
	}
}

func in2(body []byte, version string) CustomInput {
	in := upload(body, "x.bin", "")
	in.Version = version
	return in
}

// Why: the device-facing server must serve an upload exactly like a cached
// upstream file (same URL scheme, same size check), or the OTA would 404.
func TestCustomImageIsServedToDevices(t *testing.T) {
	c, _, _ := newCustomCache(t, nil)
	got, err := customOf(t, c).AddCustom(context.Background(), upload(rawImage(t, "tasmota.bin.gz", 3000), "x.bin", ""))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	Server(c).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/c/"+got.Artifact.Custom+"/fw/16.0.0.9/tasmota.bin.gz", nil))
	if rec.Code != http.StatusOK || int64(rec.Body.Len()) != got.Artifact.Size {
		t.Fatalf("status %d, %d bytes, want %d", rec.Code, rec.Body.Len(), got.Artifact.Size)
	}
	if len(gunzip(t, rec.Body.Bytes())) == 0 {
		t.Error("served file is empty")
	}
}

// Why: the chip-family guard must treat an upload like any upstream file. An
// ESP32 image can never be sent to an ESP8266 device, even when its release is
// chosen, and the matching device is accepted for the plain variant only.
func TestGuardAppliesToCustomImages(t *testing.T) {
	c, _, _ := newCustomCache(t, nil)
	cu := customOf(t, c)
	esp32, err := cu.AddCustom(context.Background(), upload(rawImage(t, "tasmota32.bin", 3000), "x.bin", ""))
	if err != nil {
		t.Fatal(err)
	}
	in8266 := upload(rawImage(t, "tasmota.bin.gz", 3000), "x.bin", "")
	in8266.Version = "16.0.0.10"
	esp8266, err := cu.AddCustom(context.Background(), in8266)
	if err != nil {
		t.Fatal(err)
	}
	dev8266 := model.Identity{Family: model.ChipESP8266, Version: "15.0.0", Variant: "tasmota"}
	choice := fwselect.Choice{Release: "16.0.0.9"}
	if err := fwselect.Guard(dev8266, esp32.Artifact, c.URLFor(esp32.Artifact), choice); !errors.Is(err, fwselect.ErrFamilyMismatch) {
		t.Fatalf("ESP32 image for an ESP8266 device: %v", err)
	}
	choice.Release = "16.0.0.10"
	if err := fwselect.Guard(dev8266, esp8266.Artifact, c.URLFor(esp8266.Artifact), choice); err != nil {
		t.Fatalf("matching custom image refused: %v", err)
	}
	sensors := dev8266
	sensors.Variant = "sensors" // a variant change needs the explicit choice, custom or not
	if err := fwselect.Guard(sensors, esp8266.Artifact, c.URLFor(esp8266.Artifact), choice); !errors.Is(err, fwselect.ErrVariantChange) {
		t.Fatalf("variant change without choice: %v", err)
	}
}

// Why: Tasmota stages ESP8266 updates through minimal firmware, whose URL
// lives next to the target. A custom release must therefore get a minimal
// image, copied from upstream (never an unchecked file), or the staged update
// has nothing to flash first.
func TestCustomReleaseGetsAMinimalImage(t *testing.T) {
	u := newUpstream(t)
	c, st, o := newCustomCache(t, u)
	ctx := context.Background()
	img, err := customOf(t, c).AddCustom(ctx, upload(rawImage(t, "tasmota.bin.gz", 3000), "x.bin", ""))
	if err != nil {
		t.Fatal(err)
	}
	key := img.Artifact.Custom
	m, err := customOf(t, c).EnsureCustom(ctx, key, "16.0.0.9", "tasmota-minimal.bin.gz")
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != customMinimalSource || m.Family != model.ChipESP8266 || m.Release != "16.0.0.9" {
		t.Fatalf("minimal = %+v", m)
	}
	up, err := st.FindArtifact(ctx, "16.0.0", "tasmota-minimal.bin.gz") // fetched through the normal path
	if err != nil || up.SHA256 != m.SHA256 {
		t.Fatalf("upstream minimal %+v, %v", up, err)
	}
	if m.Custom != key || !fileExists(filepath.Join(o.Dir, "custom", key, "16.0.0.9", "tasmota-minimal.bin.gz")) {
		t.Error("minimal copy missing on disk")
	}
	again, err := customOf(t, c).EnsureCustom(ctx, key, "16.0.0.9", "tasmota-minimal.bin.gz")
	if err != nil || again.ID != m.ID {
		t.Fatalf("second Ensure: %+v, %v", again, err)
	}
	if err := fwselect.Guard(model.Identity{Family: model.ChipESP8266, Version: "15.0.0", Variant: "tasmota"}, m, c.URLFor(m), fwselect.Choice{Release: "16.0.0.9"}); err != nil {
		t.Errorf("guard refuses the minimal stage: %v", err)
	}
}

// Why: an upstream release must never inherit the borrowed minimal logic. Only
// releases created by an upload may be asked for a minimal image that no source lists.
func TestUnknownReleaseGetsNoMinimal(t *testing.T) {
	u := newUpstream(t)
	c, _, _ := newCustomCache(t, u)
	if _, err := c.Ensure(context.Background(), "99.0.0", "tasmota-minimal.bin.gz"); !errors.Is(err, ErrNotInIndex) {
		t.Fatalf("err = %v", err)
	}
}

// Why: deleting a custom image behaves like deleting a cached file (refused
// while a job needs it, also through its borrowed minimal image), takes the
// minimal copy with it, and "delete unused" / prune never remove an upload
// the admin did not ask to delete.
func TestCustomDeleteAndRetention(t *testing.T) {
	u := newUpstream(t)
	var used map[model.ArtifactID][]model.JobID
	st := &memCustomStore{memStore: newMemStore()}
	o := Options{Dir: t.TempDir(), BaseURL: "http://172.31.12.61", Inspect: testInspect, Logger: quietLog(), Sources: []Source{u.githubSource()},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		InUseBy:    func(context.Context) (map[model.ArtifactID][]model.JobID, error) { return used, nil }}
	c, err := New(o, st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	img, err := customOf(t, c).AddCustom(ctx, upload(rawImage(t, "tasmota.bin.gz", 3000), "x.bin", ""))
	if err != nil {
		t.Fatal(err)
	}
	min, err := customOf(t, c).EnsureCustom(ctx, img.Artifact.Custom, "16.0.0.9", "tasmota-minimal.bin.gz")
	if err != nil {
		t.Fatal(err)
	}
	m := managerOf(t, c)

	if res, err := m.DeleteUnused(ctx); err != nil || len(res.Deleted) != 1 { // the upstream minimal only
		t.Fatalf("DeleteUnused = %+v, %v", res, err)
	}
	if n, err := c.Prune(ctx, 0); err != nil || n != 0 {
		t.Fatalf("Prune removed %d, %v", n, err)
	}
	if _, err := customOf(t, c).EnsureCustom(ctx, img.Artifact.Custom, "16.0.0.9", "tasmota.bin.gz"); err != nil {
		t.Fatalf("upload gone after cleanup: %v", err)
	}

	used = map[model.ArtifactID][]model.JobID{min.ID: {4}} // a staged job holds only the minimal copy
	if _, err := m.Delete(ctx, img.Artifact.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete while the minimal copy is needed: %v", err)
	}
	if !fileExists(filepath.Join(o.Dir, "custom", img.Artifact.Custom, "16.0.0.9", "tasmota.bin.gz")) {
		t.Fatal("refused delete removed the file")
	}
	used = nil
	if _, err := m.Delete(ctx, img.Artifact.ID); err != nil {
		t.Fatal(err)
	}
	arts, _ := st.ListArtifacts(ctx)
	for _, a := range arts {
		if a.Release == "16.0.0.9" {
			t.Errorf("left behind: %+v", a)
		}
	}
	if fileExists(filepath.Join(o.Dir, "custom", img.Artifact.Custom)) {
		t.Error("upload directory left behind")
	}
}
