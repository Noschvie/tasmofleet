package devlock

import (
	"testing"
	"time"
)

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newHolds() (*FirmwareHolds, *testClock) {
	c := &testClock{t: time.Date(2026, 10, 31, 18, 21, 0, 0, time.UTC)}
	return NewFirmwareHolds(c.now, 10*time.Minute), c
}

// Why: a typed Upgrade ends its job when the device answers, but the device flashes for about a minute.
// An update started in that window is a second update on the same device and can brick it, so the hold
// must exist from the moment the command was sent, for that device only.
func TestHoldStartsWithTheCommandAndOnlyForThatDevice(t *testing.T) {
	h, c := newHolds()
	h.Start(1, 4000)
	since, ok := h.Holding(1)
	if !ok || !since.Equal(c.t) {
		t.Fatalf("device 1 must be held since the command, got %v %v", since, ok)
	}
	if _, ok := h.Holding(2); ok {
		t.Fatal("another device must stay free")
	}
}

// Why: the LWT cycle is how a Tasmota with MQTT announces its restart. Offline then Online ends the hold;
// a lone Online (a retained message, a duplicate) proves no restart and must not release a device that
// is still flashing.
func TestHoldEndsAfterAnLWTOfflineOnlineCycleOnly(t *testing.T) {
	h, _ := newHolds()
	h.Start(1, 0)
	h.Observe(1, "Online", 0, false)
	if _, ok := h.Holding(1); !ok {
		t.Fatal("a lone Online is no restart")
	}
	h.Observe(1, "Offline", 0, false)
	if _, ok := h.Holding(1); !ok {
		t.Fatal("Offline alone: the device is not back yet")
	}
	h.Observe(1, "Online", 0, false)
	if _, ok := h.Holding(1); ok {
		t.Fatal("Offline then Online is a restart: the hold must end")
	}
}

// Why: without MQTT the only sign of a restart is the uptime: a reading shorter than the time since the
// command says the device booted after it. A merely lower reading proves nothing (a poll that began
// before the command can arrive after it), so it must not release a device that is still flashing.
func TestHoldEndsOnAnUptimeYoungerThanTheCommandNotOnALowerOne(t *testing.T) {
	h, c := newHolds()
	h.Start(1, 4000)
	c.advance(40 * time.Second)
	h.Observe(1, "", 3000, false) // a stale poll: lower than the cached value, but older than the command
	if _, ok := h.Holding(1); !ok {
		t.Fatal("a lower uptime alone must not release the hold")
	}
	c.advance(40 * time.Second)
	h.Observe(1, "", 12, false) // up for 12 s, 80 s since the command
	if _, ok := h.Holding(1); ok {
		t.Fatal("an uptime younger than the command is a restart")
	}
}

// Why: a two-stage update restarts into the minimal image first, which then flashes the real firmware and
// restarts again. Releasing at the first restart would let a TasmoFleet update start in the middle of that.
func TestHoldSurvivesTheRestartIntoMinimalFirmware(t *testing.T) {
	h, c := newHolds()
	h.Start(1, 4000)
	c.advance(30 * time.Second)
	h.Observe(1, "Offline", 0, false)
	h.Observe(1, "Online", 0, true) // back, but minimal
	h.Observe(1, "", 5, true)       // young uptime, still minimal
	if _, ok := h.Holding(1); !ok {
		t.Fatal("minimal firmware is not the end of the update")
	}
	c.advance(60 * time.Second)
	h.Observe(1, "Offline", 0, true)
	h.Observe(1, "Online", 0, false) // the real image is up
	if _, ok := h.Holding(1); ok {
		t.Fatal("the restart into the real firmware ends the hold")
	}
}

// Why: the tracker repeats its cached uptime with every message (a POWER report, an LWT). Taking that
// stale value for a reading would "see" a restart whenever the device had booted a while before the
// command, and release it while it still flashes.
func TestAStaleCachedUptimeIsNotAReading(t *testing.T) {
	h, c := newHolds()
	h.Start(1, 60) // booted a minute before the command
	c.advance(5 * time.Minute)
	h.Observe(1, "Online", 60, false) // the same cached value, repeated
	h.Observe(1, "Online", 60, false)
	if _, ok := h.Holding(1); !ok {
		t.Fatal("the cached uptime repeated must not release the hold")
	}
}

// Why: a device that restarts and reports before TasmoFleet saw any earlier uptime (baseline unknown or
// lower than the new reading) is still recognised: it has been up for less time than has passed since
// the command, so it booted after it.
func TestHoldEndsWhenTheDeviceIsYoungerThanTheCommand(t *testing.T) {
	h, c := newHolds()
	h.Start(1, 10)
	c.advance(2 * time.Minute)
	h.Observe(1, "", 100, false) // 100 s up, 120 s since the command: booted 20 s after it
	if _, ok := h.Holding(1); ok {
		t.Fatal("a device younger than the command has restarted")
	}
}

// Why: a hold must never last for ever. A lost report, a device without MQTT or one that never comes back
// would otherwise block every later update; the timeout is the way out. Just before it, the hold stays.
func TestHoldEndsAtTheTimeout(t *testing.T) {
	h, c := newHolds()
	h.Start(1, 0)
	c.advance(10*time.Minute - time.Second)
	if _, ok := h.Holding(1); !ok {
		t.Fatal("still inside the timeout")
	}
	c.advance(time.Second)
	if _, ok := h.Holding(1); ok {
		t.Fatal("the timeout must release the device")
	}
}

// Why: a second typed command restarts the clock; the newer flash is the one to wait for.
func TestASecondCommandRestartsTheHold(t *testing.T) {
	h, c := newHolds()
	h.Start(1, 0)
	c.advance(9 * time.Minute)
	h.Start(1, 0)
	c.advance(5 * time.Minute)
	if _, ok := h.Holding(1); !ok {
		t.Fatal("the second command must start a fresh timeout")
	}
}
