package health

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
)

// Why (#652): after a TasmoFleet start every device whose retained will says Offline used to be polled on the
// first tick. They share the poll slots and a dead one holds its slot for the connect timeout, so the replayed
// wills are spread over one poll interval instead; each device is still polled once within it.
func TestReplayedOfflineWillsAreNotAllPolledOnTheFirstTick(t *testing.T) {
	const n = 8
	var devs []model.Device
	for i := 1; i <= n; i++ {
		devs = append(devs, mqttDev(model.DeviceID(i), fmt.Sprintf("t%d", i)))
	}
	h := newHarness(t, true, Options{}, devs...)
	calls := 0
	h.svc.d.Rand = func() float64 { calls++; return 0.1 + 0.1*float64(calls%n) } // never 0: nothing is due at once
	h.listen()
	for i := 1; i <= n; i++ {
		h.bus.emit(mqttbus.KindLWT, fmt.Sprintf("t%d", i), "LWT", "Offline", true)
	}
	h.step()
	if got := len(h.cl.commands()); got != 0 {
		t.Fatalf("%d of %d devices polled on the first tick after a start: %v", got, n, h.cl.commands())
	}
	h.advance(DefaultPollInterval)
	h.step()
	if got := len(h.cl.commands()); got != n {
		t.Fatalf("%d polls within one interval, want one per device (%d)", got, n)
	}
}

// Why (#652): an Offline will that arrives live is usually a reboot or an OTA; a poll in that moment hits the
// device while it restarts. The first poll waits one interval.
func TestLiveOfflineWillIsNotPolledWhileTheDeviceReboots(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.advance(time.Minute)
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", false)
	h.step()
	h.advance(time.Minute)
	h.step()
	if n := len(h.cl.commands()); n != 0 {
		t.Fatalf("polled %d times within a minute of a live will: %v", n, h.cl.commands())
	}
	h.advance(DefaultPollInterval * 13 / 10)
	h.step()
	if n := len(h.cl.commands()); n != 1 {
		t.Fatalf("not polled one interval after the will: %v", h.cl.commands())
	}
}

// Why (#652, the 2026-10-23 decision against restart noise): the first poll after a replayed will is the
// device's baseline for this run, so a start does not write an "Online" row per such device. A real change
// afterwards still does.
func TestFirstPollAfterReplayedWillIsABaselineNotAHistoryRow(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	h.advance(DefaultPollInterval)
	h.step()
	h.want(1, model.HealthOnline)
	if got := h.st.eventTos(1); len(got) != 0 {
		t.Fatalf("history rows after the baseline poll: %v", got)
	}
	failAll(h, errDown)
	for range DefaultOfflineAfter {
		h.advance(6 * time.Minute)
		h.step()
	}
	h.want(1, model.HealthOffline)
	if got := h.st.eventTos(1); len(got) != 1 || got[0] != model.HealthOffline {
		t.Fatalf("a real change after the baseline must be stored, got %v", got)
	}
}

// Why (#652, moved from #645): a device on our broker with quiet telemetry and the HTTP API off is Online
// through the refusals alone. They used to back off to 30 minutes while contact is fresh for 11, so it went
// Stale and Online again every half hour and wrote a history row each time.
func TestQuietDeviceWithAPIOffDoesNotFlipStaleEveryBackoff(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	refuseAll(h)
	h.advance(DefaultStaleAfter + time.Minute)
	h.step() // the first poll finds the API off
	before := len(h.st.eventTos(1))
	for range 180 { // three hours
		h.advance(time.Minute)
		h.step()
		if st := h.state(1); st != model.HealthOnline {
			t.Fatalf("state %s at %v, want online (refusals are contact)", st, h.clk.Now())
		}
	}
	if got := len(h.st.eventTos(1)); got != before {
		t.Fatalf("%d history rows in three hours of steady refusals: %v", got-before, h.st.eventTos(1))
	}
}

// Why (#652): POLL_INTERVAL has no upper limit, so the 30-minute back-off cap must not undercut it: a failing
// device is never polled more often than a healthy one.
func TestBackoffNeverUndercutsALongPollInterval(t *testing.T) {
	h := newHarness(t, false, Options{PollInterval: 2 * time.Hour}, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	h.cl.set(func(string, string) (json.RawMessage, error) { return nil, errDown })
	h.step()
	h.cl.reset()
	for range 90 { // 90 minutes: less than the interval less its jitter
		h.advance(time.Minute)
		h.step()
	}
	if n := len(h.cl.commands()); n != 0 {
		t.Fatalf("a failing device was polled %d times within 90 minutes at a 2 h interval: %v", n, h.cl.commands())
	}
}
