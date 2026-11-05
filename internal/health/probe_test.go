package health

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

func refuseAll(h *harness) {
	failAll(h, tasmota.ErrHTTPAPIRefused)
}

// Why (#447): a device whose HTTP API is switched off still serves its web page. The poll
// that learns it must keep the fact (the router's memory forgets after 10 minutes), and the
// next answer must clear it; otherwise the UI says "no answer" for a device that is there.
func TestRefusedHTTPAPIIsRemembered(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	refuseAll(h)
	h.step()
	if got := h.svc.Get(1).HTTPRefusedAt; got.IsZero() {
		t.Fatal("a refused poll left no HTTPRefusedAt")
	}
	// A plain failure (no route) is not a refusal: the page does not answer either.
	h.advance(31 * time.Minute) // a refused device is polled like a failing one: every 30 minutes at most
	failAll(h, tasmota.ErrUnreachable)
	h.step()
	if got := h.svc.Get(1).HTTPRefusedAt; !got.IsZero() {
		t.Fatalf("an unreachable device kept the refusal of %v", got)
	}
	refuseAll(h)
	h.advance(31 * time.Minute)
	h.step()
	h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
	h.advance(31 * time.Minute)
	h.step()
	if got := h.svc.Get(1).HTTPRefusedAt; !got.IsZero() {
		t.Fatalf("HTTP answered again but the refusal of %v stayed", got)
	}
}

// Why (#456): a device that is fine on MQTT is not polled over HTTP, so the HTTP fact decays
// to "not checked". A person who opens the device page gets one deliberate probe instead.
func TestProbeHTTPChecksADeviceThatIsFineOnMQTT(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	h.step()
	if n := len(h.cl.commands()); n != 0 {
		t.Fatalf("a device fine on MQTT was polled: %v", h.cl.commands())
	}
	h.svc.ProbeHTTP(1)
	h.step()
	if got := h.cl.commands(); len(got) != 1 || got[0] != "Status 11" {
		t.Fatalf("probe sent %v, want one Status 11", got)
	}
	if h.svc.Get(1).SeenHTTP.IsZero() {
		t.Fatal("a successful probe left SeenHTTP unset")
	}
	h.want(1, model.HealthOnline)
}

// Why (#619 stays open, but must not get worse): a device whose last will is Offline is never
// polled in the background. The person-triggered probe is the only HTTP it gets, and it is
// what finds a device that is reachable although its MQTT is off (#447).
func TestProbeHTTPReachesALWTOfflineDeviceOnlyOnRequest(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	h.advance(time.Hour)
	h.step()
	if n := len(h.cl.commands()); n != 0 {
		t.Fatalf("background poll of an LWT-offline device: %v", h.cl.commands())
	}
	refuseAll(h)
	h.svc.ProbeHTTP(1)
	h.step()
	g := h.svc.Get(1)
	if len(h.cl.commands()) != 1 || g.HTTPRefusedAt.IsZero() {
		t.Fatalf("probe: commands %v, refused at %v", h.cl.commands(), g.HTTPRefusedAt)
	}
	if g.State != model.HealthOffline {
		t.Fatalf("a probe changed the state to %s: the LWT still says offline", g.State)
	}
}

// Why: reloading a page must not hammer a device. One probe per gap, longer after a failure.
func TestProbeHTTPIsRateLimitedPerDevice(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	failAll(h, tasmota.ErrUnreachable)
	for i := 0; i < 5; i++ {
		h.svc.ProbeHTTP(1)
		h.svc.ProbeHTTP(1)
		h.step()
		h.advance(30 * time.Second)
		h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	}
	if n := len(h.cl.commands()); n != 1 {
		t.Fatalf("%d probes within a few minutes, want 1: %v", n, h.cl.commands())
	}
	h.advance(probeFailGap)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	h.svc.ProbeHTTP(1)
	h.step()
	if n := len(h.cl.commands()); n != 2 {
		t.Fatalf("no probe after the gap: %v", h.cl.commands())
	}
}

// Why: a probe that fails says something about HTTP, not about the device. It must not count
// toward "offline" while MQTT is delivering, nor move the poll schedule.
func TestFailedProbeDoesNotMakeAMQTTDeviceOffline(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	failAll(h, tasmota.ErrUnreachable)
	for i := 0; i < 6; i++ {
		h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false) // MQTT keeps delivering
		h.svc.ProbeHTTP(1)
		h.step()
		h.advance(probeFailGap)
	}
	// MQTT goes quiet: the first silent poll must still be allowed to fail three times before
	// the device counts as offline, as without any probe.
	h.svc.mu.Lock()
	fails := h.svc.devs[1].fails
	h.svc.mu.Unlock()
	if fails != 0 {
		t.Fatalf("probes counted %d failures toward offline", fails)
	}
}

// Why: a probe of an unknown or just-polled device is a no-op (the page may be opened for a
// device that was deleted a second ago, and a fresh HTTP fact needs no second request).
func TestProbeHTTPSkipsUnknownDevicesAndFreshFacts(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	h.svc.ProbeHTTP(9)
	h.step() // the regular first poll
	before := len(h.cl.commands())
	h.svc.ProbeHTTP(1)
	h.step()
	if n := len(h.cl.commands()); n != before {
		t.Fatalf("probe right after a successful poll sent %d extra commands", n-before)
	}
}

// Why (#447): a refused HTTP API is an answer. The device's web page is up; only TasmoFleet's commands are
// refused. Counted as failures, the refusals turned a device that is there "Offline" after three polls (the
// header, its banner and the device list) while the Reachability tile said "The web page answers". A refusal
// is contact: it never counts toward offline and it moves "last seen", but never SeenHTTP, which says the API
// answered (routing, the #598 re-read and the #580 hint read it).
func TestRefusedHTTPAPIIsAnAnswerNotAFailure(t *testing.T) {
	t.Run("HTTP-only device that answered before", func(t *testing.T) {
		h := newHarness(t, false, Options{}, httpDev(1))
		h.svc.reload(h.ctx, h.clk.Now())
		h.step()
		answered := h.svc.Get(1).SeenHTTP
		refuseAll(h)
		for i := 0; i < 2*DefaultOfflineAfter; i++ {
			h.advance(31 * time.Minute)
			h.step()
			h.want(1, model.HealthOnline)
		}
		g := h.svc.Get(1)
		if !g.SeenHTTP.Equal(answered) {
			t.Fatalf("a refusal moved SeenHTTP from %v to %v", answered, g.SeenHTTP)
		}
		if !g.LastSeen.Equal(h.clk.Now()) {
			t.Fatalf("last seen %v, want the last refusal at %v", g.LastSeen, h.clk.Now())
		}
	})
	t.Run("HTTP-only device added with its API off", func(t *testing.T) {
		h := newHarness(t, false, Options{}, httpDev(1))
		h.svc.reload(h.ctx, h.clk.Now())
		refuseAll(h)
		for i := 0; i < 2*DefaultOfflineAfter; i++ {
			h.step()
			h.want(1, model.HealthOnline)
			h.advance(31 * time.Minute)
		}
		if g := h.svc.Get(1); !g.SeenHTTP.IsZero() {
			t.Fatalf("a refusal set SeenHTTP %v", g.SeenHTTP)
		}
	})
	t.Run("MQTT device whose telemetry is quiet", func(t *testing.T) {
		h := newHarness(t, true, Options{}, mqttDev(1, "a"))
		h.listen()
		h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
		h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
		h.step()
		refuseAll(h)
		for i := 0; i < 12; i++ {
			h.advance(31 * time.Minute)
			h.step()
			if st := h.state(1); st == model.HealthOffline {
				t.Fatalf("after %d refused polls the device is %s", len(h.cl.commands()), st)
			}
		}
		if n := len(h.cl.commands()); n <= DefaultOfflineAfter {
			t.Fatalf("only %d polls: the device never reached the offline threshold", n)
		}
		if g := h.svc.Get(1); !g.SeenHTTP.IsZero() {
			t.Fatalf("a refusal set SeenHTTP %v", g.SeenHTTP)
		}
	})
}

// Why (decision 2026-11-05): a refused API does not change by itself. A refused poll waits like a failing one
// (up to 30 minutes), not the 5-minute interval of a healthy device; the page's probe gives news on demand.
func TestRefusedPollsBackOffLikeFailingOnes(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	refuseAll(h)
	h.step()
	for range 5 {
		h.advance(6 * time.Minute) // past a normal poll interval, within the back-off
		h.step()
	}
	if n := len(h.cl.commands()); n > 4 { // the poll, its follow-up is inside the client; no re-poll within 30 min
		t.Fatalf("%d commands within 30 minutes of a refusal: %v", n, h.cl.commands())
	}
}

// Why (decision 2026-11-05): the banner says "Offline since ..." from the state change or the last contact. A probe
// that reaches a device its last will reports offline (the page asked for it) must move neither, or every page
// view would make the device look seen again.
func TestProbeContactDoesNotMoveOfflineSince(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	h.advance(time.Minute)
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", false)
	h.want(1, model.HealthOffline)
	before := h.svc.Get(1)
	h.advance(time.Hour)
	for _, answer := range []bool{true, false} { // the device answers HTTP, then refuses its API
		if answer {
			h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
		} else {
			refuseAll(h)
			h.advance(probeFailGap)
		}
		h.svc.ProbeHTTP(1)
		h.step()
		g := h.svc.Get(1)
		if !g.LastSeen.Equal(before.LastSeen) || !g.Since.Equal(before.Since) || g.State != model.HealthOffline {
			t.Fatalf("a probe moved the offline time: last seen %v -> %v, since %v -> %v, state %s",
				before.LastSeen, g.LastSeen, before.Since, g.Since, g.State)
		}
		if answer && g.SeenHTTP.IsZero() {
			t.Fatal("the probe's answer was not recorded as SeenHTTP")
		}
	}
}
