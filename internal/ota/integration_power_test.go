//go:build integration

package ota

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

// Power restore against the simulator: the relay behaviour of real devices
// (kept over a plain ESP8266 restart, lost through the minimal image and on
// an ESP32) is what makes the feature necessary, so these scenarios use it.

func (e *itEnv) setRelays(dev model.DeviceID, on ...bool) {
	for i, v := range on {
		e.ctrl[dev].SetRelay(i+1, v)
	}
}

func (e *itEnv) wantRelays(dev model.DeviceID, want ...bool) {
	e.t.Helper()
	if got := e.ctrl[dev].Relays(); !slices.Equal(got, want) {
		e.t.Fatalf("relays %v, want %v", got, want)
	}
}

// A MagicHome-style light on a 1 MB ESP8266 goes through minimal and comes
// back ON for a moment, then switches itself OFF (observed on the fleet,
// 2026-10-19); the engine waits until the reading has settled, switches it
// back on, audits the command and records the state in the job.
func TestITPowerRestoredAfterStagedUpdate(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M, Light: true})
	e.setRelays(dev, true)
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	j := e.expectSuccess(dev, orig, 2, model.PlanStaged)
	e.wantRelays(dev, true)
	if j.PowerOn != relaysOn(1) {
		t.Fatalf("job PowerOn = %b", j.PowerOn)
	}
	audits := e.audits.powerAudits(dev)
	if len(audits) != 1 || audits[0].Actor != "tasmofleet (OTA restore for job 1, requested by it)" || !strings.Contains(audits[0].Detail, `"action":"ON"`) || audits[0].Result != "ok" {
		t.Fatalf("power audit %+v, want one successful ON, attributed to the OTA job and its requester", audits)
	}
}

// An ESP32 without SetOption0 never restores the relay after a restart.
func TestITPowerRestoredAfterESP32Update(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32C3, SetOption0: boolp(false), Light: true})
	e.setRelays(dev, true)
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
	e.wantRelays(dev, true)
}

// Only the relays that were ON come back; the OFF ones stay OFF.
func TestITPowerMultiRelayMixed(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32C3, SetOption0: boolp(false), Relays: 4})
	e.setRelays(dev, true, false, true, false)
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, RestoreRelays: true})
	j := e.expectSuccess(dev, orig, 1, model.PlanDirect)
	e.wantRelays(dev, true, false, true, false)
	if j.PowerOn != relaysOn(1, 3) {
		t.Fatalf("job PowerOn = %b", j.PowerOn)
	}
	if n := len(e.audits.powerAudits(dev)); n != 2 {
		t.Fatalf("%d power commands, want 2 (relays 2 and 4 were OFF)", n)
	}
}

// A device that comes back ON by itself (plain restart of a 4 MB ESP8266
// restores the relay from RTC RAM) is not switched at all.
func TestITPowerDeviceRestoredItself(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	e.setRelays(dev, true)
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
	e.wantRelays(dev, true)
	if a := e.audits.powerAudits(dev); len(a) != 0 {
		t.Fatalf("power commands %+v although the device restored the relay itself", a)
	}
}

func TestITPowerRestoreOptOut(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32C3, SetOption0: boolp(false), Light: true})
	e.setRelays(dev, true)
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, SkipPowerRestore: true})
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
	e.wantRelays(dev, false)
}

// The preview tells which relays would be switched on again.
func TestITPreviewShowsPowerRestore(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M, Relays: 3})
	e.setRelays(dev, true, false, true)
	eng := e.start(2)
	items, err := eng.Preview(context.Background(), []Request{{DeviceID: dev, Release: itRelease, RestoreRelays: true}, {DeviceID: dev, Release: itRelease}})
	if err != nil || len(items) != 2 {
		t.Fatalf("preview %v %v", items, err)
	}
	if items[0].PowerRestore != "1, 3" {
		t.Fatalf("with relays requested: PowerRestore %q", items[0].PowerRestore)
	}
	if items[1].PowerRestore != "" || items[1].PowerRelaysKept != "1, 3" {
		t.Fatalf("by default plain relays are kept, not restored: %+v", items[1])
	}
}

// The process dies right after restore_power was persisted. The new engine
// reads the relays and switches each missing one once: relay 1 was switched
// by hand in the meantime and is not commanded again.
func TestITPowerRestartInRestoreStep(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32C3, SetOption0: boolp(false), Relays: 2})
	e.setRelays(dev, true, true)
	orig := e.ctrl[dev].OtaURL()
	crashed := e.crashAt(model.JobRestorePower, 1)
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, RestoreRelays: true})
	select {
	case <-crashed:
	case <-time.After(60 * time.Second):
		t.Fatal("job never reached restore_power")
	}
	e.stop()
	e.clearHook()
	if j := e.jobOf(dev); j.State != model.JobRestorePower || j.PowerOn != relaysOn(1, 2) {
		t.Fatalf("job after the crash: %s PowerOn %b", j.State, j.PowerOn)
	}
	if a := e.audits.powerAudits(dev); len(a) != 0 {
		t.Fatalf("relays switched before the crash point: %+v", a)
	}
	e.settle(dev)
	e.ctrl[dev].SetRelay(1, true)
	e.start(2)
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
	e.wantRelays(dev, true, true)
	if a := e.audits.powerAudits(dev); len(a) != 1 || !strings.Contains(a[0].Detail, `"relay":2`) {
		t.Fatalf("power audits after the resume: %+v, want only relay 2", a)
	}
}

// Relays that were OFF before are never switched, also when something else
// (a rule, a person) turned one ON in the meantime.
func TestITPowerLeavesForeignRelayAlone(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32C3, SetOption0: boolp(false), Relays: 2})
	e.setRelays(dev, true, false)
	orig := e.ctrl[dev].OtaURL()
	var once bool
	e.log.mu.Lock()
	e.log.hook = func(j model.Job, ev model.JobEvent) {
		if j.State == model.JobRestorePower && !once {
			once = true
			e.ctrl[dev].SetRelay(2, true) // right before the restore reads the relays
		}
	}
	e.log.mu.Unlock()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, RestoreRelays: true})
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
	e.wantRelays(dev, true, true)
	if n := len(e.audits.powerAudits(dev)); n != 1 {
		t.Fatalf("%d power commands, want 1 (relay 1 only)", n)
	}
}

// The fleet's ESP32 lamps run SetOption0 1: the device keeps its state, the
// engine sees the relay ON right after the boot and touches nothing.
func TestITPowerESP32WithSetOption0KeepsState(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32}) // the capture has SetOption0 1
	e.setRelays(dev, true)
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
	e.wantRelays(dev, true)
	if a := e.audits.powerAudits(dev); len(a) != 0 {
		t.Fatalf("power commands %+v although the device kept its state", a)
	}
}

// The user's OFF wins (direct plan, only stage on full firmware): on an ESP32 with SetOption0 1 the OFF is what the
// device saved, so after the boot it looks exactly like a lost state. The
// readings before the Upgrade are what keeps the engine from switching the
// lamp back on.
func TestITPowerSwitchedOffBeforeTheUpgradeStaysOff(t *testing.T) {
	e := newItEnv(t)
	e.keepBackup = true
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, Light: true})
	e.setRelays(dev, true)
	e.backups.hook = func() { e.ctrl[dev].SetRelay(1, false) } // during the backup, after the preflight reading
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	j := e.expectSuccessNote(dev, orig, 1, model.PlanDirect, "relays 1 were not ON right before the update")
	e.wantRelays(dev, false)
	if a := e.audits.powerAudits(dev); len(a) != 0 {
		t.Fatalf("power commands %+v although the user switched the lamp off (job %+v)", a, j)
	}
}

// PulseTime and Interlock relays are never switched, the other relay is.
func TestITPowerRestrictedRelaysAreLeftAlone(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32C3, SetOption0: boolp(false), Relays: 3, PulseTimes: []int{0, 50, 0}, Interlock: "3"})
	e.setRelays(dev, true, true, true)
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, RestoreRelays: true})
	e.expectSuccessNote(dev, orig, 1, model.PlanDirect, "relays 2, 3 are ON or unclear but not restored")
	e.wantRelays(dev, true, false, false)
	if n := len(e.audits.powerAudits(dev)); n != 1 {
		t.Fatalf("%d power commands, want 1 (relay 1)", n)
	}
}

// The restart takes longer than the maximum age: nothing is switched on.
func TestITPowerRestoreRefusedWhenStateIsOld(t *testing.T) {
	e := newItEnv(t)
	e.machine.PowerRestoreMaxAge = 4 * time.Minute // virtual; the staged update itself needs about 2 minutes
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M, Light: true})
	e.setRelays(dev, true)
	orig := e.ctrl[dev].OtaURL()
	crashed := e.crashAt(model.JobRestorePower, 1)
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	select {
	case <-crashed:
	case <-time.After(60 * time.Second):
		t.Fatal("job never reached restore_power")
	}
	e.stop()
	e.clearHook()
	time.Sleep(time.Duration(float64(5*time.Minute) * timeScale)) // virtual: past the maximum age
	e.start(2)
	e.expectSuccessNote(dev, orig, 2, model.PlanStaged, "too old")
	e.wantRelays(dev, false)
}

func boolp(b bool) *bool { return &b }

// The inventory says another MAC than the device that comes back: the
// relays are read from and switched on the right device only.
func TestITPowerNotRestoredWhenTheMACDiffers(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M, Light: true})
	e.setRelays(dev, true)
	d, _ := e.st.GetDevice(context.Background(), dev)
	d.MAC = "02:ff:ff:ff:ff:ff"
	if err := e.st.UpdateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccessNote(dev, orig, 2, model.PlanStaged, "another MAC address")
	e.wantRelays(dev, false)
	if a := e.audits.powerAudits(dev); len(a) != 0 {
		t.Fatalf("power commands %+v on a device that could not be confirmed", a)
	}
}

// Observed on the fleet (2026-10-19, dehumidifier plug, NOUS A1T): a relay on a
// 1 MB ESP8266 keeps its state through the two-step update, so the restore
// finds it ON and sends nothing.
func TestITPowerRelayKeepsStateThroughMinimal(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M})
	e.setRelays(dev, true)
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccess(dev, orig, 2, model.PlanStaged)
	e.wantRelays(dev, true)
	if a := e.audits.powerAudits(dev); len(a) != 0 {
		t.Fatalf("power commands %+v although the relay kept its state", a)
	}
}

// The same on a 4 MB ESP8266 (direct plan): the second reading right before
// the Upgrade is what keeps the engine from undoing the user's OFF.
func TestITPowerSwitchedOffBeforeTheUpgradeStaysOffESP8266(t *testing.T) {
	e := newItEnv(t)
	e.keepBackup = true
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Light: true})
	e.setRelays(dev, true)
	e.backups.hook = func() { e.ctrl[dev].SetRelay(1, false) }
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccessNote(dev, orig, 1, model.PlanDirect, "were not ON right before the update")
	e.wantRelays(dev, false)
	if a := e.audits.powerAudits(dev); len(a) != 0 {
		t.Fatalf("power commands %+v", a)
	}
}

// A plain relay that is OFF after the update is not switched on by default,
// whatever the reason (here an ESP32 without SetOption0 that lost it).
func TestITPlainRelayIsNotRestoredByDefault(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32C3, SetOption0: boolp(false)})
	e.setRelays(dev, true)
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	e.expectSuccess(dev, orig, 1, model.PlanDirect)
	e.wantRelays(dev, false)
	if a := e.audits.powerAudits(dev); len(a) != 0 {
		t.Fatalf("power commands %+v for a plain relay", a)
	}
}
