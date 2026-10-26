package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Identity over MQTT (#87): devices on our broker get their firmware from INFO1 and one
// Status 2 when HTTP cannot identify them.

type idFakeBus struct {
	mu   sync.Mutex
	cmds []string // "<topic> <command> <payload>"
	down bool
}

func (b *idFakeBus) Subscribe([]mqttbus.Kind, func(mqttbus.Message)) func() { return func() {} }
func (b *idFakeBus) Command(_ context.Context, topic, command, payload string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cmds = append(b.cmds, topic+" "+command+" "+payload)
	return nil
}
func (b *idFakeBus) Connected() bool { return !b.down }
func (b *idFakeBus) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.cmds...)
}

// idFakeInventory is the inventory plus the store: UpdateIdentityFromMQTT has the same
// compare-and-set rule as the real one (only while DetectedAt is zero).
type idFakeInventory struct {
	mu   sync.Mutex
	devs map[model.DeviceID]model.Device
}

func (f *idFakeInventory) list(context.Context) ([]model.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.Device
	for _, d := range f.devs {
		out = append(out, d)
	}
	return out, nil
}

func (f *idFakeInventory) get(id model.DeviceID) model.Device {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.devs[id]
}

func (f *idFakeInventory) UpdateIdentityFromMQTT(_ context.Context, id model.DeviceID, _ store.ProbeGuard, topic string, i model.Identity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.devs[id]
	if d.Topic != topic || !d.Identity.DetectedAt.IsZero() {
		return store.ErrConflict
	}
	d.Identity.Family, d.Identity.Hardware, d.Identity.SingleCore = i.Family, i.Hardware, i.SingleCore
	d.Identity.Version, d.Identity.Image, d.Identity.Variant = i.Version, i.Image, i.Variant
	f.devs[id] = d
	return nil
}

type idAudit struct {
	mu sync.Mutex
	ev []model.AuditEvent
}

func (a *idAudit) Record(_ context.Context, e model.AuditEvent) {
	a.mu.Lock()
	a.ev = append(a.ev, e)
	a.mu.Unlock()
}

type idEnv struct {
	m     *mqttIdentity
	bus   *idFakeBus
	inv   *idFakeInventory
	audit *idAudit
	now   time.Time
}

func newIDEnv(devs ...model.Device) *idEnv {
	e := &idEnv{bus: &idFakeBus{}, inv: &idFakeInventory{devs: map[model.DeviceID]model.Device{}}, audit: &idAudit{}, now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	for _, d := range devs {
		e.inv.devs[d.ID] = d
	}
	e.m = newMQTTIdentity(e.bus, e.inv.list, e.inv, e.audit, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.m.now = func() time.Time { return e.now }
	return e
}

func (e *idEnv) advance(d time.Duration) { e.now = e.now.Add(d) }

func (e *idEnv) emit(kind mqttbus.Kind, topic, suffix, payload string, retained bool) {
	e.m.onMessage(mqttbus.Message{Kind: kind, Topic: topic, Suffix: suffix, Payload: []byte(payload), Retained: retained})
}

// online brings topic online and lets the HTTP grace period pass.
func (e *idEnv) online(topics ...string) {
	for _, t := range topics {
		e.emit(mqttbus.KindLWT, t, "LWT", "Online", true)
	}
	e.advance(mqttIDGrace + time.Second)
}

func oursDevice(id model.DeviceID, topic string) model.Device {
	return model.Device{ID: id, Name: topic, Topic: topic, Address: "192.0.2." + string(rune('0'+id)), MQTT: model.MQTTOurs}
}

const (
	info1Payload   = `{"Info1":{"Module":"Sonoff Basic","Version":"16.0.0(release-tasmota)","FallbackTopic":"cmnd/plug_fb/","GroupTopic":"cmnd/tasmotas/"}}`
	status2Payload = `{"StatusFWR":{"Version":"16.0.0(release-tasmota)","Hardware":"ESP8266EX"}}`
)

func (e *idEnv) drain(ctx context.Context) {
	for {
		select {
		case msg := <-e.m.msgs:
			e.m.handle(ctx, msg)
		default:
			return
		}
	}
}

// Why: the version a device announces at boot is the fix for "Unknown" versions of devices HTTP
// cannot reach; without hardware it must stay unusable for updates.
func TestMQTTIdentityTakesTheVersionFromInfo1(t *testing.T) {
	e := newIDEnv(oursDevice(1, "plug"))
	e.emit(mqttbus.KindInfo, "plug", "INFO1", info1Payload, false)
	e.drain(t.Context())
	got := e.inv.get(1).Identity
	if got.Version != "16.0.0" || got.Variant != "tasmota" || got.Image != "release-tasmota" {
		t.Fatalf("identity = %+v", got)
	}
	if got.Known() || !got.DetectedAt.IsZero() {
		t.Errorf("INFO1 alone made the identity usable for updates or stopped the HTTP probe: %+v", got)
	}
	if len(e.bus.sent()) != 0 {
		t.Errorf("INFO1 caused commands: %q", e.bus.sent())
	}
}

// Why: the HTTP probe is the primary path; whatever MQTT announces must not replace an identity
// it detected, even a different one (a stale retained INFO1).
func TestMQTTIdentityNeverOverwritesAnHTTPIdentity(t *testing.T) {
	d := oursDevice(1, "plug")
	d.Identity = model.Identity{Family: model.ChipESP32, Hardware: "ESP32-D0WD-V3", Version: "17.0.0", Image: "release-tasmota32", Variant: "tasmota", DetectedAt: time.Unix(1700000000, 0)}
	e := newIDEnv(d)
	e.emit(mqttbus.KindInfo, "plug", "INFO1", info1Payload, true)
	e.drain(t.Context())
	if got := e.inv.get(1).Identity; got.Version != "17.0.0" || got.Family != model.ChipESP32 {
		t.Fatalf("HTTP identity overwritten: %+v", got)
	}
	// a probe that finishes between read and write loses against the store's check too
	e2 := newIDEnv(oursDevice(1, "plug"))
	e2.emit(mqttbus.KindInfo, "plug", "INFO1", info1Payload, false)
	e2.inv.mu.Lock()
	dd := e2.inv.devs[1]
	dd.Identity.DetectedAt = time.Unix(1700000000, 0)
	dd.Identity.Version = "17.0.0"
	e2.inv.devs[1] = dd
	e2.inv.mu.Unlock()
	e2.drain(t.Context())
	if got := e2.inv.get(1).Identity.Version; got != "17.0.0" {
		t.Fatalf("version = %q", got)
	}
}

// Why: Status 2 is asked once (not every second) when HTTP did not identify an online device on
// our broker, and the answer is a full identity that passes the same Known() rule as HTTP.
func TestMQTTIdentityAsksStatus2OnceAndStoresTheAnswer(t *testing.T) {
	e := newIDEnv(oursDevice(1, "plug"))
	e.online("plug")
	e.m.askDue(t.Context())
	e.advance(5 * time.Second)
	e.m.askDue(t.Context()) // still waiting for the answer: no second command
	if got := e.bus.sent(); len(got) != 1 || got[0] != "plug STATUS 2" {
		t.Fatalf("commands = %q, want exactly one \"plug STATUS 2\"", got)
	}
	if len(e.audit.ev) != 1 || e.audit.ev[0].Action != "identity.mqtt_ask" || e.audit.ev[0].Target != "device:1" {
		t.Errorf("audit = %+v", e.audit.ev)
	}
	e.emit(mqttbus.KindStatus, "plug", "STATUS2", status2Payload, false)
	e.drain(t.Context())
	got := e.inv.get(1).Identity
	if !got.Known() || got.Hardware != "ESP8266EX" || got.Version != "16.0.0" {
		t.Fatalf("identity = %+v", got)
	}
	e.advance(time.Hour)
	e.m.askDue(t.Context())
	if len(e.bus.sent()) != 1 {
		t.Errorf("asked again after the answer: %q", e.bus.sent())
	}
}

// Why: a device that never answers (or whose answer is unusable) is asked again only after the
// same 10 minutes discovery uses for its Status 5, not on every tick.
func TestMQTTIdentityAsksAnUnansweredDeviceOncePerTenMinutes(t *testing.T) {
	e := newIDEnv(oursDevice(1, "plug"))
	e.online("plug")
	e.m.askDue(t.Context())
	for range 9 {
		e.advance(time.Minute)
		e.m.askDue(t.Context())
	}
	if n := len(e.bus.sent()); n != 1 {
		t.Fatalf("%d commands within 10 minutes, want 1", n)
	}
	e.advance(2 * time.Minute)
	e.m.askDue(t.Context())
	if n := len(e.bus.sent()); n != 2 {
		t.Fatalf("%d commands after 10 minutes, want 2", n)
	}
}

// Why: a working HTTP probe is the primary source: a device it identified is never asked, and
// neither is one that is still inside the grace period HTTP gets after the device came online.
func TestMQTTIdentityDoesNotAskWhenHTTPWorksOrIsStillTrying(t *testing.T) {
	d := oursDevice(1, "plug")
	d.Identity = model.Identity{Family: model.ChipESP8266, Version: "16.0.0", Variant: "tasmota", DetectedAt: time.Unix(1700000000, 0)}
	fresh := oursDevice(2, "fresh")
	e := newIDEnv(d, fresh)
	e.emit(mqttbus.KindLWT, "plug", "LWT", "Online", true)
	e.emit(mqttbus.KindLWT, "fresh", "LWT", "Online", true)
	e.advance(mqttIDGrace - time.Second)
	e.m.askDue(t.Context())
	e.advance(time.Hour)
	e.emit(mqttbus.KindLWT, "plug", "LWT", "Online", true)
	e.m.askDue(t.Context())
	got := e.bus.sent()
	if len(got) != 1 || got[0] != "fresh STATUS 2" {
		t.Fatalf("commands = %q: only the device HTTP did not identify, and only after the grace, may be asked", got)
	}
}

// Why: an offline device cannot answer; asking it only queues a command that fires at its next
// boot (heap burst, spec 6.4 for Status 5). Devices that are not ours, minimal firmware and a
// broker that is away are not asked either.
func TestMQTTIdentityAsksOnlyWhoCanAnswer(t *testing.T) {
	other := oursDevice(2, "other")
	other.MQTT = model.MQTTOtherBroker
	minimal := oursDevice(3, "mini")
	minimal.Identity = model.Identity{Variant: "minimal", Version: "16.0.0"}
	e := newIDEnv(oursDevice(1, "off"), other, minimal)
	e.emit(mqttbus.KindLWT, "off", "LWT", "Online", true)
	e.emit(mqttbus.KindLWT, "off", "LWT", "Offline", true)
	e.online("other", "mini")
	e.m.askDue(t.Context())
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("asked %q", got)
	}
	e.bus.down = true
	e.emit(mqttbus.KindLWT, "off", "LWT", "Online", true)
	e.advance(time.Minute)
	e.m.askDue(t.Context())
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("asked while the broker is away: %q", got)
	}
}

// Why: one question per second on the whole bus (heap bursts, broker load), so a fleet that
// comes back after a broker outage is asked one by one, oldest waiter first.
func TestMQTTIdentityKeepsOneSecondBetweenQuestions(t *testing.T) {
	e := newIDEnv(oursDevice(1, "a"), oursDevice(2, "b"))
	e.online("a", "b")
	e.m.askDue(t.Context())
	e.m.askDue(t.Context())
	if n := len(e.bus.sent()); n != 1 {
		t.Fatalf("%d commands in the same second, want 1", n)
	}
	e.advance(1100 * time.Millisecond)
	e.m.askDue(t.Context())
	got := e.bus.sent()
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("commands = %q, want one question to each device", got)
	}
	for _, c := range got {
		if !strings.HasSuffix(c, " STATUS 2") {
			t.Errorf("automated code sent %q (never Status 0)", c)
		}
	}
}

// Why: anyone on the broker can publish stat/<topic>/STATUS2. Only a timely reply to our own
// question may become an identity.
func TestMQTTIdentityIgnoresUnsolicitedAndLateStatus2(t *testing.T) {
	e := newIDEnv(oursDevice(1, "plug"))
	e.emit(mqttbus.KindStatus, "plug", "STATUS2", status2Payload, false)
	e.drain(t.Context())
	if e.inv.get(1).Identity.Hardware != "" {
		t.Fatal("an unsolicited STATUS2 became the identity")
	}
	e.online("plug")
	e.m.askDue(t.Context())
	e.advance(mqttIDWindow + time.Second)
	e.emit(mqttbus.KindStatus, "plug", "STATUS2", status2Payload, false)
	e.drain(t.Context())
	if e.inv.get(1).Identity.Hardware != "" {
		t.Fatal("a late STATUS2 became the identity")
	}
}

// Why (end to end): a real MQTT connection and a simulated device. HTTP is not used at all; the
// device ends up with a Known() identity from INFO1 plus exactly one Status 2, and the simulator
// proves nothing but Status 2 was sent (safety rule 2).
func TestMQTTIdentityWithSimulatedDevice(t *testing.T) {
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() }) // test teardown
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", MQTTURL: br.URL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() }) // test teardown
	dev, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "esp32-plug", Profile: simulator.ProfileESP32, Version: "15.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	e := newIDEnv(oursDevice(1, dev.Topic()))
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: br.URL(), ClientID: "tasmofleet-test"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.bus = bus
	e.m.now = time.Now
	e.m.grace = 200 * time.Millisecond
	go e.m.run(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for !e.inv.get(1).Identity.Known() {
		if time.Now().After(deadline) {
			t.Fatalf("identity = %+v, commands %q", e.inv.get(1).Identity, dev.Commands())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := e.inv.get(1).Identity; got.Version != "15.1.0" || got.Family != model.ChipESP32 {
		t.Errorf("identity = %+v", got)
	}
	time.Sleep(1500 * time.Millisecond) // a second question would show by now
	var status2 int
	for _, c := range dev.Commands() {
		switch strings.ToUpper(strings.TrimSpace(c)) {
		case "STATUS 2":
			status2++
		case "STATUS 0", "STATUS":
			t.Errorf("automated code sent %q", c)
		}
	}
	if status2 != 1 {
		t.Errorf("Status 2 sent %d times, want 1: %q", status2, dev.Commands())
	}
}
