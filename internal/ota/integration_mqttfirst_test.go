//go:build integration

package ota

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// Updates over MQTT first while HTTP works (#608): the devices answer over both transports, and the update's commands
// still go through the broker. A recorder on the router's HTTP client proves that HTTP carried none of them.

// httpRec records every command the router sends over HTTP.
type httpRec struct {
	tasmota.Client
	mu   sync.Mutex
	cmds []string
}

func (h *httpRec) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	h.mu.Lock()
	h.cmds = append(h.cmds, c)
	h.mu.Unlock()
	return h.Client.Command(ctx, t, c)
}

// firmwareCommands lists what of an update's own commands went over HTTP: OtaUrl, Upgrade (bare or with an argument).
func (h *httpRec) firmwareCommands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.cmds {
		if l := strings.ToLower(strings.TrimSpace(c)); strings.HasPrefix(l, "otaurl") || strings.HasPrefix(l, "upgrade") {
			out = append(out, c)
		}
	}
	return out
}

// addMQTTWithHTTP adds a device on our broker whose HTTP answers too: nothing marks it failing.
func (m *itMQTT) addMQTTWithHTTP(spec simulator.DeviceSpec) model.DeviceID {
	m.t.Helper()
	id := m.addDevice(spec)
	d, err := m.st.GetDevice(context.Background(), id)
	if err != nil {
		m.t.Fatal(err)
	}
	d.MQTT = model.MQTTOurs
	if err := m.st.UpdateDevice(context.Background(), d); err != nil {
		m.t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := m.ch.Do(context.Background(), d, "Status 5"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			m.t.Fatal("device never answered over MQTT")
		}
	}
	if _, err := m.httpRec.Client.Command(context.Background(), tasmota.TargetFor(d), "Status 11"); err != nil {
		m.t.Fatalf("the device must answer over HTTP for this test: %v", err)
	}
	return id
}

// fwGets records the paths the firmware server served.
func (m *itMQTT) fwGets() func() []string {
	var mu sync.Mutex
	var got []string
	inner := m.fwHandler.Load().(http.Handler)
	m.fwHandler.Store(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Path)
		mu.Unlock()
		inner.ServeHTTP(w, r)
	})))
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// startGuarded starts an engine whose Guard calls are recorded against the device's command history.
func (m *itMQTT) startGuarded(dev model.DeviceID) *guardSpy {
	m.t.Helper()
	m.stop()
	eng := m.newEngine(2)
	spy := &guardSpy{selector: eng.sel, ctrl: m.ctrl[dev]}
	eng.sel = spy
	m.runEngine(eng)
	return spy
}

// assertGuardRightBefore checks safety rule 1 on the device's own history: every OtaUrl to TasmoFleet's firmware
// server and every Upgrade 1 it received had a Guard call right before it, with no command in between.
func (m *itMQTT) assertGuardRightBefore(dev model.DeviceID, spy *guardSpy) {
	m.t.Helper()
	spy.mu.Lock()
	marks := map[int]bool{}
	for _, k := range spy.marks {
		marks[k] = true
	}
	spy.mu.Unlock()
	n := 0
	for i, c := range m.ctrl[dev].Commands() {
		l := strings.ToLower(strings.TrimSpace(c))
		if strings.HasPrefix(l, "otaurl "+strings.ToLower(m.fwSrv.URL)) || l == "upgrade 1" {
			n++
			if !marks[i] {
				m.t.Errorf("command %d %q had no Guard right before it (Guard at %v)", i, c, spy.marks)
			}
		}
	}
	if n == 0 {
		m.t.Fatal("no OtaUrl or Upgrade 1 reached the device")
	}
}

// Why (#608, safety rules 1 and 3): the staged 1 MB ESP8266 update, one of the two live test shapes, runs its
// commands over MQTT while the device's HTTP answers: the job is pinned to MQTT, HTTP carries no OtaUrl and no
// Upgrade, Guard runs right before each of them, and the device fetches both images (minimal, then the final one)
// from TasmoFleet's firmware server.
func TestITMQTTFirstStagedESP8266WhileHTTPWorks(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTTWithHTTP(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M, Light: true})
	orig := m.ctrl[dev].OtaURL()
	gets := m.fwGets()
	spy := m.startGuarded(dev)
	m.submit(m.engine.Load(), Request{DeviceID: dev, Language: "en"})
	m.expectSuccess(dev, orig, 2, model.PlanStaged)
	m.wantTransport(dev)
	if got := m.httpRec.firmwareCommands(); len(got) != 0 {
		t.Fatalf("HTTP carried %v: the update's commands must go over MQTT while HTTP works", got)
	}
	m.assertGuardRightBefore(dev, spy)
	served := strings.Join(gets(), " ")
	for _, f := range []string{"/fw/" + itRelease + "/tasmota-minimal.bin.gz", "/fw/" + itRelease + "/tasmota.bin.gz"} {
		if !strings.Contains(served, f) {
			t.Errorf("the firmware server never served %s (served: %s)", f, served)
		}
	}
}

// Why (#608): the ESP32, the other live test shape, with the download (pull): one Upgrade over MQTT, the image of its
// family from TasmoFleet's firmware server, nothing of the update over HTTP. A push stays an HTTP upload by design.
func TestITMQTTFirstESP32WhileHTTPWorks(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTTWithHTTP(simulator.DeviceSpec{Profile: simulator.ProfileESP32})
	orig := m.ctrl[dev].OtaURL()
	gets := m.fwGets()
	spy := m.startGuarded(dev)
	m.submit(m.engine.Load(), Request{DeviceID: dev, Language: "en"})
	m.expectSuccess(dev, orig, 1, model.PlanDirect)
	m.wantTransport(dev)
	if got := m.httpRec.firmwareCommands(); len(got) != 0 {
		t.Fatalf("HTTP carried %v: the update's commands must go over MQTT while HTTP works", got)
	}
	m.assertGuardRightBefore(dev, spy)
	if served := strings.Join(gets(), " "); !strings.Contains(served, "/fw/"+itRelease+"/tasmota32.bin") {
		t.Errorf("the firmware server never served tasmota32.bin (served: %s)", served)
	}
}

// Why (safety rule 3, D5): the Upgrade 1 ran over MQTT but its answer was lost while HTTP works. The engine probes
// and never sends a second Upgrade 1, over MQTT or over HTTP: a pinned job does not move to the other transport.
func TestITMQTTFirstLostUpgradeIsProbedNeverResentOverHTTP(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTTWithHTTP(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	m.lossy.loseCmd = "Upgrade 1"
	orig := m.ctrl[dev].OtaURL()
	eng := m.start(2)
	m.submit(eng, Request{DeviceID: dev, Language: "en"})
	m.expectSuccess(dev, orig, 1, model.PlanDirect)
	m.wantTransport(dev)
	if m.lossy.lost != 1 {
		t.Fatal("the test never lost a reply")
	}
	if got := m.httpRec.firmwareCommands(); len(got) != 0 {
		t.Fatalf("HTTP carried %v after the lost answer", got)
	}
}

// Why (#608, T6): a job pins MQTT when it is made, which for a scheduled one can be hours before it runs. Each pinned
// write is checked again under the strict rule right before it is published: once another device's group topic may
// cover this device's topic, OtaUrl and Upgrade are not published at all and the job fails, instead of flashing a
// second device without its own Guard. Nothing goes over HTTP either.
func TestITAnMQTTUpdateWhoseTopicMayBeSharedAfterThePinSendsNothing(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTTWithHTTP(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	eng := m.newEngine(2)
	m.submit(eng, Request{DeviceID: dev, Language: "en"})
	if j := m.wantTransport(dev); j.State.Terminal() {
		t.Fatalf("job %s before it ran", j.State)
	}
	m.facts.mu.Lock()
	m.facts.othersUnread = []string{"porch"}
	m.facts.mu.Unlock()
	m.runEngine(eng)
	j := m.waitTerminal(dev)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "porch") {
		t.Fatalf("job %s %q: want a failure that names the device holding the update back", j.State, j.LastError)
	}
	for _, c := range m.ctrl[dev].Commands() {
		if l := strings.ToLower(c); strings.HasPrefix(l, "otaurl http") || strings.HasPrefix(l, "upgrade 1") {
			t.Fatalf("the device received %q on a topic that may be shared", c)
		}
	}
	if got := m.httpRec.firmwareCommands(); len(got) != 0 {
		t.Fatalf("HTTP carried %v: a pinned job never moves to HTTP", got)
	}
}

// identitySpy counts the identities the engine stores.
type identitySpy struct {
	store.DeviceStore
	mu     sync.Mutex
	stored int
}

func (s *identitySpy) UpdateIdentity(ctx context.Context, id model.DeviceID, i model.Identity) error {
	s.mu.Lock()
	s.stored++
	s.mu.Unlock()
	return s.DeviceStore.UpdateIdentity(ctx, id, i)
}

// Why (#608 review, #852): a job keeps the path it pinned, and a scheduled one pins MQTT hours before it starts. If
// another device is found on the topic meanwhile, the preflight's identity reads (Status 2, Status 4, Status 11, the
// OtaUrl read) would be answered by that device, and the engine would store its hardware, version or image on this
// device's row before the OtaUrl write is refused. The job must read nothing on the taken topic, store no identity and
// fail naming the taken topic.
func TestITAnMQTTUpdateOnATopicTakenAfterThePinReadsNothingThere(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTTWithHTTP(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	spy := &identitySpy{DeviceStore: m.st}
	eng := NewEngine(Options{Machine: m.machine, MaxParallel: 2, Paths: m.paths}, m.st, spy, m.api, m.cache, m.backups, m.clk, m.log)
	m.engine.Store(eng)
	eng.safebootPush = m.safebootPush
	m.submit(eng, Request{DeviceID: dev, Language: "en"})
	if j := m.wantTransport(dev); j.State.Terminal() {
		t.Fatalf("job %s before it ran", j.State)
	}
	m.facts.mu.Lock()
	m.facts.topicTaken = time.Now()
	m.facts.mu.Unlock()
	before := len(m.ctrl[dev].Commands())
	m.runEngine(eng)
	j := m.waitTerminal(dev)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "another device answers") {
		t.Fatalf("job %s %q: want a failure that names the taken topic", j.State, j.LastError)
	}
	spy.mu.Lock()
	stored := spy.stored
	spy.mu.Unlock()
	if stored != 0 {
		t.Errorf("the job stored %d identities read on a topic another device answers on", stored)
	}
	if got := m.ctrl[dev].Commands()[before:]; len(got) != 0 {
		t.Errorf("the device received %v on its taken topic", got)
	}
	if got := m.httpRec.firmwareCommands(); len(got) != 0 {
		t.Fatalf("HTTP carried %v: a pinned job never moves to HTTP", got)
	}
}
