package ota

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Power restore through the engine with fakes. The point of the feature:
// The maintainer runs PowerOnState 0 and SetOption0 0, so a device that was ON must
// not come back OFF after an update.

func relaysOn(ns ...int) model.RelaySet {
	var s model.RelaySet
	for _, n := range ns {
		s = s.Add(n)
	}
	return s
}

// powerDevice is a device whose relays in on are ON, which are light channels
// (lights are what the restore handles by default) and which loses its state
// when it is flashed.
func powerDevice(on model.RelaySet) *fakeDevice {
	d := plugDevice()
	d.power, d.light, d.flashClearsPower = on, on, true
	return d
}

// otaActor is the audit actor of a restore command.
func otaActor(job model.JobID, user string) string {
	return "tasmofleet (OTA restore for job " + strconv.Itoa(int(job)) + ", requested by " + user + ")"
}

func powerCalls(h *harness) []string {
	var out []string
	for _, e := range h.rec.entries() {
		if strings.HasPrefix(e, "power ") {
			out = append(out, e)
		}
	}
	return out
}

func TestPowerRestoredAfterStagedUpdate(t *testing.T) {
	h := newHarness(t, 2, powerDevice(relaysOn(1, 3)))
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)

	j := h.job(t, id)
	if j.State != model.JobSucceeded || j.LastError != "" {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if j.PowerOn != relaysOn(1, 3) {
		t.Fatalf("PowerOn = %b, the state read in preflight must be persisted", j.PowerOn)
	}
	if got := h.api.dev(1).power; got != relaysOn(1, 3) {
		t.Fatalf("device power %b, want relays 1 and 3 ON", got)
	}
	want := []string{"power 1 relay 1 ON by " + otaActor(1, "alice"), "power 1 relay 3 ON by " + otaActor(1, "alice")}
	if got := powerCalls(h); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("power calls %v, want %v (relay 2 was OFF and must not be touched)", got, want)
	}
	// Only after the last Upgrade and the OtaUrl restore: switching earlier
	// would be undone by the flash.
	log := h.rec.entries()
	lastOther := -1
	firstPower := -1
	for i, e := range log {
		switch {
		case strings.HasPrefix(e, "power "):
			if firstPower < 0 {
				firstPower = i
			}
		case strings.HasPrefix(e, "upgrade") || strings.HasPrefix(e, "set_ota_url"):
			lastOther = i
		}
	}
	if firstPower < lastOther {
		t.Fatalf("power switched before the update was finished: %v", log)
	}
	assertWriteAhead(t, log)
}

// A device that restores its relay itself (plain soft restart on ESP8266
// with RTC memory) is left alone.
func TestPowerNotTouchedWhenDeviceRestoredItself(t *testing.T) {
	d := powerDevice(relaysOn(1))
	d.flashClearsPower = false
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	if j := h.job(t, id); j.State != model.JobSucceeded {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 0 {
		t.Fatalf("power commands %v although the relay was ON already", got)
	}
}

// A relay that is ON after the update but was OFF before is somebody else's
// decision (a timer, a rule, a person): no surprise switching, in either
// direction.
func TestPowerLeavesOtherRelaysAlone(t *testing.T) {
	d := powerDevice(relaysOn(1))
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	// the device comes back with relay 2 ON (e.g. a rule) and relay 1 OFF
	d.bootPower = relaysOn(2)
	h.run(t, h.e)
	if j := h.job(t, id); j.State != model.JobSucceeded {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := d.power; got != relaysOn(1, 2) {
		t.Fatalf("device power %b, want relay 1 restored and relay 2 untouched", got)
	}
	for _, c := range powerCalls(h) {
		if !strings.Contains(c, "relay 1 ON") {
			t.Fatalf("unexpected power command %q", c)
		}
	}
}

func TestPowerRestoreCanBeSkipped(t *testing.T) {
	h := newHarness(t, 2, powerDevice(relaysOn(1)))
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipPowerRestore: true})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || j.PowerOn != 0 || !j.SkipPowerRestore {
		t.Fatalf("job %+v", j)
	}
	if got := powerCalls(h); len(got) != 0 {
		t.Fatalf("power commands %v although the restore was turned off", got)
	}
}

// A relay that cannot be switched does not undo the update; the job says
// what is left to do.
func TestPowerRestoreFailureIsReported(t *testing.T) {
	for name, set := range map[string]func(*fakePower){
		"command fails":   func(p *fakePower) { p.fail = map[int]bool{3: true} },
		"relay stays OFF": func(p *fakePower) { p.ignore = map[int]bool{3: true} },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 2, powerDevice(relaysOn(1, 3)))
			set(h.power)
			id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
			h.run(t, h.e)
			j := h.job(t, id)
			if j.State != model.JobSucceeded || !strings.Contains(j.LastError, ErrTextPowerFailed) || !strings.Contains(j.LastError, "3") {
				t.Fatalf("job %s %q", j.State, j.LastError)
			}
			if got := h.api.dev(1).power; !got.Has(1) {
				t.Fatalf("relay 1 must still be restored, power %b", got)
			}
		})
	}
}

func TestPowerRestoreWithoutSwitchSaysSo(t *testing.T) {
	h := newHarness(t, 2, powerDevice(relaysOn(1)))
	h.e.power = nil
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, "no power switch") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
}

// If the relay state cannot be read in preflight the job stops before any
// OtaUrl/Upgrade: no update without knowing what to restore.
func TestPowerUnreadableFailsPreflight(t *testing.T) {
	d := powerDevice(relaysOn(1))
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	d.readPowerErr = errors.New("status 11: timeout")
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "relay state unreadable") || !strings.Contains(j.LastError, "do not switch relays back on") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if n := h.rec.count("set_ota_url") + h.rec.count("upgrade"); n != 0 {
		t.Fatalf("device was changed: %v", h.rec.entries())
	}
}

// A device that already runs minimal has no Status 11; nothing is read and
// nothing is restored.
func TestPowerNotReadOnMinimal(t *testing.T) {
	d := &fakeDevice{ident: minimalIdent("15.6.0"), uptime: 0, otaURL: minimalURL, readPowerErr: errors.New("must not be read")}
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
	h.run(t, h.e)
	if j := h.job(t, id); j.State == model.JobFailed && strings.Contains(j.LastError, "relay state") {
		t.Fatalf("power was read on minimal: %s", j.LastError)
	}
}

func TestPreviewShowsPowerRestore(t *testing.T) {
	h := newHarness(t, 2, powerDevice(relaysOn(1, 3)), powerDevice(relaysOn(1)))
	items, err := h.e.Preview(context.Background(), []Request{
		{DeviceID: 1, Release: "16.0.0"},
		{DeviceID: 2, Release: "16.0.0", SkipPowerRestore: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].PowerRestore != "1, 3" || items[1].PowerRestore != "" {
		t.Fatalf("PowerRestore %q / %q", items[0].PowerRestore, items[1].PowerRestore)
	}
	// unreadable is not the same as nothing to restore
	h.api.dev(1).readPowerErr = errors.New("timeout")
	items, _ = h.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.0.0"}})
	if !items[0].PowerUnreadable || items[0].PowerRestore != "" {
		t.Fatalf("preview %+v, want unreadable", items[0])
	}
}

// Restart in the restore step: the process dies right after restore_power
// was persisted, and again after one relay was switched. The resumed job
// reads first and only switches what is still OFF.
func TestPowerRestoreSurvivesRestart(t *testing.T) {
	d := powerDevice(relaysOn(1, 3))
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})

	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	crashed := make(chan struct{})
	h.st.onSave = func(j model.Job) {
		if j.State == model.JobRestorePower {
			once.Do(func() { cancel(); close(crashed) })
		}
	}
	done := make(chan error, 1)
	go func() { done <- h.e.Run(ctx) }()
	<-crashed
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	h.st.onSave = nil
	if j := h.job(t, id); j.State != model.JobRestorePower {
		t.Fatalf("job %s after the crash, want restore_power", j.State)
	}
	if got := powerCalls(h); len(got) != 0 {
		t.Fatalf("power switched before the crash point: %v", got)
	}
	// relay 1 was switched by hand while the engine was down
	d.power = d.power.Add(1)

	h.run(t, h.newEngine(2))
	if j := h.job(t, id); j.State != model.JobSucceeded || j.LastError != "" {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 1 || !strings.Contains(got[0], "relay 3 ON") {
		t.Fatalf("power calls after resume %v, want only relay 3", got)
	}
	if d.power != relaysOn(1, 3) {
		t.Fatalf("device power %b", d.power)
	}
}

// One pass: a relay that is OFF after the switch (or the verify read) stays
// OFF and the job says so. Sending the command again could override someone
// who switched it off in between.
func TestPowerRestoreIsSinglePass(t *testing.T) {
	h := newHarness(t, 2, powerDevice(relaysOn(1)))
	h.power.ignore = map[int]bool{1: true}
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	if got := powerCalls(h); len(got) != 1 {
		t.Fatalf("%d power calls, want exactly 1", len(got))
	}
	if j := h.job(t, id); !strings.Contains(j.LastError, "still OFF") {
		t.Fatalf("LastError %q", j.LastError)
	}
}

// Someone switches a relay off between preflight and the Upgrade (here during
// the backup): the second reading sees it, the relay stays off, the job says
// so.
func TestPowerSwitchedOffDuringBackupIsNotRestored(t *testing.T) {
	d := powerDevice(relaysOn(1, 3))
	h := newHarness(t, 2, d)
	h.backups.hook = func() { d.power = relaysOn(3) }
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, "relays 1 were not ON right before the update") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if d.power != relaysOn(3) {
		t.Fatalf("device power %b: relay 1 must stay OFF, relay 3 comes back", d.power)
	}
	for _, c := range powerCalls(h) {
		if !strings.Contains(c, "relay 3 ON") {
			t.Fatalf("unexpected power command %q", c)
		}
	}
}

// An MQTT report that a relay was switched off, arriving before the
// download starts, removes it from the candidates too.
func TestPowerReportDropsRelay(t *testing.T) {
	h := newHarness(t, 2, powerDevice(relaysOn(1, 3)))
	h.backups.hook = func() { h.e.PowerReport(1, 3, false) }
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, "relays 3 were switched off during the update") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := h.api.dev(1).power; got != relaysOn(1) {
		t.Fatalf("device power %b, want only relay 1 restored", got)
	}
	h.e.PowerReport(1, 1, true) // ON reports and unknown devices are ignored
	h.e.PowerReport(99, 1, false)
}

// The first reading after the boot is the evidence: a relay that is ON
// there was kept by the device and is never touched, even if somebody
// switches it off right after. (Restoring it would override that person.)
func TestPowerKeptByDeviceIsNeverSwitchedAgain(t *testing.T) {
	d := powerDevice(relaysOn(1))
	d.flashClearsPower = false
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	reads := 0
	d.onRead = func(int) {
		// readings: preflight, before the Upgrade, two after the boot
		if reads++; reads == 4 {
			d.power = 0 // the user switches it off right after the settled post-boot reading
		}
	}
	h.run(t, h.e)
	if j := h.job(t, id); j.State != model.JobSucceeded || j.LastError != "" {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 0 {
		t.Fatalf("power commands %v: the relay was kept by the device and then switched off by someone", got)
	}
	if d.power != 0 {
		t.Fatalf("device power %b, the user's OFF must stay", d.power)
	}
}

// PulseTime, Interlock and shutter relays are actions, not states: they are
// never restored, and the job says which ones were left.
func TestPowerRestrictedRelaysAreSkipped(t *testing.T) {
	d := powerDevice(relaysOn(1, 2, 9))
	d.restricted = relaysOn(2)
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, "relays 2, 9 are ON or unclear but not restored") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 1 || !strings.Contains(got[0], "relay 1 ON") {
		t.Fatalf("power calls %v, want relay 1 only", got)
	}
}

// A restart that takes longer than the maximum age does not switch anything
// on: the state is too old to know what is wanted.
func TestPowerRestoreRefusedWhenStateIsOld(t *testing.T) {
	d := powerDevice(relaysOn(1))
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})

	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	crashed := make(chan struct{})
	h.st.onSave = func(j model.Job) {
		if j.State == model.JobRestorePower {
			once.Do(func() { cancel(); close(crashed) })
		}
	}
	done := make(chan error, 1)
	go func() { done <- h.e.Run(ctx) }()
	<-crashed
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	h.st.onSave = nil
	h.clk.mu.Lock()
	h.clk.now = h.clk.now.Add(testCfg().PowerRestoreMaxAge + time.Minute)
	h.clk.mu.Unlock()

	h.run(t, h.newEngine(2))
	j := h.job(t, id)
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, "too old") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 0 {
		t.Fatalf("power switched on from an old state: %v", got)
	}
}

// ---- uncertain input fails closed ----
//
// Readings are numbered per test: after Submit's preview the job reads in
// preflight (1), right before the first Upgrade (2), twice after the final
// boot until two equal readings settled it (3, 4) and when the restore step
// starts (5).

// newReads installs hook on device d, counting from the first reading of the job.
func newReads(d *fakeDevice, hook func(n int)) {
	reads := 0
	d.onRead = func(int) { reads++; hook(reads) }
}

func TestPowerUnclearRelayAtPreflightIsNotRestored(t *testing.T) {
	d := powerDevice(relaysOn(1, 3))
	d.unclear = relaysOn(1)
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, "relays 1 are ON or unclear but not restored") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	for _, c := range powerCalls(h) {
		if !strings.Contains(c, "relay 3 ON") {
			t.Fatalf("unexpected power command %q", c)
		}
	}
}

func TestPowerUnclearRelayAfterBootIsNotRestored(t *testing.T) {
	d := powerDevice(relaysOn(1, 3))
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	newReads(d, func(n int) {
		if n == 2 {
			d.unclear = relaysOn(1) // from the post-boot reading on
		}
	})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, "relays 1 answered neither ON nor OFF after the update") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	for _, c := range powerCalls(h) {
		if !strings.Contains(c, "relay 3 ON") {
			t.Fatalf("unexpected power command %q", c)
		}
	}
}

func TestPowerRelayUnclearWhenTheRestoreStartsIsNotSwitched(t *testing.T) {
	d := powerDevice(relaysOn(1, 3))
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	newReads(d, func(n int) {
		if n == 4 {
			d.unclear = relaysOn(3) // after the settled post-boot readings, before the restore reads
		}
	})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, "relays 3 answer neither ON nor OFF, not switched") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	for _, c := range powerCalls(h) {
		if !strings.Contains(c, "relay 1 ON") {
			t.Fatalf("unexpected power command %q", c)
		}
	}
}

func TestPowerUnreadableBeforeTheUpgradeDropsTheRestore(t *testing.T) {
	d := powerDevice(relaysOn(1))
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	newReads(d, func(n int) {
		if n == 1 {
			d.readPowerErr = errors.New("status 11: timeout") // the reading right before the Upgrade fails
		}
	})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, ErrTextPowerUnreadable) {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 0 {
		t.Fatalf("power commands %v after an unreadable state", got)
	}
}

func TestPowerUnreadableAfterBootDropsTheRestore(t *testing.T) {
	d := powerDevice(relaysOn(1))
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	newReads(d, func(n int) {
		if n == 2 {
			d.readPowerErr = errors.New("status 11: timeout")
		}
	})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, ErrTextPowerUnreadable) {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 0 {
		t.Fatalf("power commands %v although the state after the boot was unreadable", got)
	}
}

// A partly kept state (one relay ON that was ON, another OFF that was ON) is
// ambiguous: the device kept something, so it is not a lost state, and
// nothing is switched.
func TestPowerPartlyKeptStateTouchesNothing(t *testing.T) {
	d := powerDevice(relaysOn(1, 3))
	d.bootPower = relaysOn(1)
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	if j := h.job(t, id); j.State != model.JobSucceeded || j.LastError != "" {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 0 || d.power != relaysOn(1) {
		t.Fatalf("power commands %v, device %b", got, d.power)
	}
}

// The device that answers after the boot must be the one the relays were
// read from.
func TestPowerDeviceNotConfirmedIsNotSwitched(t *testing.T) {
	for name, setup := range map[string]func(*harness, *fakeDevice){
		"another MAC":         func(_ *harness, d *fakeDevice) { d.mac = "02:00:00:00:00:99" },
		"MAC unreadable":      func(_ *harness, d *fakeDevice) { d.macErr = errors.New("status 5: timeout") },
		"no MAC in inventory": func(h *harness, _ *fakeDevice) { dv := h.st.devices[1]; dv.MAC = ""; h.st.devices[1] = dv },
	} {
		t.Run(name, func(t *testing.T) {
			d := powerDevice(relaysOn(1))
			h := newHarness(t, 2, d)
			setup(h, d)
			id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
			h.run(t, h.e)
			j := h.job(t, id)
			if j.State != model.JobSucceeded || !strings.Contains(j.LastError, "power not restored") {
				t.Fatalf("job %s %q", j.State, j.LastError)
			}
			if got := powerCalls(h); len(got) != 0 {
				t.Fatalf("power commands %v on an unconfirmed device", got)
			}
		})
	}
}

// A relay that was ON in ANY reading after the boot was kept by the device.
// If somebody switches it off a moment later (the readings settle on OFF),
// that is their decision and must not be undone. (The fleet's lights show the
// old state for milliseconds only, far less than the time to the first
// reading, so this costs nothing in practice.)
func TestPowerOnInAnyPostBootReadingCountsAsKept(t *testing.T) {
	d := powerDevice(relaysOn(1))
	d.bootPower = relaysOn(1)
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	newReads(d, func(n int) {
		if n == 3 { // right after the first post-boot reading somebody switches it off
			d.power = 0
		}
	})
	h.run(t, h.e)
	if j := h.job(t, id); j.State != model.JobSucceeded || j.LastError != "" {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 0 || d.power != 0 {
		t.Fatalf("power commands %v, device %b: the user's OFF must stay", got, d.power)
	}
}

// A state that never settles is not acted on.
func TestPowerReadingThatKeepsChangingIsNotRestored(t *testing.T) {
	d := powerDevice(relaysOn(1))
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	newReads(d, func(n int) {
		if n >= 3 {
			d.power ^= relaysOn(1) // flips after every post-boot reading
		}
	})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, ErrTextPowerUnreadable) {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 0 {
		t.Fatalf("power commands %v on a state that never settled", got)
	}
}

// Relays keep their state across an update on this fleet, so by default only
// the light channel is restored; a relay that is OFF after the boot was most
// likely switched off by someone.
func TestPowerPlainRelaysAreNotRestoredByDefault(t *testing.T) {
	d := powerDevice(relaysOn(1, 2))
	d.light = relaysOn(2) // relay 1 is a plain relay, relay 2 the light
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobSucceeded || j.LastError != "" || j.PowerOn != relaysOn(2) {
		t.Fatalf("job %s %q PowerOn %b", j.State, j.LastError, j.PowerOn)
	}
	if got := powerCalls(h); len(got) != 1 || !strings.Contains(got[0], "relay 2 ON") {
		t.Fatalf("power calls %v, want the light only", got)
	}
}

func TestPowerRelaysAreRestoredOnRequest(t *testing.T) {
	d := powerDevice(relaysOn(1, 2))
	d.light = relaysOn(2)
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", RestoreRelays: true})
	h.run(t, h.e)
	if j := h.job(t, id); j.State != model.JobSucceeded || j.PowerOn != relaysOn(1, 2) || !j.RestoreRelays {
		t.Fatalf("job %+v", j)
	}
	if d.power != relaysOn(1, 2) {
		t.Fatalf("device power %b", d.power)
	}
}

func TestPreviewSaysWhichRelaysAreKept(t *testing.T) {
	d := powerDevice(relaysOn(1, 2))
	d.light = relaysOn(2)
	h := newHarness(t, 2, d)
	items, err := h.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.0.0"}, {DeviceID: 1, Release: "16.0.0", RestoreRelays: true}})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].PowerRestore != "2" || items[0].PowerRelaysKept != "1" {
		t.Fatalf("default preview %+v", items[0])
	}
	if items[1].PowerRestore != "1, 2" || items[1].PowerRelaysKept != "" {
		t.Fatalf("opt-in preview %+v", items[1])
	}
}

// Direct plans (ESP32, 4 MB ESP8266) read the state again right before the
// Upgrade too; without it a user's OFF during the backup would be undone.
func TestPowerSecondReadingOnDirectPlans(t *testing.T) {
	d := powerDevice(relaysOn(1))
	d.ident.FreeKB = 2000 // plenty of free flash: direct plan
	h := newHarness(t, 2, d)
	h.backups.hook = func() { d.power = 0 }
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.Plan != model.PlanDirect {
		t.Fatalf("plan %s", j.Plan)
	}
	if j.State != model.JobSucceeded || !strings.Contains(j.LastError, "were not ON right before the update") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 0 || d.power != 0 {
		t.Fatalf("power commands %v, device %b", got, d.power)
	}
}

// An MQTT report counts by the time it was received: one that arrived before
// the state was read is old news (the reading already shows it).
func TestPowerReportsAreJudgedByReceiveTime(t *testing.T) {
	d := powerDevice(relaysOn(1))
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	var after bool
	h.backups.hook = func() {
		// a boot-announcement style report stamped long before the job read the state
		h.e.mu.Lock()
		inbox := h.e.workers[id]
		h.e.mu.Unlock()
		inbox <- Event{Kind: EvPowerReport, Power: relaysOn(1), At: t0.Add(-time.Hour)}
		after = true
	}
	h.run(t, h.e)
	if !after {
		t.Fatal("hook did not run")
	}
	if j := h.job(t, id); j.State != model.JobSucceeded || j.LastError != "" {
		t.Fatalf("job %s %q: a report from before the reading must be ignored", j.State, j.LastError)
	}
	if got := powerCalls(h); len(got) != 1 {
		t.Fatalf("power calls %v", got)
	}
}

// Fail closed in the engine too: whatever job it is handed, a restore with
// nothing valid to restore sends no switch command.
func TestPowerRestoreActionSwitchesNothingOnZeroInput(t *testing.T) {
	for name, mod := range map[string]func(*model.Job){
		"no candidates":      func(j *model.Job) { j.PowerOn = 0 },
		"restore turned off": func(j *model.Job) { j.SkipPowerRestore = true },
		"relay beyond 8":     func(j *model.Job) { j.PowerOn = relaysOn(9) },
	} {
		t.Run(name, func(t *testing.T) {
			d := powerDevice(0)
			h := newHarness(t, 2, d)
			j := model.Job{ID: 1, DeviceID: 1, PowerOn: relaysOn(1), CreatedBy: "alice"}
			mod(&j)
			_ = h.e.restorePower(context.Background(), j, model.Device{ID: 1})
			if got := powerCalls(h); len(got) != 0 {
				t.Fatalf("power commands %v", got)
			}
		})
	}
}

// A resumed restore_power job whose reading time was never recorded (an
// older row, a bug) is finished without switching anything.
func TestPowerResumedRestoreWithoutTimestampSwitchesNothing(t *testing.T) {
	h := newHarness(t, 2, powerDevice(0))
	j := job(model.JobRestorePower, model.StageFinal)
	j.ID, j.DeviceID, j.CreatedBy = 1, 1, "alice"
	j.PowerOn = relaysOn(1) // PowerReadAt stays zero
	h.st.put(j)
	h.run(t, h.e)
	if got := h.job(t, 1); got.State != model.JobSucceeded || !strings.Contains(got.LastError, "unknown age") {
		t.Fatalf("job %s %q", got.State, got.LastError)
	}
	if got := powerCalls(h); len(got) != 0 {
		t.Fatalf("power commands %v", got)
	}
}
