//go:build integration

package ota

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

// The push into safeboot (#30) against the simulator. The captured ESP32 (office-monitor-guest)
// has the safeboot layout: Status 4 Free 697 KB, /up calls upl(this). A real tasmota32.bin is 2.2 MB;
// the generated test image is made larger than the 697 KB the running partition has free.
const safebootImageLen = 800 * 1000

// withBigImage makes the upstream tasmota32.bin larger than the free flash of the captured ESP32.
func (e *itEnv) withBigImage() {
	e.t.Helper()
	img, err := simulator.NewImage(itFixtures, "tasmota32.bin", itRelease)
	if err != nil {
		e.t.Fatal(err)
	}
	filler := make([]byte, safebootImageLen)
	for i := range filler {
		filler[i] = byte(i * 7)
	}
	e.override.Store("tasmota32.bin", append(img, filler...))
}

// count is how many recorded device commands start with prefix.
func (e *itEnv) count(dev model.DeviceID, prefix string) int {
	n := 0
	for _, c := range e.ctrl[dev].Commands() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (e *itEnv) indexOf(dev model.DeviceID, prefix string) int {
	for i, c := range e.ctrl[dev].Commands() {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// guardSpy records, at every Guard call, how many commands the device had received by then.
type guardSpy struct {
	selector
	mu    sync.Mutex
	ctrl  simulator.Controller
	marks []int
}

func (g *guardSpy) Guard(id model.Identity, a model.Artifact, url string, c fwselect.Choice) error {
	g.mu.Lock()
	g.marks = append(g.marks, len(g.ctrl.Commands()))
	g.mu.Unlock()
	return g.selector.Guard(id, a, url, c)
}

// startSafeboot starts an engine with the gate open and submits a push for the device.
func (e *itEnv) startSafeboot(dev model.DeviceID) *Engine {
	e.t.Helper()
	e.safebootPush = true
	e.withBigImage()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
	return eng
}

// Why: the whole point of #30. The image is larger than the running partition, so the device is
// restarted into safeboot once, the image is uploaded there once, the app comes back on the new
// version, and nothing of the pull flow (OtaUrl, Upgrade) is used.
func TestITSafebootPushSucceeds(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32})
	orig := e.ctrl[dev].OtaURL()
	e.startSafeboot(dev)
	j := e.expectSuccess(dev, orig, 0, model.PlanDirect)
	if !j.ViaSafeboot || j.Delivery != model.DeliveryPush {
		t.Fatalf("job ViaSafeboot=%v delivery=%q", j.ViaSafeboot, j.Delivery)
	}
	if n := e.count(e.jobOf(dev).DeviceID, "GET /u4 fct"); n != 1 {
		t.Fatalf("%d switches into safeboot, want 1: %v", n, e.ctrl[dev].Commands())
	}
	if n := e.count(dev, "GET /u4 ota"); n != 0 {
		t.Errorf("a successful job sent the way back %d times", n)
	}
	if n := e.uploadsOf(dev); n != 1 {
		t.Fatalf("%d uploads, want 1", n)
	}
	e.assertNoPullCommands(dev)
	if e.indexOf(dev, "GET /u4 fct") > e.indexOf(dev, "POST /u2 tasmota32.bin") {
		t.Errorf("uploaded before the device was switched: %v", e.ctrl[dev].Commands())
	}
	if c := e.ctrl[dev]; c.Image() != "tasmota32" || c.Boots() != 2 {
		t.Errorf("image %s after %d boots, want the app after two (safeboot, then the new app)", c.Image(), c.Boots())
	}
	evs, _ := e.st.JobEvents(context.Background(), j.ID)
	states := map[model.JobState]bool{}
	for _, ev := range evs {
		states[ev.To] = true
	}
	if !states[model.JobSafebootSwitch] || states[model.JobSafebootRecover] {
		t.Errorf("history %+v: want safeboot_switch and no recovery", evs)
	}
}

// Why: Guard must run again between the switch and the upload (the device identity and the artifact
// are checked once more with the device in its new state). Its call must come after the /u4 went
// out and before the image did.
func TestITSafebootGuardRunsBetweenSwitchAndUpload(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32})
	e.safebootPush = true
	e.withBigImage()
	eng := e.newEngine(2) // the spy must sit in the engine before it runs
	spy := &guardSpy{selector: eng.sel, ctrl: e.ctrl[dev]}
	eng.sel = spy
	e.runEngine(eng)
	e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
	if j := e.waitTerminal(dev); j.State != model.JobSucceeded {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	fct, up := e.indexOf(dev, "GET /u4 fct"), e.indexOf(dev, "POST /u2 tasmota32.bin")
	spy.mu.Lock()
	defer spy.mu.Unlock()
	between := 0
	for _, m := range spy.marks {
		if m > fct && m <= up {
			between++
		}
	}
	if fct < 0 || up < 0 || between == 0 {
		t.Fatalf("Guard marks %v, switch at %d, upload at %d: no Guard between the two", spy.marks, fct, up)
	}
	// The call right before the upload must be after the last probe of safeboot, i.e. after the
	// device answered as safeboot: the commands between it and the upload are none.
	last := 0
	for _, m := range spy.marks {
		if m <= up && m > last {
			last = m
		}
	}
	if last < up-1 { // the upload itself is the command at index up; /up is not recorded
		t.Errorf("the last Guard saw %d commands, the upload is command %d: something was sent in between", last, up)
	}
}

// Why: the gate. While it is closed (the shipped state) a device that needs safeboot downloads, as
// before this feature, and the preview says why. Nothing is sent to /u4.
func TestITSafebootGateClosedDownloadsInstead(t *testing.T) {
	e := newItEnv(t)
	e.safebootPush = false // the gate closed, as it was before the real-device test
	e.withBigImage()
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	items, err := eng.Preview(context.Background(), []Request{{DeviceID: dev, Release: itRelease, SkipBackup: true, Delivery: model.DeliveryPush}})
	if err != nil || len(items) != 1 {
		t.Fatalf("Preview: %v %v", items, err)
	}
	if it := items[0]; it.Delivery != model.DeliveryPull || it.ViaSafeboot || it.DeliveryNote != "push into safeboot is not enabled yet" {
		t.Fatalf("item %+v", it)
	}
	e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
	j := e.expectSuccess(dev, orig, 1, model.PlanDirect)
	if j.ViaSafeboot || j.Delivery != model.DeliveryPull {
		t.Fatalf("job ViaSafeboot=%v delivery=%q", j.ViaSafeboot, j.Delivery)
	}
	if n := e.count(dev, "GET /u4"); n != 0 {
		t.Errorf("%d /u4 requests while the gate is closed", n)
	}
	if n := e.uploadsOf(dev); n != 0 {
		t.Errorf("%d uploads while the gate is closed", n)
	}
}

// Why: a dual-partition ESP32 has room for the image in its running partition and takes the plain
// push, with no detour through safeboot (the detour is the risk; it is only taken when needed).
func TestITSafebootDualLayoutPushesDirectly(t *testing.T) {
	e := newItEnv(t)
	e.safebootPush = true
	e.withBigImage()
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32Dual})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
	j := e.expectSuccess(dev, orig, 0, model.PlanDirect)
	if j.ViaSafeboot || j.Delivery != model.DeliveryPush {
		t.Fatalf("job ViaSafeboot=%v delivery=%q", j.ViaSafeboot, j.Delivery)
	}
	if n := e.count(dev, "GET /u4"); n != 0 {
		t.Errorf("%d /u4 requests on a dual layout", n)
	}
	if n := e.uploadsOf(dev); n != 1 {
		t.Errorf("%d uploads", n)
	}
	_ = eng
}

// Why: #191. Status 4 Free is only a hint. A single-OTA device (/up calls upl(this)) whose Free is
// larger than the image must still never get the plain upload into its running app: it goes the
// safeboot route, so the one app partition is not overwritten while it runs.
func TestITSingleOTALargeFreeTakesSafebootRoute(t *testing.T) {
	e := newItEnv(t)
	e.safebootPush = true
	e.withBigImage()
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, FreeKB: 1800})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
	j := e.expectSuccess(dev, orig, 0, model.PlanDirect)
	if !j.ViaSafeboot || j.Delivery != model.DeliveryPush {
		t.Fatalf("job ViaSafeboot=%v delivery=%q", j.ViaSafeboot, j.Delivery)
	}
	if n := e.count(dev, "GET /u4 fct"); n != 1 {
		t.Errorf("%d switches into safeboot, want 1: %v", n, e.ctrl[dev].Commands())
	}
	if e.indexOf(dev, "GET /u4 fct") > e.indexOf(dev, "POST /u2 tasmota32.bin") {
		t.Errorf("uploaded into the running app: %v", e.ctrl[dev].Commands())
	}
}

// failedWithDevice waits for the job to fail and returns it; the device must not be flashed.
func (e *itEnv) failedJob(dev model.DeviceID) model.Job {
	e.t.Helper()
	j := e.waitTerminal(dev)
	if j.State != model.JobFailed {
		e.t.Fatalf("job %s %q%s\ncmds %v", j.State, j.LastError, e.history(), e.ctrl[dev].Commands())
	}
	e.settle(dev)
	return j
}

// Why: the device refuses the image in safeboot (certain: nothing flashed). It must not be left there:
// one way back, the old version running again, the job failed with the words the user needs, and no
// second upload.
func TestITSafebootRefusedUploadIsRecovered(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, Faults: []simulator.Fault{{Kind: simulator.FaultSafebootRefuseUpload}}})
	e.startSafeboot(dev)
	j := e.failedJob(dev)
	if !strings.Contains(j.LastError, "Upload into safeboot failed; the device was switched back to 15.6.0") {
		t.Fatalf("LastError %q", j.LastError)
	}
	c := e.ctrl[dev]
	if c.Image() != "tasmota32" || c.Version() != "15.6.0" {
		t.Fatalf("device runs %s %s, want its old firmware", c.Image(), c.Version())
	}
	if e.count(dev, "GET /u4 fct") != 1 || e.count(dev, "GET /u4 ota") != 1 || e.uploadsOf(dev) != 1 {
		t.Fatalf("fct %d, ota %d, uploads %d: want 1 each\n%v", e.count(dev, "GET /u4 fct"), e.count(dev, "GET /u4 ota"), e.uploadsOf(dev), c.Commands())
	}
	e.assertNoPullCommands(dev)
	e.assertNoViolations()
}

// Why: a device that cannot leave safeboot is the case the instruction exists for. The job tries the
// way back once (never a second /u4, never a second upload), then fails with the address and what
// to do. The upload "succeeds" here and the device comes back in safeboot.
func TestITSafebootStuckDeviceGetsTheManualInstruction(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, Faults: []simulator.Fault{{Kind: simulator.FaultStuckInSafeboot}}})
	e.startSafeboot(dev)
	j := e.failedJob(dev)
	addr := e.ctrl[dev].Addr()
	want := "Device is still in safeboot at " + addr + ": open its web page and upload the firmware, or use Upgrade by download"
	if !strings.Contains(j.LastError, want) {
		t.Fatalf("LastError %q, want it to contain %q", j.LastError, want)
	}
	if e.count(dev, "GET /u4 fct") != 1 || e.count(dev, "GET /u4 ota") != 1 || e.uploadsOf(dev) != 1 {
		t.Fatalf("fct %d, ota %d, uploads %d: want 1 each\n%v", e.count(dev, "GET /u4 fct"), e.count(dev, "GET /u4 ota"), e.uploadsOf(dev), e.ctrl[dev].Commands())
	}
	if e.ctrl[dev].Image() != "safeboot" {
		t.Errorf("the simulated device runs %s", e.ctrl[dev].Image())
	}
}

// Why: a /u4 that gets no answer and does nothing leaves the app running. The job must not upload
// (it never saw safeboot), must not send the switch a second time, and ends with the device on the
// old version.
func TestITSafebootSwitchWithoutAnswerIsNotRepeated(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, Faults: []simulator.Fault{{Kind: simulator.FaultSwitchNoAnswer, After: "u4"}}})
	e.startSafeboot(dev)
	j := e.failedJob(dev)
	if !strings.Contains(j.LastError, "switched back to 15.6.0") || !strings.Contains(j.LastError, "did not answer as safeboot") {
		t.Fatalf("LastError %q", j.LastError)
	}
	if n := e.uploadsOf(dev); n != 0 {
		t.Errorf("%d uploads to a device that never ran safeboot", n)
	}
	// The fault drops every /u4. The app answered the whole time, so the recovery sends nothing.
	if e.count(dev, "GET /u4 fct") != 1 || e.count(dev, "GET /u4 ota") != 0 {
		t.Errorf("fct %d, ota %d, want 1 and 0: %v", e.count(dev, "GET /u4 fct"), e.count(dev, "GET /u4 ota"), e.ctrl[dev].Commands())
	}
}

// Why: a reply lost AFTER the device acted must not make the engine send the switch again or give up:
// it probes, finds safeboot, and carries on as if the reply had arrived.
func TestITSafebootLostSwitchReplyIsProbed(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, Faults: []simulator.Fault{{Kind: simulator.FaultSwitchLostReply, After: "u4"}}})
	orig := e.ctrl[dev].OtaURL()
	e.startSafeboot(dev)
	e.expectSuccess(dev, orig, 0, model.PlanDirect)
	if n := e.count(dev, "GET /u4 fct"); n != 1 {
		t.Fatalf("%d switches, want 1", n)
	}
}

// Why: the upload's reply is lost but the device flashed. The engine must probe, see the new
// version and finish; a second upload (to the new app, which cannot take it) must never happen.
func TestITSafebootLostUploadReplyIsNotResent(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, Faults: []simulator.Fault{{Kind: simulator.FaultUploadLostReply}}})
	orig := e.ctrl[dev].OtaURL()
	e.startSafeboot(dev)
	e.expectSuccess(dev, orig, 0, model.PlanDirect)
	if n := e.uploadsOf(dev); n != 1 {
		t.Fatalf("%d uploads after a lost reply, want 1", n)
	}
	if n := e.count(dev, "GET /u4 ota"); n != 0 {
		t.Errorf("%d recoveries for a successful update", n)
	}
}

// Why: cancelling while the device switches is honoured by bringing it back: the job ends cancelled,
// the device runs its old firmware, and no image was sent.
func TestITSafebootCancelDuringSwitchRecovers(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, Faults: []simulator.Fault{{Kind: simulator.FaultSwitchNoAnswer, After: "u4"}}})
	eng := e.startSafeboot(dev)
	j := e.waitJob(dev, func(j model.Job) bool { return j.State == model.JobSafebootSwitch }, "safeboot_switch")
	if err := eng.Cancel(context.Background(), "it", j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got := e.waitTerminal(dev)
	if got.State != model.JobCancelled || !strings.Contains(got.LastError, "switched back to 15.6.0") {
		t.Fatalf("job %s %q", got.State, got.LastError)
	}
	if n := e.uploadsOf(dev); n != 0 {
		t.Errorf("%d uploads after a cancel", n)
	}
}

// Why: tasmofleet restarts while the device is in safeboot waiting for its upload (the process is
// stopped, then started again). The job must neither switch again nor give up: it resumes by
// probing and uploads once.
func TestITSafebootResumeAfterRestartDoesNotSwitchAgain(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32})
	orig := e.ctrl[dev].OtaURL()
	e.startSafeboot(dev)
	// The process exits as soon as the device got the switch: the job is stored as safeboot_switch.
	for deadline := time.Now().Add(30 * time.Second); e.count(dev, "GET /u4 fct") == 0; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the device never got the switch")
		}
	}
	e.stop()
	if j := e.jobOf(dev); j.State != model.JobSafebootSwitch {
		t.Fatalf("job stored as %s, want safeboot_switch", j.State)
	}
	e.start(2)
	e.expectSuccess(dev, orig, 0, model.PlanDirect)
	if n := e.count(dev, "GET /u4 fct"); n != 1 {
		t.Fatalf("%d switches across a restart, want 1: %v", n, e.ctrl[dev].Commands())
	}
	if n := e.uploadsOf(dev); n != 1 {
		t.Fatalf("%d uploads, want 1", n)
	}
}

// Why: a safeboot whose Wi-Fi join is slower than the switch window keeps the device offline the
// whole time. The switch back must not be sent into that silence (it would be lost and the job
// would report a device "still in safeboot" that is merely late): the recovery probes first and
// sends it on the first answer from safeboot.
func TestITSafebootSlowSafebootIsRecoveredOnItsFirstAnswer(t *testing.T) {
	e := newItEnv(t)
	// The reboot gap (real time) is longer than the switch window (2 nominal minutes at timeScale).
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, RebootGap: 6 * time.Second})
	e.startSafeboot(dev)
	j := e.failedJob(dev)
	// The gap applies to the way back as well, so the device may be late again: what matters is that the
	// switch back was sent exactly once, after the first answer from safeboot, and no image was.
	if !strings.Contains(j.LastError, "did not answer as safeboot in time") {
		t.Fatalf("LastError %q", j.LastError)
	}
	if e.count(dev, "GET /u4 fct") != 1 || e.count(dev, "GET /u4 ota") != 1 || e.uploadsOf(dev) != 0 {
		t.Fatalf("fct %d, ota %d, uploads %d: want 1, 1, 0\n%v", e.count(dev, "GET /u4 fct"), e.count(dev, "GET /u4 ota"), e.uploadsOf(dev), e.ctrl[dev].Commands())
	}
	if c := e.ctrl[dev]; c.Image() != "tasmota32" || c.Version() != "15.6.0" {
		t.Errorf("device runs %s %s", c.Image(), c.Version())
	}
}
