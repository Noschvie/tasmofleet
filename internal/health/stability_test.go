package health

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// fakeStability keeps what the tracker stores.
type fakeStability struct {
	mu      sync.Mutex
	boots   []model.DeviceBoot
	samples []model.WifiSample
}

func (f *fakeStability) AppendBoot(_ context.Context, b model.DeviceBoot) error {
	f.mu.Lock()
	f.boots = append(f.boots, b)
	f.mu.Unlock()
	return nil
}
func (f *fakeStability) ListBoots(_ context.Context, id model.DeviceID, limit int) ([]model.DeviceBoot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.DeviceBoot
	for i := len(f.boots) - 1; i >= 0; i-- {
		if f.boots[i].DeviceID == id && (limit <= 0 || len(out) < limit) {
			out = append(out, f.boots[i])
		}
	}
	return out, nil
}
func (f *fakeStability) BootsSince(context.Context, time.Time) (map[model.DeviceID][]model.DeviceBoot, error) {
	return nil, nil
}
func (f *fakeStability) AppendWifiSample(_ context.Context, s model.WifiSample) error {
	f.mu.Lock()
	f.samples = append(f.samples, s)
	f.mu.Unlock()
	return nil
}
func (f *fakeStability) WifiSamplesSince(context.Context, time.Time) (map[model.DeviceID][]model.WifiSample, error) {
	return nil, nil
}
func (f *fakeStability) PruneWifiSamples(context.Context, time.Time) (int64, error) { return 0, nil }
func (f *fakeStability) all() []model.DeviceBoot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.DeviceBoot(nil), f.boots...)
}

var _ store.StabilityStore = (*fakeStability)(nil)

// stabilityDevice answers Status 11 with the given uptime and Status 1 with a restart reason.
type stabilityDevice struct {
	mu        sync.Mutex
	uptime    int
	bootCount int
	reason    string
	signal    int
	links     int
	status1   int // Status 1 requests
}

func (d *stabilityDevice) handler(_, cmnd string) (json.RawMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch cmnd {
	case "Status 1":
		d.status1++
		return json.RawMessage(fmt.Sprintf(`{"StatusPRM":{"RestartReason":%q,"BootCount":%d}}`, d.reason, d.bootCount)), nil
	default:
		return json.RawMessage(fmt.Sprintf(`{"StatusSTS":{"UptimeSec":%d,"Wifi":{"RSSI":50,"Signal":%d,"LinkCount":%d,"Downtime":"0T00:03:41"},"POWER":"ON"}}`, d.uptime, d.signal, d.links)), nil
	}
}

func (d *stabilityDevice) set(f func(*stabilityDevice)) {
	d.mu.Lock()
	f(d)
	d.mu.Unlock()
}

func stabilityHarness(t *testing.T, withBus bool, dev model.Device) (*harness, *stabilityDevice, *fakeStability) {
	t.Helper()
	h := newHarness(t, withBus, Options{}, dev)
	fs := &fakeStability{}
	h.svc.d.Stability = fs
	sd := &stabilityDevice{uptime: 7200, bootCount: 41, reason: "Software/System restart", signal: -60, links: 1}
	h.cl.set(sd.handler)
	return h, sd, fs
}

// Why: an HTTP-only device has no INFO3. A restart shows only as an uptime that fell between two
// polls, and then the reason must be read once with Status 1 (never Status 0) and stored with how
// long the device had been up before.
func TestHTTPOnlyRestartIsDetectedFromAnUptimeDrop(t *testing.T) {
	h, sd, fs := stabilityHarness(t, false, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	h.step() // first reading: baseline
	h.step() // runs the baseline Status 1
	if got := fs.all(); len(got) != 1 || got[0].BootCount != 41 {
		t.Fatalf("baseline: %+v, want the current boot recorded once", got)
	}
	sd.set(func(d *stabilityDevice) { d.uptime, d.bootCount, d.reason = 20, 42, "Exception" })
	h.advance(6 * time.Minute)
	h.step() // poll sees the drop
	h.step() // Status 1
	got := fs.all()
	if len(got) != 2 {
		t.Fatalf("restarts stored: %+v, want baseline + the new one", got)
	}
	b := got[1]
	if b.Reason != "Exception" || b.BootCount != 42 || b.UptimeBefore != 7200*time.Second {
		t.Fatalf("restart = %+v, want Exception, BootCount 42, 2 h before", b)
	}
	if want := h.clk.Now().Add(-20 * time.Second); !b.At.Equal(want) {
		t.Fatalf("restart time %v, want now - uptime = %v", b.At, want)
	}
}

// Why: a poll and an MQTT STATE of the same moment can arrive a few seconds out of order; that must
// not look like a restart, or every device would log a phantom restart per poll.
func TestSmallUptimeJitterIsNoRestart(t *testing.T) {
	h, sd, fs := stabilityHarness(t, false, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	h.step()
	h.step()
	n := sd.status1
	sd.set(func(d *stabilityDevice) { d.uptime = 7195 })
	h.advance(6 * time.Minute)
	h.step()
	h.step()
	if sd.status1 != n || len(fs.all()) != 1 {
		t.Fatalf("Status 1 sent %d times (was %d), %d restarts stored: a 5 s step back is not a restart", sd.status1, n, len(fs.all()))
	}
}

// Why: once the baseline is stored, a TasmoFleet restart must not read Status 1 of the whole fleet
// again or store a second row for a boot it already knows.
func TestBaselineIsNotRepeatedAfterATasmoFleetRestart(t *testing.T) {
	h, sd, fs := stabilityHarness(t, false, httpDev(1))
	_ = fs.AppendBoot(h.ctx, model.DeviceBoot{DeviceID: 1, At: h.clk.Now().Add(-time.Hour), BootCount: 41})
	h.svc.reload(h.ctx, h.clk.Now())
	h.step()
	h.step()
	if sd.status1 != 0 || len(fs.all()) != 1 {
		t.Fatalf("Status 1 sent %d times, %d rows: a stored restart makes the baseline unnecessary", sd.status1, len(fs.all()))
	}
}

// Why: with MQTT the device announces its restart itself, with the reason: INFO3 must be stored with
// the BootCount and the uptime the device had before, without any command to the device.
func TestInfo3IsStoredAsARestart(t *testing.T) {
	h, sd, fs := stabilityHarness(t, true, mqttDev(1, "plug"))
	h.listen()
	h.bus.emit(mqttbus.KindState, "plug", "STATE", `{"UptimeSec":900,"Wifi":{"Signal":-60}}`, false)
	h.bus.emit(mqttbus.KindInfo, "plug", "INFO3", `{"Info3":{"RestartReason":"Hardware Watchdog","BootCount":7}}`, false)
	// the worker goroutine is not running in the harness: run the queued write
	for len(h.svc.work) > 0 {
		(<-h.svc.work)()
	}
	got := fs.all()
	if len(got) != 1 || got[0].Reason != "Hardware Watchdog" || got[0].BootCount != 7 || got[0].UptimeBefore != 900*time.Second {
		t.Fatalf("restarts = %+v", got)
	}
	// The uptime that follows is a new run, not a "drop" that needs a Status 1.
	h.bus.emit(mqttbus.KindState, "plug", "STATE", `{"UptimeSec":3,"Wifi":{"Signal":-60}}`, false)
	h.step()
	if sd.status1 != 0 {
		t.Fatalf("Status 1 sent %d times after INFO3", sd.status1)
	}
}

// Why: a retained INFO3 is the last boot message the broker kept, not a restart now; counting it at
// every TasmoFleet start would add one restart per device per start.
func TestRetainedAndRepeatedInfo3IsNotARestart(t *testing.T) {
	h, _, fs := stabilityHarness(t, true, mqttDev(1, "plug"))
	h.listen()
	h.bus.emit(mqttbus.KindInfo, "plug", "INFO3", `{"Info3":{"RestartReason":"Exception","BootCount":7}}`, true)
	h.bus.emit(mqttbus.KindInfo, "plug", "INFO3", `{"Info3":{"RestartReason":"Exception","BootCount":7}}`, false)
	h.bus.emit(mqttbus.KindInfo, "plug", "INFO3", `{"Info3":{"RestartReason":"Exception","BootCount":7}}`, false)
	for len(h.svc.work) > 0 {
		(<-h.svc.work)()
	}
	if got := fs.all(); len(got) != 1 {
		t.Fatalf("%d restarts stored, want 1 (retained ignored, same BootCount once)", len(got))
	}
}

// Why: a failing Status 1 (minimal firmware, a device that is not reachable over HTTP) must not be
// retried for ever; each device would be asked every five minutes.
func TestFailedRestartReadsAreGivenUp(t *testing.T) {
	h, sd, _ := stabilityHarness(t, false, httpDev(1))
	calls := 0
	h.cl.set(func(a, c string) (json.RawMessage, error) {
		if c == "Status 1" {
			calls++
			return nil, errDown
		}
		return sd.handler(a, c)
	})
	h.svc.reload(h.ctx, h.clk.Now())
	for i := 0; i < 12; i++ {
		h.step()
		h.advance(6 * time.Minute)
	}
	if calls != bootCheckTries {
		t.Fatalf("Status 1 attempted %d times, want %d", calls, bootCheckTries)
	}
}

// Why: the Wi-Fi history is hourly; polling every five minutes must still give one sample per hour
// and none for a device that is offline.
func TestWifiIsSampledOncePerHour(t *testing.T) {
	h, _, fs := stabilityHarness(t, false, httpDev(1))
	h.clk.now = time.Date(2026, 10, 18, 12, 10, 0, 0, time.UTC)
	h.svc.reload(h.ctx, h.clk.Now())
	for i := 0; i < 13; i++ { // 65 minutes in 5 minute steps
		h.step()
		h.advance(5 * time.Minute)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.samples) != 2 || fs.samples[0].DBm != -60 || !fs.samples[0].At.Equal(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("samples %+v, want two on the hours 12:00 and 13:00", fs.samples)
	}
}

// Why: LinkCount and Downtime are what tells a weak link from a good one; they must reach Health.
func TestLinkCountAndDowntimeReachHealth(t *testing.T) {
	h, sd, _ := stabilityHarness(t, false, httpDev(1))
	sd.set(func(d *stabilityDevice) { d.links = 15 })
	h.svc.reload(h.ctx, h.clk.Now())
	h.step()
	g := h.svc.Get(1)
	if g.LinkCount != 15 || g.SignalDBm != -60 || g.WifiDowntime != 3*time.Minute+41*time.Second {
		t.Fatalf("health: links %d signal %d downtime %v", g.LinkCount, g.SignalDBm, g.WifiDowntime)
	}
}

func TestParseDowntime(t *testing.T) {
	for in, want := range map[string]time.Duration{"0T00:03:41": 221 * time.Second, "2T01:00:00": 49 * time.Hour} {
		if got, ok := parseDowntime(in); !ok || got != want {
			t.Errorf("%q -> %v %v, want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "00:03:41", "xT00:00:00", "0T00:00", "0T0a:00:00"} {
		if _, ok := parseDowntime(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

// Why: two restarts with no reading between them (a boot loop faster than the telemetry period) must
// not both claim the uptime of the run before the first one; the second one's is unknown.
func TestUptimeBeforeIsNotReusedForTheNextRestart(t *testing.T) {
	h, _, fs := stabilityHarness(t, true, mqttDev(1, "plug"))
	h.listen()
	h.bus.emit(mqttbus.KindState, "plug", "STATE", `{"UptimeSec":4000,"Wifi":{"Signal":-60}}`, false)
	h.bus.emit(mqttbus.KindInfo, "plug", "INFO3", `{"Info3":{"RestartReason":"Exception","BootCount":7}}`, false)
	h.bus.emit(mqttbus.KindInfo, "plug", "INFO3", `{"Info3":{"RestartReason":"Exception","BootCount":8}}`, false)
	for len(h.svc.work) > 0 {
		(<-h.svc.work)()
	}
	got := fs.all()
	if len(got) != 2 || got[0].UptimeBefore != 4000*time.Second || got[1].UptimeBefore != 0 {
		t.Fatalf("restarts %+v, want 4000 s then unknown", got)
	}
}
