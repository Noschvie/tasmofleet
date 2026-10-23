package simulator

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// postU2 sends the multipart body of Tasmota's upload form to /u2.
func postU2(d Device, filename string, img []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("u2", filename)
	_, _ = fw.Write(img)
	_ = mw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+d.Addr()+"/u2?fsz="+strconv.Itoa(len(img)), &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func getUp(t *testing.T, d Device) {
	t.Helper()
	b, code, err := cmWithPath(d, "/up", "")
	if err != nil || code != 200 || !strings.Contains(b, "action='u2?fsz='") {
		t.Fatalf("GET /up: %d %v", code, err)
	}
}

func esp32Image(t *testing.T, name, version string) []byte {
	t.Helper()
	img, err := NewImage(fixtures, name, version)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// Why: the push delivery must flash exactly like the web UI form does:
// GET /up, then the multipart POST; the device restarts on the new version.
func TestU2UploadFlashesAndRestarts(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0"})
	getUp(t, d)
	page, err := postU2(d, "tasmota32.bin", esp32Image(t, "tasmota32.bin", "16.1.0"))
	if err != nil || !strings.Contains(page, "Successful") {
		t.Fatalf("%v %s", err, page)
	}
	settled(t, d, 1)
	eq(t, "version", d.Version(), "16.1.0")
	if !slicesContainsPrefix(d.Commands(), "POST /u2 tasmota32.bin") {
		t.Errorf("commands %v", d.Commands())
	}
}

func slicesContainsPrefix(l []string, p string) bool {
	for _, s := range l {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Why: real Tasmota only flashes after GET /up has set the upload type. A POST
// without it answers "Successful" and restarts WITHOUT flashing. A client that
// skips GET /up would report a success that did not happen; the simulator keeps
// this trap so such a client fails its test.
func TestU2WithoutUpPageLooksSuccessfulButFlashesNothing(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0"})
	page, err := postU2(d, "tasmota32.bin", esp32Image(t, "tasmota32.bin", "16.1.0"))
	if err != nil || !strings.Contains(page, "Successful") {
		t.Fatalf("%v %s", err, page)
	}
	settled(t, d, 1)
	eq(t, "version unchanged although the page said Successful", d.Version(), "16.0.0")
}

// Why: the upload type is consumed by one upload; a second POST needs a new GET /up.
func TestU2UploadTypeIsOneShot(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0"})
	getUp(t, d)
	if _, err := postU2(d, "tasmota32.bin", []byte{0x00, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if page, _ := postU2(d, "tasmota32.bin", esp32Image(t, "tasmota32.bin", "16.1.0")); !strings.Contains(page, "Successful") {
		t.Fatal(page)
	}
	settled(t, d, 1)
	eq(t, "second POST without /up flashed nothing", d.Version(), "16.0.0")
}

// Why: the device refuses what it cannot run, with Tasmota's own messages; the
// client must see a failure and nothing may be flashed.
func TestU2Refusals(t *testing.T) {
	cases := []struct {
		name    string
		profile Profile
		file    string
		img     func(*testing.T) []byte
		want    string
	}{
		{"not an image", ProfileESP32, "tasmota32.bin", func(*testing.T) []byte { return []byte("<html>nope</html>") }, "Invalid file signature"},
		{"wrong chip family", ProfileESP32, "tasmota32c3.bin", func(t *testing.T) []byte { return esp32Image(t, "tasmota32c3.bin", "16.1.0") }, "Upload failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTestFleet(t, "", Extra{})
			d := add(t, f, DeviceSpec{Name: "plug", Profile: c.profile, Version: "16.0.0"})
			getUp(t, d)
			page, err := postU2(d, c.file, c.img(t))
			if err != nil || !strings.Contains(page, "Failed") || !strings.Contains(page, c.want) {
				t.Fatalf("%v %s", err, page)
			}
			eq(t, "version", d.Version(), "16.0.0")
			eq(t, "phase", d.Phase(), PhaseRun)
		})
	}
}

// Why: a body cut halfway (wifi drop) must leave the old firmware running and
// the client with a transport error, never with a success page.
func TestU2ConnectionCutFlashesNothing(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0", Faults: []Fault{{Kind: FaultUploadCut}}})
	getUp(t, d)
	if page, err := postU2(d, "tasmota32.bin", esp32Image(t, "tasmota32.bin", "16.1.0")); err == nil {
		t.Fatalf("expected a transport error, got %q", page)
	}
	eq(t, "version", d.Version(), "16.0.0")
	eq(t, "phase", d.Phase(), PhaseRun)
}

// Why: the dangerous ambiguity: the image is flashed but the reply is lost. The
// client cannot tell from the error, so it must probe and never resend.
func TestU2LostReplyStillFlashes(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0", Faults: []Fault{{Kind: FaultUploadLostReply}}})
	getUp(t, d)
	if page, err := postU2(d, "tasmota32.bin", esp32Image(t, "tasmota32.bin", "16.1.0")); err == nil {
		t.Fatalf("expected a transport error, got %q", page)
	}
	settled(t, d, 1)
	eq(t, "version", d.Version(), "16.1.0")
}

// Why: flash writes on a real device are slow; a client with a short timeout
// must be able to hit it in a test.
func TestU2SlowWriteDelaysTheReply(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0", Faults: []Fault{{Kind: FaultSlowWrite, Duration: 300 * time.Millisecond}}})
	getUp(t, d)
	start := time.Now()
	if _, err := postU2(d, "tasmota32.bin", esp32Image(t, "tasmota32.bin", "16.1.0")); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Errorf("reply after %v, want at least the slow-write delay", time.Since(start))
	}
}

// Why: the wrong_family fault is shared with the pull flow; it must make the
// upload fail too, so one scripted fault covers both deliveries.
func TestU2WrongFamilyFaultAppliesToAnyImage(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0", Faults: []Fault{{Kind: FaultWrongFamily}}})
	getUp(t, d)
	page, err := postU2(d, "tasmota32.bin", esp32Image(t, "tasmota32.bin", "16.1.0"))
	if err != nil || !strings.Contains(page, "Failed") {
		t.Fatalf("%v %s", err, page)
	}
}
