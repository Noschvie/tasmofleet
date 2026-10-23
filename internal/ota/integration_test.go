//go:build integration

package ota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

const fakePassword = "not-a-real-password" // obviously fake, only for the simulator

// expectSuccess waits for the device's job and checks the final state of
// the device: new version, OtaUrl back where it was, the expected number of
// Upgrade commands, and no Upgrade into a running download.
func (e *itEnv) expectSuccess(dev model.DeviceID, orig string, upgrades int, plan model.Plan) model.Job {
	e.t.Helper()
	return e.expectSuccessNote(dev, orig, upgrades, plan, "")
}

// expectSuccessNote is expectSuccess for a job that ends succeeded with a
// note in LastError containing wantNote (empty = no note at all).
func (e *itEnv) expectSuccessNote(dev model.DeviceID, orig string, upgrades int, plan model.Plan, wantNote string) model.Job {
	e.t.Helper()
	j := e.waitTerminal(dev)
	if j.State != model.JobSucceeded || (wantNote == "" && j.LastError != "") || !strings.Contains(j.LastError, wantNote) {
		e.t.Fatalf("job %s %q%s\ncmds %v", j.State, j.LastError, e.history(), e.ctrl[dev].Commands())
	}
	if plan != "" && j.Plan != plan {
		e.t.Fatalf("plan %s, want %s", j.Plan, plan)
	}
	e.settle(dev)
	c := e.ctrl[dev]
	if c.Version() != itRelease {
		e.t.Fatalf("device runs %s %s, want %s", c.Image(), c.Version(), itRelease)
	}
	if n := e.upgrades(dev); n != upgrades {
		e.t.Fatalf("Upgrade sent %d times, want %d", n, upgrades)
	}
	if got := c.OtaURL(); got != orig {
		e.t.Fatalf("OtaUrl after the update is %q, want the original %q", got, orig)
	}
	e.assertNoViolations()
	return j
}

// The staged 1 MB ESP8266 flow end to end: two hops via minimal, one Upgrade
// per hop, and the OtaUrl the device boots with is the original again (the
// minimal URL left behind by TasmoAdmin is the bug this flow avoids).
func TestITStagedESP8266(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	j := e.expectSuccess(dev, orig, 2, model.PlanStaged)
	if e.ctrl[dev].Image() != "tasmota" {
		t.Fatalf("device runs image %s", e.ctrl[dev].Image())
	}
	evs, _ := e.st.JobEvents(context.Background(), j.ID)
	if len(evs) == 0 || evs[len(evs)-1].To != model.JobSucceeded {
		t.Fatalf("history does not end in succeeded: %+v", evs)
	}
}

// Tasmota's own fallback: the plan says direct (small image), the 1 MB
// device cannot take it, loads minimal by itself and continues. The engine
// must follow with a single Upgrade and not mistake the minimal sighting
// for a failure or send a second Upgrade.
func TestITAutoContinueViaMinimal(t *testing.T) {
	e := newItEnv(t)
	e.bigImages.Store(false)
	// Nothing is pre-seeded: planning a direct ESP8266 update must itself put
	// tasmota-minimal.bin.gz in the cache, because Tasmota's fallback derives
	// its URL from the directory of OtaUrl and would hit a 404 otherwise.
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
}

func TestITDirectESP8266(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
}

// Every ESP32 chip gets the image of its own family and one direct Upgrade.
func TestITDirectESP32PerChip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile simulator.Profile
		image   string
	}{
		{"esp32", simulator.ProfileESP32, "tasmota32"},
		{"esp32c3", simulator.ProfileESP32C3, "tasmota32c3"},
		{"esp32s3", simulator.ProfileESP32S3, "tasmota32s3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newItEnv(t)
			dev := e.addDevice(simulator.DeviceSpec{Profile: tc.profile})
			orig := e.ctrl[dev].OtaURL()
			eng := e.start(2)
			e.submit(eng, Request{DeviceID: dev})
			e.expectSuccess(dev, orig, 1, model.PlanDirect)
			if got := e.ctrl[dev].Image(); got != tc.image {
				t.Fatalf("device runs image %q, want %q", got, tc.image)
			}
		})
	}
}

// Unknown hardware is refused at Submit and the device is only read, never
// configured: safety rule 1, no guessing.
func TestITUnknownChipRefused(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileUnknownChip})
	eng := e.start(2)
	_, err := eng.Submit(context.Background(), "it", []Request{{DeviceID: dev, Release: itRelease, SkipBackup: true}})
	var re *RefusedError
	if !errors.As(err, &re) || len(re.Refused) != 1 {
		t.Fatalf("Submit: %v, want a RefusedError for the device", err)
	}
	for _, c := range e.ctrl[dev].Commands() {
		lc := strings.ToLower(c)
		if strings.HasPrefix(lc, "upgrade 1") || strings.HasPrefix(lc, "otaurl http") {
			t.Fatalf("refused device received %q", c)
		}
	}
	if act, _ := e.st.ActiveJobs(context.Background()); len(act) != 0 {
		t.Fatalf("a job was created: %+v", act)
	}
}

// An upstream file whose header belongs to another chip family never gets
// served to a device: the cache rejects it, so the update is refused.
func TestITWrongFamilyArtifactRefused(t *testing.T) {
	e := newItEnv(t)
	c3, err := simulator.NewImage(itFixtures, "tasmota32c3.bin", itRelease)
	if err != nil {
		t.Fatal(err)
	}
	e.override.Store("tasmota32.bin", c3) // "tasmota32.bin" with a C3 header
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32})
	eng := e.start(2)
	_, err = eng.Submit(context.Background(), "it", []Request{{DeviceID: dev, Release: itRelease, SkipBackup: true}})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Submit: %v, want ErrRefused", err)
	}
	for _, c := range e.ctrl[dev].Commands() {
		if strings.HasPrefix(strings.ToLower(c), "upgrade 1") {
			t.Fatalf("device received %q", c)
		}
	}
}

// A different variant than the one running needs the explicit flag.
func TestITVariantChangeNeedsFlag(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	eng := e.start(2)
	_, err := eng.Submit(context.Background(), "it", []Request{{DeviceID: dev, Release: itRelease, Variant: "lite", SkipBackup: true}})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Submit without flag: %v, want ErrRefused", err)
	}
	orig := e.ctrl[dev].OtaURL()
	e.submit(eng, Request{DeviceID: dev, Variant: "lite", AllowVariantChange: true})
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
	if got := e.ctrl[dev].Image(); got != "lite" {
		t.Fatalf("device runs %q, want lite", got)
	}
}

// HTTP Basic auth (WebPassword): the engine uses the inventory credentials
// for every command, including the download-time probes.
func TestITPasswordProtectedDevice(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M, User: "admin", Password: fakePassword})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccess(dev, orig, 2, model.PlanStaged)
}

// Wrong credentials fail the job in preflight, change nothing on the device
// and the password appears in no error text or history row.
func TestITWrongPasswordFailsWithoutLeak(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, User: "admin", Password: fakePassword})
	d, _ := e.st.GetDevice(context.Background(), dev)
	d.HTTPPassword = "wrong-fake-password"
	if err := e.st.UpdateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	eng := e.start(2)
	_, err := eng.Submit(context.Background(), "it", []Request{{DeviceID: dev, Release: itRelease, SkipBackup: true}})
	var re *RefusedError
	if !errors.As(err, &re) {
		t.Fatalf("Submit: %v, want refusal (preview cannot read the device)", err)
	}
	for _, s := range []string{err.Error()} {
		if strings.Contains(s, "fake-password") {
			t.Fatalf("password leaked: %s", s)
		}
	}
	for _, c := range e.ctrl[dev].Commands() {
		if strings.HasPrefix(strings.ToLower(c), "otaurl http") || strings.HasPrefix(strings.ToLower(c), "upgrade 1") {
			t.Fatalf("device received %q without valid credentials", c)
		}
	}
}

// A device without MQTT (HTTP only) is updated by probes alone: nothing in
// the engine depends on MQTT hints.
func TestITHTTPOnlyDevice(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M, Faults: []simulator.Fault{{Kind: simulator.FaultHTTPOnly}}})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccess(dev, orig, 2, model.PlanStaged)
}

// ---- faults ----

func faultAfterUpgrade(kind string, d time.Duration) simulator.Fault {
	return simulator.Fault{After: "Upgrade 1", Kind: kind, Duration: d}
}

// The device stays offline longer than UpgradeAccept (3 min) but within the
// RebootWindow: that is normal for a slow Wi-Fi join, not a failure, and no
// second Upgrade is sent.
func TestITRebootGapLongerThanAcceptWindow(t *testing.T) {
	e := newItEnv(t)
	gap := time.Duration(float64(4*time.Minute) * timeScale) // 4 nominal minutes
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{faultAfterUpgrade(simulator.FaultLongReboot, gap)}})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
}

// The device flashes and never answers again: the job fails with a reason
// after the reboot window and Upgrade was sent exactly once.
func TestITDeviceNeverReturns(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{faultAfterUpgrade(simulator.FaultNeverReturns, 0)}})
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	j := e.waitTerminal(dev)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, ErrTextNotBack) {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if n := e.upgrades(dev); n != 1 {
		t.Fatalf("Upgrade sent %d times, want 1: the device may still be flashing", n)
	}
	e.assertNoViolations()
}

// Download failure ("File Not Found (404)"): the device keeps its old
// firmware; the engine retries once after the accept window and then fails
// with the reason. Never more than MaxAttempts Upgrades.
func TestITDownloadFailRetriesThenFails(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{faultAfterUpgrade(simulator.FaultDownloadFail, 0)}})
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	j := e.waitTerminal(dev)
	if j.State != model.JobFailed {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if n := e.upgrades(dev); n != e.machine.MaxAttempts {
		t.Fatalf("Upgrade sent %d times, want %d", n, e.machine.MaxAttempts)
	}
	if got := e.ctrl[dev].Version(); got != "15.6.0" {
		t.Fatalf("device version %s changed although every download failed", got)
	}
	e.assertNoViolations()
}

// The flash "succeeds" but the device boots the old version: verify sees
// that nothing changed, retries once, then fails loudly.
func TestITStayOldVersionFails(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{faultAfterUpgrade(simulator.FaultStayOldVersion, 0)}})
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	j := e.waitTerminal(dev)
	if j.State != model.JobFailed {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if n := e.upgrades(dev); n > e.machine.MaxAttempts {
		t.Fatalf("Upgrade sent %d times, more than %d", n, e.machine.MaxAttempts)
	}
	e.assertNoViolations()
}

// The device itself rejects the image header ("Verify Bin Header Failed")
// whatever it is sent: a bounded number of attempts, then failed.
func TestITDeviceRejectsHeader(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{faultAfterUpgrade(simulator.FaultWrongFamily, 0)}})
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	j := e.waitTerminal(dev)
	if j.State != model.JobFailed {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if n := e.upgrades(dev); n > e.machine.MaxAttempts {
		t.Fatalf("Upgrade sent %d times", n)
	}
	e.assertNoViolations()
}

// The Upgrade reply is lost (connection dropped after the command ran): the
// engine probes and follows the update instead of resending.
func TestITLostUpgradeResponse(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{{After: "Upgrade 1", Kind: simulator.FaultDrop}}})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
}

// Another tool changes OtaUrl between our OtaUrl and Upgrade: the device
// would flash a file nobody checked, so the job fails on the echo.
func TestITOtaUrlChangedBySomeoneElse(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	e.beforeUpgrade = func(id model.DeviceID) { e.ctrl[id].SetOtaURL("http://127.0.0.1:1/other.bin.gz") }
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	j := e.waitTerminal(dev)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, ErrTextOtaURLChanged) {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
}

// ---- fleet ----

// Mixed fleet under MaxParallel=2: every job succeeds, and never more than
// two jobs are past "queued" at the same time.
func TestITMixedFleetRespectsMaxParallel(t *testing.T) {
	e := newItEnv(t)
	specs := []simulator.DeviceSpec{
		{Profile: simulator.ProfileESP8266_1M},
		{Profile: simulator.ProfileESP8266_1M, User: "admin", Password: fakePassword},
		{Profile: simulator.ProfileESP8266_4M},
		{Profile: simulator.ProfileESP32},
		{Profile: simulator.ProfileESP32C3},
	}
	var devs []model.DeviceID
	origs := map[model.DeviceID]string{}
	var reqs []Request
	for _, s := range specs {
		id := e.addDevice(s)
		devs = append(devs, id)
		origs[id] = e.ctrl[id].OtaURL()
		reqs = append(reqs, Request{DeviceID: id})
	}
	eng := e.start(2)
	e.submit(eng, reqs...)
	for _, id := range devs {
		j := e.waitTerminal(id)
		e.expectSuccess(id, origs[id], map[bool]int{true: 2, false: 1}[j.Plan == model.PlanStaged], "")
	}
	if peak := e.log.peakBusy(); peak > 2 {
		t.Fatalf("%d jobs were past queued at once, MaxParallel is 2", peak)
	}
}

// A batch cancel while the first job flashes: that job finishes its stage
// (the device cannot be stopped) and ends cancelled; the queued ones never
// touch their devices.
func TestITBatchCancel(t *testing.T) {
	e := newItEnv(t)
	d1 := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M})
	d2 := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	d3 := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32})
	eng := e.start(1)
	b := e.submit(eng, Request{DeviceID: d1}, Request{DeviceID: d2}, Request{DeviceID: d3})
	e.waitJob(d1, func(j model.Job) bool { return j.State == model.JobUpgradeIssued }, "upgrade_issued")
	if err := eng.CancelBatch(context.Background(), "it", b); err != nil {
		t.Fatal(err)
	}
	for _, d := range []model.DeviceID{d1, d2, d3} {
		if j := e.waitTerminal(d); j.State != model.JobCancelled {
			t.Fatalf("device %d: job %s %q", d, j.State, j.LastError)
		}
	}
	if n := e.upgrades(d1); n != 1 {
		t.Fatalf("flashing device got %d Upgrades, want 1 (minimal stage only)", n)
	}
	for _, d := range []model.DeviceID{d2, d3} {
		if n := e.upgrades(d); n != 0 {
			t.Fatalf("queued device %d got %d Upgrades", d, n)
		}
	}
	e.assertNoViolations()
}

// ---- process restart ----

type restartCase struct {
	state model.JobState
	nth   int
}

// The process dies right after a state was persisted and before its actions
// ran, in every non-terminal state and in both stages. A new engine on the
// same database must finish the job without ever sending Upgrade into a
// running download, and with one Upgrade per stage in total.
func TestITRestartInEveryState(t *testing.T) {
	staged := []restartCase{
		{model.JobPreflight, 1}, {model.JobBackup, 1},
		{model.JobSetOtaURL, 1}, {model.JobUpgradeIssued, 1}, {model.JobAwaitReboot, 1}, {model.JobVerify, 1},
		{model.JobSetOtaURL, 2}, {model.JobUpgradeIssued, 2}, {model.JobAwaitReboot, 2}, {model.JobVerify, 2},
		{model.JobRestoreOtaURL, 1},
	}
	for _, tc := range staged {
		t.Run(fmt.Sprintf("staged_%s_%d", tc.state, tc.nth), func(t *testing.T) {
			runRestart(t, simulator.ProfileESP8266_1M, tc, 2, model.PlanStaged)
		})
	}
	direct := []restartCase{{model.JobSetOtaURL, 1}, {model.JobUpgradeIssued, 1}, {model.JobAwaitReboot, 1}, {model.JobVerify, 1}, {model.JobRestoreOtaURL, 1}}
	for _, tc := range direct {
		t.Run(fmt.Sprintf("esp32_%s_%d", tc.state, tc.nth), func(t *testing.T) {
			runRestart(t, simulator.ProfileESP32, tc, 1, model.PlanDirect)
		})
	}
	t.Run("queued", func(t *testing.T) {
		e := newItEnv(t)
		dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M})
		orig := e.ctrl[dev].OtaURL()
		e.submit(e.newEngine(2), Request{DeviceID: dev}) // submitted, engine not running
		if j := e.jobOf(dev); j.State != model.JobQueued {
			t.Fatalf("job %s", j.State)
		}
		e.start(2)
		e.expectSuccess(dev, orig, 2, model.PlanStaged)
	})
}

func runRestart(t *testing.T, p simulator.Profile, tc restartCase, upgrades int, plan model.Plan) {
	e := newItEnv(t)
	e.keepBackup = true
	dev := e.addDevice(simulator.DeviceSpec{Profile: p})
	orig := e.ctrl[dev].OtaURL()
	crashed := e.crashAt(tc.state, tc.nth)
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	select {
	case <-crashed:
	case <-time.After(60 * time.Second):
		t.Fatalf("job never reached %s #%d", tc.state, tc.nth)
	}
	e.stop()
	e.clearHook()
	if j := e.jobOf(dev); j.State.Terminal() {
		t.Fatalf("job already %s at the crash point", j.State)
	}
	e.start(2)
	e.expectSuccess(dev, orig, upgrades, plan)
}

// The process dies while the device is downloading (Upgrade already sent):
// the resumed engine must only observe.
func TestITRestartWhileDownloading(t *testing.T) {
	for _, stage := range []int{1, 2} {
		t.Run(fmt.Sprintf("hop%d", stage), func(t *testing.T) {
			e := newItEnv(t)
			dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M})
			orig := e.ctrl[dev].OtaURL()
			eng := e.start(2)
			e.submit(eng, Request{DeviceID: dev})
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			wantUpgrades := stage
			err := e.ctrl[dev].WaitFor(ctx, func(c simulator.Controller) bool {
				return e.upgrades(dev) == wantUpgrades && c.Phase() == simulator.PhaseDownloading
			})
			if err != nil {
				t.Fatalf("device never downloaded for hop %d: %v", stage, err)
			}
			e.stop()
			e.start(2)
			e.expectSuccess(dev, orig, 2, model.PlanStaged)
		})
	}
}

// ---- MQTT hint ----

// The device publishes "Failed ..." on stat/<t>/UPGRADE when its download
// dies; wired like the application does it (bus -> Engine.Hint), the retry
// starts at once instead of after the 3 minute accept window. Nothing
// depends on the hint: the HTTP-only variant of this scenario
// (TestITDownloadFailRetriesThenFails) reaches the same end state.
func TestITMQTTFailedReportShortensTheWait(t *testing.T) {
	broker, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	e := newItEnvMQTT(t, broker.URL())
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{faultAfterUpgrade(simulator.FaultDownloadFail, 0)}})
	eng := e.start(2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: broker.URL(), ClientID: "tasmofleet-it"})
	if err != nil {
		t.Fatal(err)
	}
	unsub := bus.Subscribe([]mqttbus.Kind{mqttbus.KindUpgrade}, func(m mqttbus.Message) {
		var p struct{ Upgrade string }
		if json.Unmarshal(m.Payload, &p) != nil {
			return
		}
		d, err := e.st.GetDeviceByTopic(context.Background(), m.Topic)
		if err != nil {
			return
		}
		eng.Hint(d.ID, p.Upgrade)
	})
	defer unsub()
	deadline := time.Now().Add(10 * time.Second)
	for !bus.Connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// MQTT subscriptions are established asynchronously; give the broker a moment.
	time.Sleep(300 * time.Millisecond)

	e.submit(eng, Request{DeviceID: dev})
	j := e.waitTerminal(dev)
	if j.State != model.JobFailed {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	e.log.mu.Lock()
	defer e.log.mu.Unlock()
	var issued time.Time
	hinted := false
	for _, ev := range e.log.evs {
		if ev.To == model.JobUpgradeIssued && issued.IsZero() {
			issued = ev.At
		}
		if ev.Event == string(EvUpgradeReport) {
			hinted = true
			if wait := ev.At.Sub(issued); wait >= e.machine.UpgradeAccept {
				t.Fatalf("hint arrived after %v, not before the accept window of %v", wait, e.machine.UpgradeAccept)
			}
		}
	}
	if !hinted {
		t.Fatalf("no upgrade_report transition; history:%s", e.historyLocked())
	}
}
