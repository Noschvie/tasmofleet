package tasmota

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

const simFixtures = "../../testdata/fixtures"

// uploadSim is a dual-OTA ESP32: the only layout that takes a plain upload into the running app (#191).
func uploadSim(t *testing.T, spec simulator.DeviceSpec) (simulator.Controller, Target) {
	t.Helper()
	f, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: simFixtures}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	spec.Name, spec.Profile = "plug", simulator.ProfileESP32Dual
	if spec.Version == "" {
		spec.Version = "16.0.0"
	}
	d, err := f.Add(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := simulator.Control(d)
	tg := simTarget(t, d)
	tg.user, tg.pass = spec.User, spec.Password
	return c, tg
}

// uploadSimIn adds one device of the profile to an existing fleet.
func uploadSimIn(t *testing.T, f simulator.Fleet, name string, profile simulator.Profile) (simulator.Controller, Target) {
	t.Helper()
	d, err := f.Add(context.Background(), simulator.DeviceSpec{Name: name, Profile: profile, Version: "16.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := simulator.Control(d)
	return c, simTarget(t, d)
}

func simTarget(t *testing.T, d simulator.Device) Target {
	t.Helper()
	host, ps, _ := net.SplitHostPort(d.Addr())
	port, _ := strconv.Atoi(ps)
	return TargetFor(model.Device{ID: 7, Address: host, Port: port})
}

func simImage(t *testing.T, name, version string) []byte {
	t.Helper()
	b, err := simulator.NewImage(simFixtures, name, version)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func uploader(t *testing.T, o Options) Uploader {
	t.Helper()
	u, ok := newClient(o).(Uploader)
	if !ok {
		t.Fatal("the production client must implement Uploader")
	}
	return u
}

func upload(u Uploader, tg Target, img []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return u.UploadFirmware(ctx, tg, "tasmota32.bin", int64(len(img)), bytes.NewReader(img))
}

func waitBoots(t *testing.T, c simulator.Controller, boots int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.WaitFor(ctx, func(c simulator.Controller) bool { return c.Boots() >= boots && c.Phase() == simulator.PhaseRun }); err != nil {
		t.Fatalf("device did not reboot: %v", err)
	}
}

// Why: the happy path is what the push delivery relies on: the device takes
// the image, restarts and runs the new version.
func TestUploadFirmwareFlashesTheDevice(t *testing.T) {
	c, tg := uploadSim(t, simulator.DeviceSpec{})
	if err := upload(uploader(t, Options{}), tg, simImage(t, "tasmota32.bin", "16.1.0")); err != nil {
		t.Fatal(err)
	}
	waitBoots(t, c, 1)
	if c.Version() != "16.1.0" {
		t.Errorf("version %s", c.Version())
	}
}

// Why: the device's web password is sent to the bound target only, as Basic
// auth on both requests; a wrong one is ErrAuth and nothing is flashed.
func TestUploadFirmwareAuth(t *testing.T) {
	c, tg := uploadSim(t, simulator.DeviceSpec{Password: testPassword})
	img := simImage(t, "tasmota32.bin", "16.1.0")
	if err := upload(uploader(t, Options{}), tg, img); err != nil {
		t.Fatal(err)
	}
	waitBoots(t, c, 1)

	c2, tg2 := uploadSim(t, simulator.DeviceSpec{Password: testPassword})
	bad := TargetFor(model.Device{ID: 7, Address: tg2.host, Port: tg2.port, HTTPPassword: "wrong"})
	err := upload(uploader(t, Options{}), bad, img)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
	if strings.Contains(err.Error(), "wrong") || c2.Version() != "16.0.0" {
		t.Errorf("error %q, version %s", err, c2.Version())
	}
}

// Why: Tasmota flashes a POST /u2 only after GET /up; otherwise it answers
// "Successful" and restarts without flashing. The client must always fetch
// /up first, and must refuse (sending nothing) when the page has no plain
// upload form, as on an ESP32 with a single OTA partition.
func TestUploadFirmwareNeedsTheUploadForm(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		_, _ = w.Write([]byte("<html><form method='post' action='u4?fsz='><input type='file' name='u2'></form></html>"))
	}))
	defer srv.Close()
	err := upload(uploader(t, Options{}), target(t, srv, 1, "", ""), []byte{0xE9, 1, 2, 3})
	if !errors.Is(err, ErrUploadUnsupported) {
		t.Fatalf("got %v", err)
	}
	if posts.Load() != 0 {
		t.Error("an image was sent to a device without a plain upload form")
	}
}

// Why: #191. The running app of a single-OTA ESP32 serves an upload form whose button calls
// upl(this). A plain POST /u2 there would overwrite the running firmware, so the client sends
// nothing even when the caller (planner) could not read the layout earlier.
func TestUploadFirmwareRefusesTheSingleOTAAppPage(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		_, _ = w.Write([]byte("<html><form method='post' action='u2'><input type='file' name='u2'><button onclick='upl(this)'>Start upgrade</button></form></html>"))
	}))
	defer srv.Close()
	err := upload(uploader(t, Options{}), target(t, srv, 1, "", ""), []byte{0xE9, 1, 2, 3})
	if !errors.Is(err, ErrUploadUnsupported) {
		t.Fatalf("got %v", err)
	}
	if posts.Load() != 0 {
		t.Error("an image was sent into the running app of a single-OTA device")
	}
}

// Why: Tasmota's web server cannot read a chunked body, and fsz must be the
// file size (the device checks it against its free flash).
func TestUploadFirmwareSendsExactLengthAndFsz(t *testing.T) {
	var gotLen int64
	var gotTE []string
	var fsz string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("<form method='post' action='u2?fsz=' enctype='multipart/form-data'><input type='file' name='u2'></form>"))
			return
		}
		gotLen, gotTE, fsz = r.ContentLength, r.TransferEncoding, r.URL.Query().Get("fsz")
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("Upload <font color='#008000'>Successful</font></b>"))
	}))
	defer srv.Close()
	img := bytes.Repeat([]byte{0xE9}, 5000)
	if err := upload(uploader(t, Options{}), target(t, srv, 1, "", ""), img); err != nil {
		t.Fatal(err)
	}
	if gotLen <= int64(len(img)) || len(gotTE) != 0 || int64(len(body)) != gotLen {
		t.Errorf("length %d, transfer-encoding %v, body %d", gotLen, gotTE, len(body))
	}
	if fsz != "5000" {
		t.Errorf("fsz %q", fsz)
	}
}

// Why: a refusal is certain (nothing flashed) and carries the device's
// reason; it must be told apart from a lost reply, which is not certain.
func TestUploadFirmwareRefusedIsCertain(t *testing.T) {
	c, tg := uploadSim(t, simulator.DeviceSpec{Faults: []simulator.Fault{{Kind: simulator.FaultWrongFamily}}})
	err := upload(uploader(t, Options{}), tg, simImage(t, "tasmota32.bin", "16.1.0"))
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "Upload failed") {
		t.Fatalf("got %v", err)
	}
	if c.Version() != "16.0.0" || c.Boots() != 0 {
		t.Error("a refused image must not be flashed")
	}
}

// Why: a lost reply after a complete body is NOT a refusal: the device may be
// flashing. The error must not claim certainty, so the engine probes.
func TestUploadFirmwareLostReplyIsUncertain(t *testing.T) {
	c, tg := uploadSim(t, simulator.DeviceSpec{Faults: []simulator.Fault{{Kind: simulator.FaultUploadLostReply}}})
	err := upload(uploader(t, Options{}), tg, simImage(t, "tasmota32.bin", "16.1.0"))
	if err == nil || errors.Is(err, ErrUploadRefused) || errors.Is(err, ErrUploadUnsupported) {
		t.Fatalf("got %v, want an uncertain transport error", err)
	}
	waitBoots(t, c, 1)
	if c.Version() != "16.1.0" {
		t.Errorf("the device flashed regardless of the lost reply, version %s", c.Version())
	}
}

// Why: a cut connection and a slow device both end with an error that is not
// a refusal; the slow one is a timeout the caller can recognise.
func TestUploadFirmwareCutAndSlow(t *testing.T) {
	img := func(t *testing.T) []byte { return simImage(t, "tasmota32.bin", "16.1.0") }
	t.Run("cut", func(t *testing.T) {
		c, tg := uploadSim(t, simulator.DeviceSpec{Faults: []simulator.Fault{{Kind: simulator.FaultUploadCut}}})
		err := upload(uploader(t, Options{}), tg, img(t))
		if err == nil || errors.Is(err, ErrUploadRefused) {
			t.Fatalf("got %v", err)
		}
		if c.Version() != "16.0.0" {
			t.Error("a cut body must not flash")
		}
	})
	t.Run("slow", func(t *testing.T) {
		_, tg := uploadSim(t, simulator.DeviceSpec{Faults: []simulator.Fault{{Kind: simulator.FaultSlowWrite, Duration: 2 * time.Second}}})
		err := upload(uploader(t, Options{Timeout: 300 * time.Millisecond}), tg, img(t))
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("got %v, want ErrTimeout", err)
		}
	})
}

// Why: the SSRF rule holds for uploads too: an address outside the device
// networks is never contacted.
func TestUploadFirmwareHonoursTheDeviceNetworks(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	u := NewHTTPClient(Options{AllowedNetworks: nil}).(Uploader)
	err := upload(u, target(t, srv, 1, "", ""), []byte{0xE9})
	if !errors.Is(err, ErrForbiddenAddr) || hits.Load() != 0 {
		t.Fatalf("got %v, hits %d", err, hits.Load())
	}
}

// Why: a body shorter than the announced size must fail, not send a short image.
func TestUploadFirmwareShortBodyFails(t *testing.T) {
	c, tg := uploadSim(t, simulator.DeviceSpec{})
	img := simImage(t, "tasmota32.bin", "16.1.0")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := uploader(t, Options{}).UploadFirmware(ctx, tg, "tasmota32.bin", int64(len(img)), bytes.NewReader(img[:100]))
	if err == nil {
		t.Fatal("a short body must fail")
	}
	if c.Version() != "16.0.0" {
		t.Error("a short body must not flash")
	}
}

// Why: Tasmota (and the simulator) answer as soon as the multipart closing
// boundary arrives, without waiting for the announced length. If a short image
// is followed by the closing boundary, the device may take the cut image and
// reply "Successful" before the client notices the short body (a flaky
// TestUploadFirmwareShortBodyFails in CI). So the closing boundary must never
// be sent after a short image.
func TestUploadFirmwareShortBodyNeverSendsTheClosingBoundary(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("<form method='post' action='u2?fsz=' enctype='multipart/form-data'><input type='file' name='u2'></form>"))
			return
		}
		b, _ := io.ReadAll(r.Body) // ends with an error when the client gives up
		got <- b
	}))
	defer srv.Close()
	img := bytes.Repeat([]byte{0xE9}, 5000)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := uploader(t, Options{}).UploadFirmware(ctx, target(t, srv, 1, "", ""), "tasmota32.bin", int64(len(img)), bytes.NewReader(img[:100]))
	if err == nil {
		t.Fatal("a short body must fail")
	}
	select {
	case b := <-got:
		if bytes.Contains(b, []byte("--\r\n")) {
			t.Errorf("the closing boundary was sent after a short image: %q", b[max(0, len(b)-80):])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the upload")
	}
}

// Why: the first real ESP32 push (2026-10-24) hung for minutes without any
// trace of where. A stalled upload must say how much of the request the
// device took, so the next attempt's history shows whether the body never
// went out or stopped part-way. It stays an uncertain timeout (probe, never
// resend), not a refusal.
func TestUploadFirmwareStallReportsProgress(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("<form method='post' action='u2?fsz=' enctype='multipart/form-data'><input type='file' name='u2'></form>"))
			return
		}
		_, _ = io.CopyN(io.Discard, r.Body, 1000)
		<-stop // stop reading, like a device that stalls
	}))
	defer srv.Close()
	defer close(stop) // runs before srv.Close
	img := bytes.Repeat([]byte{0xE9}, 8<<20)
	err := upload(uploader(t, Options{Timeout: 300 * time.Millisecond}), target(t, srv, 1, "", ""), img)
	if !errors.Is(err, ErrTimeout) || errors.Is(err, ErrUploadRefused) {
		t.Fatalf("got %v, want an uncertain ErrTimeout", err)
	}
	m := regexp.MustCompile(`after (\d+) of (\d+) bytes`).FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("no progress in %q", err)
	}
	sent, _ := strconv.Atoi(m[1])
	total, _ := strconv.Atoi(m[2])
	if sent <= 0 || sent >= total || total <= len(img) {
		t.Fatalf("progress %d of %d for a %d byte image", sent, total, len(img))
	}
}
