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
	h.Observe(1, "Online", 0)
	if _, ok := h.Holding(1); !ok {
		t.Fatal("a lone Online is no restart")
	}
	h.Observe(1, "Offline", 0)
	if _, ok := h.Holding(1); !ok {
		t.Fatal("Offline alone: the device is not back yet")
	}
	h.Observe(1, "Online", 0)
	if _, ok := h.Holding(1); ok {
		t.Fatal("Offline then Online is a restart: the hold must end")
	}
}

// Why: without MQTT the only sign of a restart is the uptime. A reading below the cached one cannot belong
// to the same boot.
func TestHoldEndsWhenTheUptimeWentBackwards(t *testing.T) {
	h, c := newHolds()
	h.Start(1, 4000)
	c.advance(40 * time.Second)
	h.Observe(1, "", 4040)
	if _, ok := h.Holding(1); !ok {
		t.Fatal("a normally growing uptime is the same boot")
	}
	c.advance(40 * time.Second)
	h.Observe(1, "", 12)
	if _, ok := h.Holding(1); ok {
		t.Fatal("an uptime that fell is a restart")
	}
}

// Why: the tracker repeats its cached uptime with every message (a POWER report, an LWT). Taking that
// stale value for a reading would "see" a restart whenever the device had booted a while before the
// command, and release it while it still flashes.
func TestAStaleCachedUptimeIsNotAReading(t *testing.T) {
	h, c := newHolds()
	h.Start(1, 60) // booted a minute before the command
	c.advance(5 * time.Minute)
	h.Observe(1, "Online", 60) // the same cached value, repeated
	h.Observe(1, "Online", 60)
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
	h.Observe(1, "", 100) // 100 s up, 120 s since the command: booted 20 s after it
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
