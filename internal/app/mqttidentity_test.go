package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Identity over MQTT (#87): devices on our broker get their firmware from INFO1 and one
// Status 2 when HTTP cannot identify them.

// idFakeBus is the bus and the command channel in one: Do records "<topic> <command>" and answers
// from reply (default: no answer, the timeout a silent device gives).
type idFakeBus struct {
	mu    sync.Mutex
	cmds  []string
	down  bool
	reply func(cmd string) (json.RawMessage, error)
}

func (b *idFakeBus) Subscribe([]mqttbus.Kind, func(mqttbus.Message)) func() { return func() {} }
func (b *idFakeBus) Command(context.Context, string, string, string) error {
	panic("the identity reader must send through the command channel")
}
func (b *idFakeBus) Do(_ context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	b.mu.Lock()
	b.cmds = append(b.cmds, d.Topic+" "+cmd)
	reply := b.reply
	b.mu.Unlock()
	if reply == nil {
		return nil, tasmota.ErrTimeout
	}
	return reply(cmd)
}
func (b *idFakeBus) Connected() bool { return !b.down }

// answers makes the device answer Status 2 with status2Payload.
func (b *idFakeBus) answers() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reply = func(cmd string) (json.RawMessage, error) {
		if cmd == "Status 2" {
			return json.RawMessage(status2Payload), nil
		}
		return nil, tasmota.ErrTimeout
	}
}
func (b *idFakeBus) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.cmds...)
}

// idFakeInventory is the inventory plus the store: UpdateIdentityFromMQTT has the same
// compare-and-set rule as the real one (only while DetectedAt is zero).
type idFakeInventory struct {
	mu     sync.Mutex
	devs   map[model.DeviceID]model.Device
	naive  bool  // skip the compare-and-set: pins the checks the reader itself makes
	fail   error // returned instead of storing
	writes int
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
	if f.fail != nil {
		return f.fail
	}
	d := f.devs[id]
	if !f.naive && (d.Topic != topic || !d.Identity.DetectedAt.IsZero()) {
		return store.ErrConflict
	}
	f.writes++
	d.Identity.Family, d.Identity.Hardware, d.Identity.SingleCore = i.Family, i.Hardware, i.SingleCore
	d.Identity.Version, d.Identity.Image, d.Identity.Variant = i.Version, i.Image, i.Variant
	// The real store writes the language columns from the identity it is given (devices.go).
	d.Identity.Language, d.Identity.LanguageKnown = i.Language, i.LanguageKnown
	f.devs[id] = d
	return nil
}

func (f *idFakeInventory) UpdateGroupTopicFromMQTT(_ context.Context, id model.DeviceID, _ store.ProbeGuard, topic, groupTopic string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.devs[id]
	if d.Topic != topic {
		return store.ErrConflict
	}
	d.GroupTopic, d.GroupTopicReadAt = groupTopic, time.Now()
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
	e.m = newMQTTIdentity(e.bus, e.bus, e.inv.list, e.inv, e.audit, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.m.now = func() time.Time { return e.now }
	e.m.spawn = func(f func()) { f() } // the question runs inline: the test sees its effect when askDue returns
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

func (e *idEnv) writes() int {
	e.inv.mu.Lock()
	defer e.inv.mu.Unlock()
	return e.inv.writes
}

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
	if got.Version != "16.0.0" || got.Variant != "" || got.Image != "" {
		t.Fatalf("identity = %+v, want the version only (an INFO1 must not choose the variant)", got)
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
	e.bus.answers()
	e.online("plug")
	e.m.askDue(t.Context())
	e.advance(5 * time.Second)
	e.m.askDue(t.Context()) // answered: no second command
	if got := e.bus.sent(); len(got) != 1 || got[0] != "plug Status 2" {
		t.Fatalf("commands = %q, want exactly one \"plug Status 2\"", got)
	}
	if len(e.audit.ev) != 1 || e.audit.ev[0].Action != "identity.mqtt_ask" || e.audit.ev[0].Target != "device:1" {
		t.Errorf("audit = %+v", e.audit.ev)
	}
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

// Why: Status 2 carries no language build (only the web page does, D10). The answer must keep what
// was stored, including the "read from a page" flag, or a German device would turn "unknown" or,
// worse, lose the language it was read with.
func TestMQTTIdentityKeepsTheStoredLanguageAndItsFlag(t *testing.T) {
	for _, known := range []bool{true, false} {
		d := oursDevice(1, "plug")
		d.Identity.Language, d.Identity.LanguageKnown = "DE", known
		e := newIDEnv(d)
		e.online("plug")
		e.m.askDue(t.Context())
		e.emit(mqttbus.KindStatus, "plug", "STATUS2", status2Payload, false)
		e.drain(t.Context())
		got := e.inv.get(1).Identity
		if !got.Known() || got.Language != "DE" || got.LanguageKnown != known {
			t.Errorf("known=%v: identity language %q known %v", known, got.Language, got.LanguageKnown)
		}
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
	if len(got) != 1 || got[0] != "fresh Status 2" {
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
		if !strings.HasSuffix(c, " Status 2") {
			t.Errorf("automated code sent %q (never Status 0)", c)
		}
	}
}

// Why: anyone on the broker can publish stat/<topic>/STATUS2. Only the reply the command channel
// matched to our own question may become an identity: a STATUS2 on the bus is ignored, before and
// after a question, and an answer that does not parse leaves the identity alone.
func TestMQTTIdentityIgnoresUnsolicitedStatus2(t *testing.T) {
	e := newIDEnv(oursDevice(1, "plug"))
	e.emit(mqttbus.KindStatus, "plug", "STATUS2", status2Payload, false)
	e.drain(t.Context())
	if e.inv.get(1).Identity.Hardware != "" {
		t.Fatal("an unsolicited STATUS2 became the identity")
	}
	e.online("plug")
	e.bus.reply = func(string) (json.RawMessage, error) { return json.RawMessage(`{"nope":1}`), nil }
	e.m.askDue(t.Context())
	e.emit(mqttbus.KindStatus, "plug", "STATUS2", status2Payload, false)
	e.drain(t.Context())
	if e.inv.get(1).Identity.Hardware != "" {
		t.Fatal("a STATUS2 from the bus, or an unusable answer, became the identity")
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
	e.m.cmd = mqttcmd.New(bus, mqttcmd.Options{})
	e.m.now = time.Now
	e.m.spawn = func(f func()) { go f() }
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

// Why (#87 review M1): after an OTA preflight the stored identity is HTTP-detected. A forged
// retained INFO1 "(minimal)" must not touch it: it would make the device look like minimal
// firmware, and the update form then picks the tasmota variant and allows the variant change.
// The store is naive here so the check is pinned in the reader itself, not in the store.
func TestMQTTIdentityLeavesAPreflightIdentityAlone(t *testing.T) {
	d := oursDevice(1, "plug")
	d.Identity = model.Identity{Family: model.ChipESP8266, Hardware: "ESP8285N08", Version: "16.0.0", Image: "release-zbbridge", Variant: "zbbridge", DetectedAt: time.Unix(1700000000, 0)}
	e := newIDEnv(d)
	e.inv.naive = true
	e.emit(mqttbus.KindInfo, "plug", "INFO1", `{"Info1":{"Version":"16.0.0(release-minimal)"}}`, true)
	e.drain(t.Context())
	if got := e.inv.get(1).Identity; got != d.Identity || e.writes() != 0 {
		t.Fatalf("identity = %+v, writes %d", got, e.writes())
	}
}

// Why: an INFO1 never decides the variant, and nothing from MQTT may make a device look like
// minimal or safeboot firmware (update.go then switches the variant to tasmota on its own).
func TestMQTTIdentityNeverSetsAMinimalVariant(t *testing.T) {
	e := newIDEnv(oursDevice(1, "plug"))
	e.emit(mqttbus.KindInfo, "plug", "INFO1", `{"Info1":{"Version":"16.0.0(release-minimal)"}}`, true)
	e.drain(t.Context())
	if got := e.inv.get(1).Identity; got.Variant != "" || got.Version != "16.0.0" {
		t.Fatalf("identity = %+v", got)
	}
	e.online("plug")
	e.m.askDue(t.Context())
	e.emit(mqttbus.KindStatus, "plug", "STATUS2", `{"StatusFWR":{"Version":"16.0.0(release-minimal)","Hardware":"ESP8266EX"}}`, false)
	e.drain(t.Context())
	if got := e.inv.get(1).Identity; got.Variant != "" || got.Hardware != "" {
		t.Fatalf("a minimal image from MQTT was stored: %+v", got)
	}
}

// Why: once the hardware is known (Status 2 over MQTT) INFO1 has nothing left to add, and a
// forged one must not change what was proven.
func TestMQTTIdentityIgnoresInfo1WhenTheHardwareIsKnown(t *testing.T) {
	d := oursDevice(1, "plug")
	d.Identity = model.Identity{Family: model.ChipESP8266, Hardware: "ESP8266EX", Version: "15.0.0", Image: "release-tasmota", Variant: "tasmota"}
	e := newIDEnv(d)
	e.emit(mqttbus.KindInfo, "plug", "INFO1", info1Payload, true)
	e.drain(t.Context())
	if e.writes() != 0 {
		t.Fatalf("INFO1 changed an identity that has hardware: %+v", e.inv.get(1).Identity)
	}
}

// Why: only devices recorded as reporting to our broker are read; a device on another broker
// has no business with our INFO1. An INFO1 that arrives before health recorded the device as
// ours (it is published once per boot) is kept and applied as soon as it is.
func TestMQTTIdentityReadsOnlyDevicesOnOurBroker(t *testing.T) {
	d := oursDevice(1, "plug")
	d.MQTT = model.MQTTOtherBroker
	e := newIDEnv(d)
	e.emit(mqttbus.KindInfo, "plug", "INFO1", info1Payload, false)
	e.drain(t.Context())
	e.emit(mqttbus.KindStatus, "plug", "STATUS2", status2Payload, false)
	e.drain(t.Context())
	if e.writes() != 0 {
		t.Fatalf("a device on another broker was read: %+v", e.inv.get(1).Identity)
	}
	e.inv.mu.Lock()
	dd := e.inv.devs[1]
	dd.MQTT = model.MQTTOurs
	e.inv.devs[1] = dd
	e.inv.mu.Unlock()
	e.advance(time.Second)
	e.m.askDue(t.Context())
	e.drain(t.Context())
	if got := e.inv.get(1).Identity.Version; got != "16.0.0" {
		t.Fatalf("the INFO1 seen before the device became ours was lost: %q", got)
	}
}

// Why: a reply that could not be stored (the device changed meanwhile) is no answer; the device
// must be asked again after the usual 10 minutes, not left without identity until a restart.
func TestMQTTIdentityCountsOnlyAStoredAnswer(t *testing.T) {
	e := newIDEnv(oursDevice(1, "plug"))
	e.bus.answers()
	e.online("plug")
	e.inv.fail = store.ErrConflict
	e.m.askDue(t.Context())
	e.inv.fail = nil
	e.advance(11 * time.Minute)
	e.m.askDue(t.Context())
	if n := len(e.bus.sent()); n != 2 {
		t.Fatalf("%d questions, want 2 (the first answer was not stored)", n)
	}
}

// Why: the topic table must not fill up with topics nobody configured (a flood on the broker),
// or real devices would stop being tracked.
func TestMQTTIdentityTracksOnlyInventoryTopics(t *testing.T) {
	e := newIDEnv(oursDevice(1, "plug"))
	e.m.known = func(m mqttbus.Message) bool { return m.Topic == "plug" }
	for i := range 2000 {
		e.emit(mqttbus.KindLWT, "junk"+strconv.Itoa(i), "LWT", "Online", false)
	}
	e.emit(mqttbus.KindLWT, "plug", "LWT", "Online", true)
	if n := len(e.m.topics); n != 1 {
		t.Fatalf("%d topics tracked, want 1", n)
	}
}

// Why: after the broker connection was lost, what was known about "online" is stale. The HTTP
// grace period must apply again after the reconnect, or a whole fleet is asked at once.
func TestMQTTIdentityAppliesTheGraceAgainAfterAReconnect(t *testing.T) {
	e := newIDEnv(oursDevice(1, "plug"))
	e.online("plug")
	e.bus.down = true
	e.m.askDue(t.Context())
	e.bus.down = false
	e.emit(mqttbus.KindLWT, "plug", "LWT", "Online", true) // the retained LWT, replayed
	e.m.askDue(t.Context())
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("asked right after the reconnect: %q", got)
	}
	e.advance(mqttIDGrace + time.Second)
	e.m.askDue(t.Context())
	if got := e.bus.sent(); len(got) != 1 {
		t.Fatalf("commands = %q after the grace, want 1", got)
	}
}

// Why (#239): the boot announcement carries the group topic for free, so the device list needs no
// extra read. INFO1 writes it as "cmnd/<group>/" (the prefix form), the list and Status 1 use the
// plain "<group>"; both must read the same. It is taken even when HTTP identified the device
// (the identity rules do not apply to it), but never from a retained message: that is not "now".
func TestMQTTIdentityTakesTheGroupTopicFromInfo1(t *testing.T) {
	d := oursDevice(1, "plug")
	d.Identity = model.Identity{Family: model.ChipESP8266, Hardware: "ESP8266EX", Version: "15.0.0", Image: "release-tasmota", Variant: "tasmota", DetectedAt: time.Unix(1700000000, 0)}
	e := newIDEnv(d)
	e.emit(mqttbus.KindInfo, "plug", "INFO1", info1Payload, true)
	e.drain(t.Context())
	if got := e.inv.get(1).GroupTopic; got != "" {
		t.Fatalf("a retained INFO1 gave group topic %q", got)
	}
	e.emit(mqttbus.KindInfo, "plug", "INFO1", info1Payload, false)
	e.drain(t.Context())
	got := e.inv.get(1)
	if got.GroupTopic != "tasmotas" || got.GroupTopicReadAt.IsZero() {
		t.Fatalf("group topic %q at %v, want tasmotas with a stamp", got.GroupTopic, got.GroupTopicReadAt)
	}
	for _, bad := range []string{"tasmotas/cmnd/", "cmnd/bad#/", "cmnd/" + strings.Repeat("a", 200) + "/"} {
		e.emit(mqttbus.KindInfo, "plug", "INFO1", `{"Info1":{"GroupTopic":"`+bad+`"}}`, false)
		e.drain(t.Context())
		if g := e.inv.get(1).GroupTopic; g != "tasmotas" {
			t.Fatalf("INFO1 %.30q changed the stored group topic to %.30q", bad, g)
		}
	}
	if len(e.bus.sent()) != 0 {
		t.Errorf("INFO1 caused commands: %q", e.bus.sent())
	}
}

// Why (#239, review): only the default FullTopic form "cmnd/<group>/" is unambiguous. With another
// FullTopic the same announcement would be stored as e.g. "tasmotas/cmnd" while Status 1 stores
// "tasmotas": the value would flip between reads and a topic equal to it would be missed (#260).
// Such a message is left to Status 1. A value any broker client could send that is too long or
// holds a wildcard or control character is refused, so the stored value stays.
func TestInfo1GroupTopicIsThePlainTopic(t *testing.T) {
	for in, want := range map[string]string{
		`{"Info1":{"GroupTopic":"cmnd/tasmotas/"}}`: "tasmotas",
		`{"Info1":{"GroupTopic":""}}`:               "",
	} {
		got, ok := info1GroupTopic([]byte(in))
		if !ok || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		`{"Info1":{"GroupTopic":"tasmotas/cmnd/"}}`, // FullTopic %topic%/%prefix%/
		`{"Info1":{"GroupTopic":"tasmotas"}}`,
		`{"Info1":{"GroupTopic":"my/cmnd/tasmotas/"}}`,
		`{"Info1":{"GroupTopic":"cmnd/a/b/"}}`,
		`{"Info1":{"GroupTopic":"cmnd/tasmotas"}}`,
		`{"Info1":{"GroupTopic":"cmnd/ta#smotas/"}}`,
		`{"Info1":{"GroupTopic":"cmnd/` + strings.Repeat("a", 200) + `/"}}`,
		`{"Info1":{"Version":"1"}}`, `{}`, `nope`,
	} {
		if g, ok := info1GroupTopic([]byte(in)); ok {
			t.Errorf("%.60s: took %.20q", in, g)
		}
	}
}

// Why: a question the caller abandoned after the publish (deadline) may have been answered; the
// device is not asked again for ten minutes. Only a question that was never sent (refused, offline,
// still queued when the deadline hit) may be retried at once.
func TestMQTTIdentityKeepsTheTenMinutesAfterAnAbandonedQuestion(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		reasked bool
	}{
		"sent, caller gave up": {fmt.Errorf("%w: %w", tasmota.ErrTimeout, context.DeadlineExceeded), false},
		"still queued":         {context.DeadlineExceeded, true},
		"offline":              {tasmota.ErrUnreachable, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newIDEnv(oursDevice(1, "plug"))
			e.bus.reply = func(string) (json.RawMessage, error) { return nil, tc.err }
			e.online("plug")
			e.m.askDue(t.Context())
			e.advance(2 * time.Second)
			e.m.askDue(t.Context())
			if got := len(e.bus.sent()) == 2; got != tc.reasked {
				t.Fatalf("commands %q, asked again = %v, want %v", e.bus.sent(), got, tc.reasked)
			}
		})
	}
}
