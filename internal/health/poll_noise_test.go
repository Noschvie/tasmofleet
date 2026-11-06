package health

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/tasmota"
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
	h.advance(DefaultPollInterval / 2)
	h.step()
	if got := len(h.cl.commands()); got == 0 || got == n {
		t.Fatalf("%d of %d devices polled half an interval after the start, want them spread over the interval, not in one burst", got, n)
	}
	h.advance(DefaultPollInterval / 2)
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
// device, or one whose HTTP API is off, is never polled more often than a healthy one.
func TestBackoffNeverUndercutsALongPollInterval(t *testing.T) {
	for _, err := range []error{errDown, tasmota.ErrHTTPAPIRefused} {
		h := newHarness(t, false, Options{PollInterval: 2 * time.Hour}, httpDev(1))
		h.svc.reload(h.ctx, h.clk.Now())
		h.cl.set(func(string, string) (json.RawMessage, error) { return nil, err })
		h.step()
		h.cl.reset()
		for range 90 { // 90 minutes: less than the interval less its jitter
			h.advance(time.Minute)
			h.step()
		}
		if n := len(h.cl.commands()); n != 0 {
			t.Fatalf("%v: the device was polled %d times within 90 minutes at a 2 h interval: %v", err, n, h.cl.commands())
		}
	}
}

// Why (#652): a quiet device on our broker whose HTTP API answers is polled like a device without MQTT: once per
// interval. Its answer must schedule the next poll; with "due now" it would be asked on every 5 s tick, because
// an HTTP answer no longer stops the polls of a device whose telemetry is quiet. It stays Online throughout
// instead of going Stale and back at every poll (main did that every 11 minutes).
func TestQuietDeviceThatAnswersIsPolledOncePerIntervalAndStaysOnline(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":5}`, false) // no snapshot read either
	h.advance(DefaultStaleAfter + time.Minute)
	h.step() // the first poll: the telemetry is quiet
	h.cl.reset()
	before := len(h.st.eventTos(1))
	for range int(time.Hour / tickEvery) { // one hour, tick by tick
		h.advance(tickEvery)
		h.step()
		if st := h.state(1); st != model.HealthOnline {
			t.Fatalf("state %s at %v, want online (each answer is contact)", st, h.clk.Now())
		}
	}
	if n := len(h.cl.commands()); n < 10 || n > 13 {
		t.Fatalf("%d polls in one hour, want one per 5 minute interval", n)
	}
	if got := len(h.st.eventTos(1)); got != before {
		t.Fatalf("%d history rows in an hour of answers: %v", got-before, h.st.eventTos(1))
	}
}

// Why (#652): only the first poll after a *replayed* will is a baseline. After a live will (a reboot, a move to
// another broker while we listened) the poll that finds the device again is a real change and is stored.
func TestRecoveryByPollAfterALiveWillIsStored(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":5}`, false)
	h.advance(time.Minute)
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", false) // it left our broker unclean, HTTP still answers
	h.want(1, model.HealthOffline)
	for len(h.svc.work) > 0 { // the will's row, which Run's worker stores
		(<-h.svc.work)()
	}
	h.advance(DefaultPollInterval * 13 / 10)
	h.step()
	h.want(1, model.HealthOnline)
	if got := h.st.eventTos(1); len(got) != 2 || got[0] != model.HealthOffline || got[1] != model.HealthOnline {
		t.Fatalf("history %v, want the will's Offline and the poll's Online", got)
	}
}

// Why (#652): the baseline is the counterpart of the first observation, whose row is not stored either. A will the
// broker replays after our reconnect for a device whose state is known is stored as a change (Online -> Offline), so
// the poll that reaches the device again must be stored too; otherwise its history ends at Offline while it is online.
func TestReplayedWillForAKnownDeviceIsAChangeNotABaseline(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":5}`, false)
	h.want(1, model.HealthOnline)
	h.advance(time.Minute)
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true) // our reconnect: the will that fired meanwhile, replayed
	h.want(1, model.HealthOffline)
	for len(h.svc.work) > 0 { // the will's row, which Run's worker stores
		(<-h.svc.work)()
	}
	h.advance(DefaultPollInterval)
	h.step() // it left our broker unclean, HTTP answers
	h.want(1, model.HealthOnline)
	if got := h.st.eventTos(1); len(got) != 2 || got[0] != model.HealthOffline || got[1] != model.HealthOnline {
		t.Fatalf("history %v, want the replayed will's Offline and the poll's Online", got)
	}
}

// Why (#652): the baseline belongs to the replayed will it follows. A device that comes back over MQTT before its
// first poll has no baseline left to take: a recovery by poll much later is a real change and is stored.
func TestBaselineEndsWhenTheDeviceComesBackOverMQTT(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)              // replayed at the start
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)              // it reconnects before its first poll
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":5}`, false) // with a snapshot: no snapshot read follows
	h.want(1, model.HealthOnline)
	for len(h.svc.work) > 0 {
		(<-h.svc.work)()
	}
	before := len(h.st.eventTos(1))
	h.advance(DefaultStaleAfter + time.Minute) // later its telemetry goes quiet
	h.step()                                   // stale, and the poll this starts reaches it
	h.want(1, model.HealthOnline)
	if got := h.st.eventTos(1)[before:]; len(got) != 2 || got[0] != model.HealthStale || got[1] != model.HealthOnline {
		t.Fatalf("history after the quiet spell %v, want the drop to Stale and the recovery by poll", got)
	}
}

// Why (#652, #447): only a device whose telemetry is quiet on our broker needs its refusals at the normal interval
// to stay online. A device its last will reports offline is online through what the background poll reached, which a
// refusal is too, so its refusals still wait like a failing poll (up to 30 minutes): 50 requests a day, not 288.
func TestRefusalsOfADeviceItsWillReportsOfflineStillBackOff(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	refuseAll(h)
	h.advance(DefaultPollInterval)
	h.step() // the first poll: refused, online without a path
	h.want(1, model.HealthOnline)
	h.cl.reset()
	for range 23 { // 30 minutes less the jitter's 20 %
		h.advance(time.Minute)
		h.step()
		h.want(1, model.HealthOnline)
	}
	if n := len(h.cl.commands()); n != 0 {
		t.Fatalf("%d polls within 23 minutes of a refusal: %v", n, h.cl.commands())
	}
}

// Why (#652, #456): a probe that a page view asks for decides nothing for a device its will reports offline, so it
// does not use up the baseline either: the background poll after it still writes no row.
func TestProbeBeforeTheFirstPollLeavesTheBaseline(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	h.svc.ProbeHTTP(1)
	h.step() // the probe answers; the will still decides
	h.want(1, model.HealthOffline)
	h.advance(DefaultPollInterval)
	h.step()
	h.want(1, model.HealthOnline)
	if got := h.st.eventTos(1); len(got) != 0 {
		t.Fatalf("history rows after a probe and the baseline poll: %v", got)
	}
}
