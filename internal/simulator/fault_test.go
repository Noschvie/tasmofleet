package simulator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func newDirect(t *testing.T, f Fleet, name string, faults ...Fault) Controller {
	return add(t, f, DeviceSpec{Name: name, Profile: ProfileESP8266_4M, Faults: faults})
}

func startUpgrade(t *testing.T, d Controller, fw *firmwareServer, file string) {
	t.Helper()
	cm(t, d, "OtaUrl "+fw.URL+"/"+file)
	cm(t, d, "Upgrade 1")
}

// Why: a reply lost on the wire must not hide that the command ran; the
// engine's "probe before re-sending Upgrade" rule depends on this.
func TestDropAndTimeoutExecuteTheCommandButLoseTheReply(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := newDirect(t, f, "a", Fault{Kind: FaultDrop, After: "Power ON"}, Fault{Kind: FaultTimeout, After: "Power OFF"})
	if _, _, err := cmWith(d, "Power ON", time.Second, nil); err == nil {
		t.Error("drop must break the connection")
	}
	if !d.Power() {
		t.Error("dropped command still has to take effect")
	}
	if _, _, err := cmWith(d, "Power OFF", 150*time.Millisecond, nil); err == nil {
		t.Error("timeout must not answer")
	}
	if d.Power() {
		t.Error("timed out command still has to take effect")
	}
	if got := cm(t, d, "Power"); got != `{"POWER":"OFF"}` {
		t.Error("other commands unaffected:", got)
	}
}

func TestTimeoutWithDurationAnswersLate(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := newDirect(t, f, "a", Fault{Kind: FaultTimeout, After: "State", Duration: 150 * time.Millisecond})
	start := time.Now()
	cm(t, d, "State")
	if time.Since(start) < 140*time.Millisecond {
		t.Error("answered too early")
	}
}

// Why: Unknown for Status is how a poller recognises minimal; the fault must
// let a test produce it on demand without changing the image.
func TestUnknownCommandFaultAndAuthFault(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := newDirect(t, f, "a", Fault{Kind: FaultUnknownCommand, After: "Status 2"}, Fault{Kind: FaultAuth, After: "Upgrade 1"})
	if got := cm(t, d, "Status 2"); got != `{"Command":"Unknown","Input":"STATUS 2"}` {
		t.Error(got)
	}
	if got := cm(t, d, "Status 1"); !strings.Contains(got, "StatusPRM") {
		t.Error(got)
	}
	body, code, _ := cmWith(d, "Upgrade 1", time.Second, nil)
	if code != 401 || !strings.Contains(body, "Need user=") {
		t.Error(code, body)
	}
	eq(t, "auth failure must not run the command", d.Phase(), PhaseRun)
}

// Why: WebPassword devices answer 401 until the right credentials are sent.
func TestWebPasswordAuth(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "locked", Profile: ProfileESP8266_4M, User: "admin", Password: "fake-pass"})
	_, code, _ := cmWith(d, "State", time.Second, nil)
	eq(t, "no credentials", code, 401)
	_, code, _ = cmWith(d, "State", time.Second, map[string][]string{"user": {"admin"}, "password": {"wrong"}})
	eq(t, "wrong password", code, 401)
	_, code, _ = cmWith(d, "State", time.Second, map[string][]string{"user": {"admin"}, "password": {"fake-pass"}})
	eq(t, "right password", code, 200)
}

func TestDownloadFailFaultArmedByUpgrade(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	d := newDirect(t, f, "a", Fault{Kind: FaultDownloadFail, After: "Upgrade 1"})
	startUpgrade(t, d, fw, "tasmota.bin.gz")
	if err := d.WaitFor(ctxT(t), func(c Controller) bool { return c.DownloadAttempts() == 10 && c.Phase() == PhaseRun }); err != nil {
		t.Fatal(err)
	}
	eq(t, "server never asked", fw.hits["tasmota.bin.gz"], 0)
	d.ClearFaults()
	cm(t, d, "Upgrade 1")
	settled(t, d, 1)
	eq(t, "version after the fault is gone", d.Version(), "16.1.0")
}

func TestStayOldVersionFault(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	d := newDirect(t, f, "a", Fault{Kind: FaultStayOldVersion})
	startUpgrade(t, d, fw, "tasmota.bin.gz")
	settled(t, d, 1)
	eq(t, "version (flash did not take)", d.Version(), "16.0.0")
}

func TestLongRebootAndRebootGapOption(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "a", Profile: ProfileESP8266_4M, RebootGap: 50 * time.Millisecond,
		Faults: []Fault{{Kind: FaultLongReboot, Duration: 400 * time.Millisecond}}})
	startUpgrade(t, d, fw, "tasmota.bin.gz")
	settled(t, d, 1)
	var at [2]time.Time
	for _, h := range d.History() {
		switch h.Phase { //nolint:exhaustive // only these two matter here
		case PhaseRebooting:
			at[0] = h.At
		case PhaseRun:
			at[1] = h.At
		}
	}
	if gap := at[1].Sub(at[0]); gap < 440*time.Millisecond {
		t.Errorf("reboot gap %v, want >= 450ms (RebootGap + long_reboot)", gap)
	}
}

// Why: "device never comes back" is a terminal scenario for the engine.
func TestNeverReturnsFault(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	d := newDirect(t, f, "a", Fault{Kind: FaultNeverReturns})
	startUpgrade(t, d, fw, "tasmota.bin.gz")
	if err := d.WaitFor(ctxT(t), func(c Controller) bool { return c.Phase() == PhaseRebooting }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	eq(t, "phase", d.Phase(), PhaseRebooting)
	if _, _, err := cmWith(d, "State", 300*time.Millisecond, nil); err == nil {
		t.Error("must stay unreachable")
	}
}

func TestRebootHangAcceptsAndHangs(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{TimeScale: 0.2})
	d := newDirect(t, f, "a", Fault{Kind: FaultRebootHang})
	startUpgrade(t, d, fw, "tasmota.bin.gz")
	if err := d.WaitFor(ctxT(t), func(c Controller) bool { return c.Phase() == PhaseRebooting }); err != nil {
		t.Fatal(err)
	}
	_, _, err := cmWith(d, "State", 150*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("hang mode must accept and not answer, got %v", err)
	}
	settled(t, d, 1)
}

// Why: the SSRF review item: a device told to fetch from a non-loopback
// host must refuse before sending a single packet, never follow redirects
// and never read unbounded data.
func TestDownloadGuards(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	redirect := httptest.NewServer(http.RedirectHandler(fw.URL+"/tasmota.bin.gz", http.StatusFound))
	t.Cleanup(redirect.Close)
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		buf := make([]byte, 1<<20)
		for i := 0; i < 17; i++ {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}))
	t.Cleanup(huge.Close)
	f := newTestFleet(t, "", Extra{})
	for name, url := range map[string]string{
		"non-loopback IP":   "http://192.0.2.1:81/tasmota.bin.gz",
		"redirect":          redirect.URL + "/x.bin.gz",
		"over the size cap": huge.URL + "/x.bin.gz",
		"https":             "https://127.0.0.1:1/x.bin.gz",
	} {
		d := newDirect(t, f, strings.ReplaceAll(name, " ", "-"))
		cm(t, d, "OtaUrl "+url)
		cm(t, d, "Upgrade 1")
		if err := d.WaitFor(ctxT(t), func(c Controller) bool { return c.DownloadAttempts() > 0 && c.Phase() == PhaseRun }); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		eq(t, name+": boots", d.Boots(), 0)
	}
	eq(t, "redirect target was never fetched", fw.hits["tasmota.bin.gz"], 0)
}

func TestListenersBindLoopbackByDefault(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := newDirect(t, f, "a")
	if !strings.HasPrefix(d.Addr(), "127.0.0.1:") {
		t.Error(d.Addr())
	}
}

// Why: the simulator and the embedded broker must never end up in the
// production binary.
func TestNotInProductionBinary(t *testing.T) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "-deps", "github.com/nofuturekid/tasmofleet/cmd/tasmofleet").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, bad := range []string{"internal/simulator", "internal/testutil", "mochi-mqtt"} {
		if strings.Contains(string(out), bad) {
			t.Errorf("cmd/tasmofleet depends on %s", bad)
		}
	}
}

// Why: Fleet.Close must stop everything promptly, also mid-OTA.
func TestCloseIsPromptMidUpdate(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	f, err := NewFleetWith(FleetOptions{FixturesDir: fixtures}, Extra{TimeScale: 1})
	if err != nil {
		t.Fatal(err)
	}
	d := add(t, f, DeviceSpec{Name: "a", Profile: ProfileESP8266_4M})
	startUpgrade(t, d, fw, "tasmota.bin.gz")
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	_ = f.Close()
	if time.Since(start) > 2*time.Second {
		t.Errorf("close took %v", time.Since(start))
	}
}

type collector struct {
	mu   sync.Mutex
	msgs []string // topic=payload
}

func (c *collector) add(topic string, payload []byte, retained bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tag := ""
	if retained {
		tag = "[R]"
	}
	c.msgs = append(c.msgs, tag+topic+"="+string(payload))
}

func (c *collector) has(prefix string) bool { return c.count(prefix) > 0 }

func (c *collector) count(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.msgs {
		if strings.HasPrefix(m, prefix) {
			n++
		}
	}
	return n
}

func (c *collector) waitFor(t *testing.T, prefix string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if c.has(prefix) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("no message %q in %v", prefix, c.msgs)
}

// Why: tasmofleet's client authenticates with HTTP Basic only, while older
// tools use query parameters; the real firmware takes both, so must the
// simulator, or a locked device could not be probed in e2e.
func TestWebPasswordBasicAuth(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "locked", Profile: ProfileESP8266_4M, User: "admin", Password: "fake-pass"})
	get := func(user, pass string, useBasic bool) (int, string) {
		req, err := http.NewRequestWithContext(ctxT(t), http.MethodGet, "http://"+d.Addr()+"/cm?cmnd=State", nil)
		if err != nil {
			t.Fatal(err)
		}
		if useBasic {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b := new(strings.Builder)
		buf := make([]byte, 512)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, b.String()
	}
	if code, body := get("admin", "fake-pass", true); code != 200 || !strings.Contains(body, "UptimeSec") {
		t.Errorf("valid Basic: %d %s", code, body)
	}
	for name, c := range map[string][2]string{"wrong password": {"admin", "nope"}, "wrong user": {"root", "fake-pass"}} {
		if code, body := get(c[0], c[1], true); code != 401 || !strings.Contains(body, `{"WARNING":"Need user=<username>&password=<password>"}`) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if code, _ := get("", "", false); code != 401 {
		t.Errorf("no credentials: %d", code)
	}
	// wrong Basic must not be rescued by nothing, but valid query still works
	_, code, _ := cmWith(d, "State", time.Second, map[string][]string{"user": {"admin"}, "password": {"fake-pass"}})
	eq(t, "query form", code, 200)
}
