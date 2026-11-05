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
	h.advance(10 * time.Minute)
	failAll(h, tasmota.ErrUnreachable)
	h.step()
	if got := h.svc.Get(1).HTTPRefusedAt; !got.IsZero() {
		t.Fatalf("an unreachable device kept the refusal of %v", got)
	}
	refuseAll(h)
	h.advance(10 * time.Minute)
	h.step()
	h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
	h.advance(30 * time.Minute)
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
