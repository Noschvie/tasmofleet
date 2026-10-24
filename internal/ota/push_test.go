package ota

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Why: an ESP8266 has little RAM and the upload path is untested on real
// hardware, so it must be refused with a reason that names the way out.
// Every other chip, unknown ones included, is refused unless it is an ESP32 on
// a normal firmware: the upload writes to the running device.
func TestPushDeviceRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   model.Identity
		want string // substring of the refusal; empty = allowed
	}{
		{"esp8266", model.Identity{Family: model.ChipESP8266, Variant: "tasmota"}, "ESP8266"},
		{"unknown chip", model.Identity{Family: model.ChipUnknown, Variant: "tasmota"}, "known ESP32"},
		{"esp32 safeboot", model.Identity{Family: model.ChipESP32, Variant: variantSafeboot}, "safeboot"},
		{"esp32 minimal variant", model.Identity{Family: model.ChipESP32S3, Variant: variantMinimal}, "safeboot"},
		{"esp32", model.Identity{Family: model.ChipESP32, Variant: "tasmota"}, ""},
		{"esp32c3 sensors", model.Identity{Family: model.ChipESP32C3, Variant: "sensors"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := pushDeviceRefusal(tc.id)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal mentioning %q", err, tc.want)
			}
		})
	}
}

// Why: the refusal is shown to the user as it is; a backlog id ("F-9") means nothing to them,
// so it must stay in the code comments only, and the way out (the download delivery) must be named.
func TestPushESP8266RefusalIsPlainLanguage(t *testing.T) {
	err := pushDeviceRefusal(model.Identity{Family: model.ChipESP8266})
	if err == nil || strings.Contains(err.Error(), "F-9") || strings.Contains(err.Error(), "backlog") || !strings.Contains(err.Error(), "download delivery") {
		t.Fatalf("got %v", err)
	}
}

// Why: only a direct plan is one upload; a staged plan needs the minimal hop
// the push does not drive.
func TestPushPlanRefusal(t *testing.T) {
	if err := pushPlanRefusal(model.PlanDirect); err != nil {
		t.Fatal(err)
	}
	if err := pushPlanRefusal(model.PlanStaged); err == nil {
		t.Fatal("a staged plan was accepted")
	}
	if err := pushPlanRefusal(""); err == nil {
		t.Fatal("an unknown plan was accepted")
	}
}

// ---- engine level, with fakes ----

func esp32Ident(version string) model.Identity {
	return model.Identity{Family: model.ChipESP32, Hardware: "ESP32-D0WD-V3", Version: version, Image: "tasmota32", Variant: "tasmota", FlashSizeKB: 4096, FreeKB: 1800}
}

// pushAPI adds the optional upload to the fake device API. A flashed image
// makes the fake device reboot into the new version.
type pushAPI struct {
	*fakeAPI
	mu2       sync.Mutex
	uploads   []string
	err       error // returned after the device flashed (a lost reply)
	refuse    error // returned without flashing
	size      int64
	readBytes int64
	// stall makes the upload take this long (virtual time) and end without
	// flashing, like a POST whose body never reaches the device's handler.
	stall  time.Duration
	during func() // called while the upload runs
}

func (p *pushAPI) UploadFirmware(_ context.Context, d model.Device, name string, size int64, body io.Reader) error {
	p.rec.add("upload %d %s", d.ID, name)
	n, _ := io.Copy(io.Discard, body)
	p.mu2.Lock()
	p.uploads = append(p.uploads, name)
	p.size, p.readBytes = size, n
	p.mu2.Unlock()
	if p.during != nil {
		p.during()
	}
	if p.stall > 0 {
		<-p.clk.After(p.stall)
		return errors.New("upload: tasmota: timeout: no answer from 192.0.2.1")
	}
	if p.refuse != nil {
		return p.refuse
	}
	p.mu.Lock()
	fd := p.dev(d.ID)
	fd.ident, fd.offline, fd.uptime = esp32Ident("16.0.0"), 2, 0
	p.mu.Unlock()
	return p.err
}

type pushFW struct {
	fakeFW
	rec     *recorder
	openErr error
}

func (f pushFW) Open(_ context.Context, a model.Artifact) (io.ReadCloser, error) {
	f.rec.add("open %s", a.Filename)
	if f.openErr != nil {
		return nil, f.openErr
	}
	return io.NopCloser(bytes.NewReader(make([]byte, a.Size))), nil
}

func pushHarness(t *testing.T, fd *fakeDevice) (*harness, *pushAPI, pushFW) {
	t.Helper()
	h := newHarness(t, 2, fd)
	pa := &pushAPI{fakeAPI: h.api}
	pf := pushFW{fakeFW: h.fw, rec: h.rec}
	e := NewEngine(Options{Machine: testCfg(), MaxParallel: 2, Power: h.power}, h.st, h.st, pa, pf, h.backups, h.clk, nil)
	e.sel = h.sel
	h.e = e
	return h, pa, pf
}

func esp32Device() *fakeDevice {
	return &fakeDevice{ident: esp32Ident("15.6.0"), uptime: 5000, otaURL: origURL}
}

// Why: the push delivery is the same transaction with another last mile: Guard
// right before the upload, the image uploaded once, and the device's OtaUrl
// never written, so nothing has to be restored.
func TestEnginePushUploadsOnceAfterGuardAndLeavesOtaUrlAlone(t *testing.T) {
	h, pa, _ := pushHarness(t, esp32Device())
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush})
	h.run(t, h.e)

	j := h.job(t, id)
	if j.State != model.JobSucceeded || j.LastError != "" || j.Delivery != model.DeliveryPush {
		t.Fatalf("job %s %q delivery %q\n%v", j.State, j.LastError, j.Delivery, h.rec.entries())
	}
	log := h.rec.entries()
	if h.rec.count("set_ota_url") != 0 || h.rec.count("upgrade") != 0 {
		t.Fatalf("a push job must not touch OtaUrl or send Upgrade: %v", log)
	}
	if len(pa.uploads) != 1 {
		t.Fatalf("uploads: %v", pa.uploads)
	}
	if pa.size != 469152 || pa.readBytes != pa.size {
		t.Errorf("announced %d bytes, body had %d", pa.size, pa.readBytes)
	}
	for i, e := range log {
		if strings.HasPrefix(e, "upload ") && !strings.HasPrefix(log[i-2], "guard ") {
			t.Fatalf("upload not preceded by a guard run (guard, open, upload expected): %v", log[:i+1])
		}
	}
	if d := h.api.dev(1); d.otaURL != origURL {
		t.Fatalf("OtaUrl is %q, want it untouched", d.otaURL)
	}
	for i, e := range log {
		if strings.HasPrefix(e, "upload") {
			saved := ""
			for _, p := range log[:i] {
				if strings.HasPrefix(p, "save 1 ") {
					saved = p
				}
			}
			if saved != "save 1 upgrade_issued" {
				t.Fatalf("upload sent while the last persisted state was %q", saved)
			}
		}
	}
}

// Why: Guard must refuse an upload exactly like it refuses an OtaUrl: nothing
// is read from the cache and nothing reaches the device.
func TestEnginePushGuardRefusalSendsNothing(t *testing.T) {
	h, pa, _ := pushHarness(t, esp32Device())
	h.sel.refuse = true
	_, err := h.e.Submit(context.Background(), "alice", []Request{{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush}})
	if err == nil {
		t.Fatal("Submit accepted a transaction the guard refuses")
	}
	if len(pa.uploads) != 0 || h.rec.count("open") != 0 {
		t.Fatalf("something was sent: %v", h.rec.entries())
	}
}

// Why: ESP8266 stays on the download delivery until F-9 is done; the refusal
// happens at Submit, before any job exists and before any image is fetched.
func TestEnginePushRefusesESP8266AtSubmit(t *testing.T) {
	h, pa, _ := pushHarness(t, plugDevice())
	_, err := h.e.Submit(context.Background(), "alice", []Request{{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush}})
	var re *RefusedError
	if !errors.As(err, &re) || len(re.Refused) != 1 || !strings.Contains(re.Refused[0].Reason, "ESP8266") {
		t.Fatalf("got %v", err)
	}
	if len(pa.uploads) != 0 || h.rec.count("set_ota_url") != 0 {
		t.Fatalf("device was touched: %v", h.rec.entries())
	}
}

// Why: a typo in the delivery must not silently fall back to another path.
func TestEngineUnknownDeliveryIsRefused(t *testing.T) {
	h, _, _ := pushHarness(t, esp32Device())
	_, err := h.e.Submit(context.Background(), "alice", []Request{{DeviceID: 1, Release: "16.0.0", Delivery: "ftp"}})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "unknown delivery") {
		t.Fatalf("got %v", err)
	}
}

// Why: the default stays the download delivery, and an API that cannot upload
// must make a push job fail loudly instead of falling back to OtaUrl.
func TestEnginePushWithoutUploaderIsRefused(t *testing.T) {
	h := newHarness(t, 2, esp32Device()) // plain fakes: no upload capability
	_, err := h.e.Submit(context.Background(), "alice", []Request{{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush}})
	_ = err // the plan passes; the job must fail at the first push step
	jobs, _ := h.st.ListJobs(context.Background(), storeFilter(1))
	if len(jobs) != 1 {
		t.Fatalf("jobs: %v", jobs)
	}
	h.run(t, h.e)
	j := h.job(t, jobs[0].ID)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "cannot") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if h.rec.count("set_ota_url") != 0 || h.rec.count("upgrade") != 0 {
		t.Fatalf("fell back to OtaUrl/Upgrade: %v", h.rec.entries())
	}
}

// Why: a lost reply after the image went out must not trigger a second
// upload: the engine probes, sees the new version and finishes.
func TestEnginePushLostReplyIsNotResent(t *testing.T) {
	h, pa, _ := pushHarness(t, esp32Device())
	pa.err = errors.New("upload: connection lost")
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush})
	h.run(t, h.e)
	if j := h.job(t, id); j.State != model.JobSucceeded {
		t.Fatalf("job %s %q\n%v", j.State, j.LastError, h.rec.entries())
	}
	if len(pa.uploads) != 1 {
		t.Fatalf("image sent %d times after a lost reply: %v", len(pa.uploads), h.rec.entries())
	}
}

// Why: the first real ESP32 push (2026-10-24) stalled for the whole
// upgrade_issued window; the job then timed out without a single probe and
// uploaded the image a second time. After an upload whose outcome is unknown
// the engine must probe (the device may still be flashing) and never send the
// image again (safety rule 3); without a reboot the job ends failed.
func TestEnginePushStalledUploadIsProbedAndNotResent(t *testing.T) {
	h, pa, _ := pushHarness(t, esp32Device())
	pa.stall = testCfg().UpgradeAccept
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, ErrTextNoUpgradeStart) {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if n := len(pa.uploads); n != 1 {
		t.Fatalf("image sent %d times after a stalled upload: %v", n, h.rec.entries())
	}
	log := h.rec.entries()
	up := slices.IndexFunc(log, func(s string) bool { return strings.HasPrefix(s, "upload ") })
	if probes := slices.IndexFunc(log[up+1:], func(s string) bool { return strings.HasPrefix(s, "probe ") }); probes < 0 {
		t.Fatalf("no probe after the stalled upload: %v", log)
	}
	// The upload's own error is in the job history, not swallowed.
	h.st.mu.Lock()
	defer h.st.mu.Unlock()
	if !slices.ContainsFunc(h.st.events, func(ev model.JobEvent) bool { return strings.Contains(ev.Detail, "no answer") }) {
		t.Fatalf("upload error not recorded: %+v", h.st.events)
	}
}

// Why: a cancel that arrives while the upload runs must end the transaction
// (live 2026-10-24 it stayed in upgrade_issued and the image went out again).
// The stage is finished first, so the device is probed, but nothing is sent
// again and the job ends cancelled.
func TestEnginePushCancelDuringUploadEndsCancelledWithoutResend(t *testing.T) {
	h, pa, _ := pushHarness(t, esp32Device())
	pa.stall = testCfg().UpgradeAccept
	var id model.JobID
	pa.during = func() {
		if err := h.e.Cancel(context.Background(), "bob", id); err != nil {
			t.Errorf("Cancel: %v", err)
		}
	}
	id = submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobCancelled {
		t.Fatalf("job %s %q\n%v", j.State, j.LastError, h.rec.entries())
	}
	if n := len(pa.uploads); n != 1 {
		t.Fatalf("image sent %d times after a cancel: %v", n, h.rec.entries())
	}
}

// Why: a refusal by the device means nothing was flashed; the job may try
// again, but only after probing and at most MaxAttempts times, and ends failed
// with the device's reason.
func TestEnginePushRefusedByDeviceEndsFailed(t *testing.T) {
	h, pa, _ := pushHarness(t, esp32Device())
	pa.refuse = errors.New("upload: tasmota: device refused the upload: Invalid file signature")
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "Invalid file signature") && !strings.Contains(j.LastError, "did not start") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if n := len(pa.uploads); n > testCfg().MaxAttempts {
		t.Fatalf("%d uploads, at most %d attempts", n, testCfg().MaxAttempts)
	}
}

// Why: a damaged cache file is found before the job commits to the upload, so
// the device is not touched.
func TestEnginePushDamagedCacheFileFailsBeforeUpload(t *testing.T) {
	h, pa, pf := pushHarness(t, esp32Device())
	pf.openErr = errors.New("content does not match the cached artifact")
	h.e = NewEngine(Options{Machine: testCfg(), MaxParallel: 2, Power: h.power}, h.st, h.st, pa, pf, h.backups, h.clk, nil)
	h.e.sel = h.sel
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "does not match") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if len(pa.uploads) != 0 {
		t.Fatal("an unverified image was uploaded")
	}
}

// Why: a retry must repeat the delivery of the original transaction.
func TestRetryRequestKeepsTheDelivery(t *testing.T) {
	r := retryRequest(model.Job{DeviceID: 1, Delivery: model.DeliveryPush})
	if r.Delivery != model.DeliveryPush {
		t.Fatalf("delivery %q", r.Delivery)
	}
}

func storeFilter(dev model.DeviceID) store.JobFilter { return store.JobFilter{DeviceID: dev} }

type uploadingClient struct {
	*fixtureClient
	got  string
	size int64
	err  error
}

func (c *uploadingClient) UploadFirmware(_ context.Context, _ tasmota.Target, name string, size int64, _ io.Reader) error {
	c.got, c.size = name, size
	return c.err
}

// Why: the production DeviceAPI hands the upload to the tasmota client
// unchanged and says where a failure came from; a client that cannot upload is
// an error, never a silent success.
func TestDeviceAPIUploadFirmware(t *testing.T) {
	fc := &uploadingClient{fixtureClient: &fixtureClient{t: t, dir: cabinet}}
	api := newTestAPI(fc.fixtureClient, nil, nil)
	if err := api.UploadFirmware(context.Background(), model.Device{ID: 1}, "tasmota32.bin", 10, bytes.NewReader(nil)); err == nil {
		t.Fatal("a client without the Uploader capability must be an error")
	}
	api.c = fc
	if err := api.UploadFirmware(context.Background(), model.Device{ID: 1}, "tasmota32.bin", 10, bytes.NewReader(nil)); err != nil || fc.got != "tasmota32.bin" || fc.size != 10 {
		t.Fatalf("err %v, got %q %d", err, fc.got, fc.size)
	}
	fc.err = tasmota.ErrUploadRefused
	err := api.UploadFirmware(context.Background(), model.Device{ID: 1}, "x.bin", 1, bytes.NewReader(nil))
	if !errors.Is(err, tasmota.ErrUploadRefused) || !strings.HasPrefix(err.Error(), "upload: ") {
		t.Fatalf("got %v", err)
	}
}
