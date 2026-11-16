package transport

import (
	"errors"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// infoMQTT is the fake channel with the INFO1 cache of the real one.
type infoMQTT struct {
	*fakeMQTT
	info map[string]mqttcmd.Info
}

func (m infoMQTT) Info(topic string) (mqttcmd.Info, bool) { i, ok := m.info[topic]; return i, ok }

func rootRig(t *testing.T, info1 string) (*rig, infoMQTT) {
	t.Helper()
	g := newRig(t)
	m := infoMQTT{fakeMQTT: g.mq, info: map[string]mqttcmd.Info{}}
	if info1 != "" {
		m.info["plug"] = mqttcmd.Info{Info1: []byte(info1)}
	}
	g.r.SetMQTT(m)
	return g, m
}

// Why (D9/D10): over MQTT the main page does not exist; the image tag in INFO1 is the only proof that
// a device runs minimal firmware, and the language build cannot be read at all, so it must come back
// as "not known" and never as English.
func TestRootPageOverMQTTFromInfo1(t *testing.T) {
	g, _ := rootRig(t, `{"Info1":{"Module":"Sonoff Basic","Version":"15.1.0(release-minimal)","GroupTopic":"cmnd/tasmotas/"}}`)
	ri, err := g.r.RootPage(WithPath(t.Context(), MQTT), g.target())
	if err != nil {
		t.Fatal(err)
	}
	if ri.Version != "15.1.0" || ri.Image != "release-minimal" || !ri.Minimal {
		t.Fatalf("root = %+v", ri)
	}
	if ri.LangKnown || ri.Lang != "" {
		t.Fatalf("an INFO1 must not claim a language: %+v", ri)
	}
	if g.http.n() != 0 {
		t.Fatal("a job pinned to MQTT touched HTTP")
	}
}

// Why: an INFO1 from before a restart is dropped by the channel (LWT Offline); with none left the page
// must fail, so ProbeIdentity fails and Guard never runs on a guess.
func TestRootPageOverMQTTWithoutInfo1FailsAsBadResponse(t *testing.T) {
	g, _ := rootRig(t, "")
	_, err := g.r.RootPage(WithPath(t.Context(), MQTT), g.target())
	if !errors.Is(err, ErrNoInfo) || !errors.Is(err, tasmota.ErrBadResponse) {
		t.Fatalf("err = %v, want ErrNoInfo (an ErrBadResponse)", err)
	}
	if g.http.n() != 0 {
		t.Fatal("fell back to HTTP")
	}
}

func TestRootPageOverMQTTRefusesUnreadableInfo1(t *testing.T) {
	for _, in := range []string{`{"Info1":{"Version":"nonsense"}}`, `{"Info1":{"Version":"15.1.0()"}}`, `not json`, `{}`} {
		g, _ := rootRig(t, in)
		if _, err := g.r.RootPage(WithPath(t.Context(), MQTT), g.target()); !errors.Is(err, tasmota.ErrBadResponse) {
			t.Fatalf("%s: err = %v", in, err)
		}
	}
}

// Why (D5): a job that pinned HTTP never reads INFO1, and a device whose HTTP fails reads the page
// over MQTT without a pin.
func TestRootPagePathSelection(t *testing.T) {
	g, _ := rootRig(t, `{"Info1":{"Version":"15.1.0(release-tasmota)"}}`)
	if ri, err := g.r.RootPage(WithPath(t.Context(), HTTP), g.target()); err != nil || ri.Version != "" {
		t.Fatalf("pinned HTTP must use the (empty) HTTP page, got %+v %v", ri, err)
	}
	g.mem.Fail(7, HTTPFailUnreachable)
	ri, err := g.r.RootPage(t.Context(), g.target())
	if err != nil || ri.Version != "15.1.0" {
		t.Fatalf("unpinned, HTTP failing: %+v %v", ri, err)
	}
}

// Why (#686, MQTT first): minimal and safeboot firmware are named by the INFO1 they publish on the first
// connection after boot, a passive observation on the broker; the main page is an HTML page for people
// that changes with the firmware. A stored device usable over MQTT that sent one since its last boot is
// read from it without any HTTP request, even while HTTP works. Without one the page is still read over
// HTTP (the fallback, path 3), and an address with no stored device (the first add) has no topic.
func TestTheMainPageOutsideAJobIsTheINFO1SinceTheLastBoot(t *testing.T) {
	g, _ := rootRig(t, `{"Info1":{"Module":"Sonoff Basic","Version":"16.0.0(release-minimal)"}}`)
	ri, err := g.r.RootPage(t.Context(), g.target())
	if err != nil || ri.Version != "16.0.0" || ri.Image != "release-minimal" || ri.LangKnown {
		t.Fatalf("unpinned with an INFO1: %+v %v", ri, err)
	}
	if g.http.n() != 0 {
		t.Fatalf("the main page was read over HTTP although an INFO1 since the last boot names the image")
	}

	g, _ = rootRig(t, "")
	if _, err := g.r.RootPage(t.Context(), g.target()); err != nil {
		t.Fatalf("unpinned without an INFO1: %v", err)
	}
	if g.http.n() != 1 {
		t.Fatalf("without an INFO1 the page must be read over HTTP, %d requests", g.http.n())
	}

	g, _ = rootRig(t, `{"Info1":{"Version":"16.0.0(release-minimal)"}}`)
	if _, err := g.r.RootPage(t.Context(), tasmota.TargetFor(model.Device{Address: "192.0.2.7"})); err != nil || g.http.n() != 1 {
		t.Fatalf("an address without a stored device: http %d, err %v", g.http.n(), err)
	}

	// An INFO1 on a topic that is not usable over MQTT (here: the device is on another broker) is no proof
	// that it came from this device: the page is read.
	g, _ = rootRig(t, `{"Info1":{"Version":"16.0.0(release-minimal)"}}`)
	g.devs.d.MQTT = model.MQTTOtherBroker
	if _, err := g.r.RootPage(t.Context(), g.target()); err != nil || g.http.n() != 1 {
		t.Fatalf("a device not usable over MQTT: http %d, err %v", g.http.n(), err)
	}
}

// Why (#686): an update job reads minimal and safeboot firmware from the INFO1 alone, whatever path it
// pinned: an HTTP pin keeps meaning the main page for RootPage (D5), so the job has its own call that never
// reaches the device, never falls back to the page, and refuses an INFO1 it cannot vouch for (a device not
// usable over MQTT) as unreachable, so the job goes on from what it knows itself.
func TestInfo1RootNeverReadsThePage(t *testing.T) {
	g, _ := rootRig(t, `{"Info1":{"Version":"16.0.0(release-safeboot)"}}`)
	ri, err := g.r.Info1Root(WithPath(t.Context(), HTTP), g.target())
	if err != nil || ri.Image != "release-safeboot" || ri.Version != "16.0.0" {
		t.Fatalf("pinned HTTP: %+v %v", ri, err)
	}
	if ri, err := g.r.RootPage(WithPath(t.Context(), HTTP), g.target()); err != nil || ri.Image != "" || g.http.n() != 1 {
		t.Fatalf("RootPage pinned to HTTP must still read the page: %+v %v http %d", ri, err, g.http.n())
	}

	g, _ = rootRig(t, "")
	if _, err := g.r.Info1Root(t.Context(), g.target()); !errors.Is(err, ErrNoInfo) || g.http.n() != 0 {
		t.Fatalf("no INFO1: err %v, http %d, want ErrNoInfo and no HTTP", err, g.http.n())
	}
	if _, err := g.r.Info1Root(t.Context(), tasmota.TargetFor(model.Device{Address: "192.0.2.7"})); !errors.Is(err, ErrNoInfo) || g.http.n() != 0 {
		t.Fatalf("no stored device: err %v, http %d", err, g.http.n())
	}

	g, _ = rootRig(t, `{"Info1":{"Version":"16.0.0(release-minimal)"}}`)
	g.devs.d.MQTT = model.MQTTOtherBroker
	if _, err := g.r.Info1Root(t.Context(), g.target()); !errors.Is(err, tasmota.ErrUnreachable) || g.http.n() != 0 {
		t.Fatalf("not usable over MQTT: err %v, http %d", err, g.http.n())
	}
}

// Why (#852, T19): while another device answers on this device's topic, an INFO1 on that topic may be the
// other device's. Its image names the family (minimal means ESP8266, safeboot an ESP32-line chip), which
// picks the firmware outside a job and fails a job inside one, so it is no proof for this device: outside a
// job the page at the address is read, and a job's INFO1-only read is unreachable, so the job goes on from
// its own family.
func TestAnINFO1OnATakenTopicNamesNothing(t *testing.T) {
	g, _ := rootRig(t, `{"Info1":{"Module":"Sonoff Basic","Version":"16.0.0(release-minimal)"}}`)
	g.f.httpOK = true
	g.r.TopicTaken().Mark(g.devs.d.ID, g.devs.d.Topic)
	ri, err := g.r.RootPage(t.Context(), g.target())
	if err != nil || ri.Image == "release-minimal" || g.http.n() != 1 {
		t.Fatalf("unpinned on a taken topic: %+v %v, http %d; want the page at the address", ri, err, g.http.n())
	}
	if _, err := g.r.Info1Root(t.Context(), g.target()); !errors.Is(err, tasmota.ErrUnreachable) || g.http.n() != 1 {
		t.Fatalf("Info1Root on a taken topic: err %v, http %d; want ErrUnreachable and no HTTP", err, g.http.n())
	}
}
