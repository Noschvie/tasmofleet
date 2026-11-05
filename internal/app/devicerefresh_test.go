package app

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/templates"
)

// refreshRig is a brokerRig whose identity and data reads are recorded too (#600).
type refreshRig struct {
	*brokerRig
	identity, data []model.DeviceID
	inFlight, peak atomic.Int32
}

func newRefreshRig(ids ...model.DeviceID) *refreshRig {
	g := &refreshRig{brokerRig: newBrokerRig(ids...)}
	step := func(rec *[]model.DeviceID, id model.DeviceID) error {
		if n := g.inFlight.Add(1); n > g.peak.Load() {
			g.peak.Store(n)
		}
		defer g.inFlight.Add(-1)
		*rec = append(*rec, id)
		return nil
	}
	read := g.r.read
	g.r.read = func(ctx context.Context, id model.DeviceID) error {
		g.inFlight.Add(1)
		defer g.inFlight.Add(-1)
		return read(ctx, id)
	}
	g.r.readIdentity = func(_ context.Context, id model.DeviceID) error { return step(&g.identity, id) }
	g.r.readData = func(_ context.Context, d model.Device) error { return step(&g.data, d.ID) }
	return g
}

func (g *refreshRig) takeAll() (broker, identity, data []model.DeviceID) {
	broker, identity, data = g.take(), g.identity, g.data
	g.identity, g.data = nil, nil
	return
}

// Why: a restart is the moment settings, policy values and identity may have changed (a restored
// config, a firmware update outside TasmoFleet), and a device announces it more than once (INFO3, the
// uptime in STATE, a second boot while it is still flapping). Every announcement must collapse into
// one refresh after the device settled, not one per message, or a boot loop would be read to death.
func TestRestartTriggersOneRefreshHoweverOftenItIsAnnounced(t *testing.T) {
	g := newRefreshRig(1)
	g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = g.clock.Add(time.Hour), g.clock.Add(time.Hour), g.clock.Add(time.Hour) // nothing periodic due
	for i := 0; i < 5; i++ {
		g.r.restarted(1)
		g.r.wantRestart(<-g.r.kickRestart) // what run does with each message
		g.advance(time.Second)
	}
	g.r.pass(t.Context())
	if b, i, d := g.takeAll(); len(b)+len(i)+len(d) != 0 {
		t.Fatalf("read before the device settled: %v %v %v", b, i, d)
	}
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	b, i, d := g.takeAll()
	if len(b) != 1 || len(i) != 1 || len(d) != 1 {
		t.Fatalf("a restart caused broker %v, identity %v, data %v reads, want one each", b, i, d)
	}
	g.advance(time.Minute)
	g.r.pass(t.Context())
	if b, i, d := g.takeAll(); len(b)+len(i)+len(d) != 0 {
		t.Fatalf("the restart was refreshed again: %v %v %v", b, i, d)
	}
}

// Why: a command that only changed the broker (MqttHost typed in the console) restarts the device too,
// but the broker read is the only thing it makes stale; it must not cost an identity probe and a full
// settings read.
func TestBrokerCommandReadsOnlyTheBroker(t *testing.T) {
	g := newRefreshRig(1)
	g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = g.clock.Add(time.Hour), g.clock.Add(time.Hour), g.clock.Add(time.Hour)
	g.r.want(1)
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if b, i, d := g.takeAll(); len(b) != 1 || len(i)+len(d) != 0 {
		t.Fatalf("broker %v, identity %v, data %v", b, i, d)
	}
}

// Why: the schedule is low-rate and spread: the data is read every six hours and the identity every day
// per device, and a fleet is visited one device per pass, never in one burst (200 devices at once would
// be thousands of requests in a second).
func TestScheduleSpreadsTheFleetAndKeepsItsIntervals(t *testing.T) {
	g := newRefreshRig(1, 2, 3)
	var seen []model.DeviceID
	for i := 0; i < 3; i++ {
		g.r.pass(t.Context())
		b, ident, d := g.takeAll()
		if len(b) != 1 || len(ident) != 1 || len(d) != 1 {
			t.Fatalf("pass %d visited %v %v %v, want exactly one device", i, b, ident, d)
		}
		seen = append(seen, b[0])
	}
	slices.Sort(seen)
	if !slices.Equal(seen, []model.DeviceID{1, 2, 3}) {
		t.Fatalf("three passes visited %v, want every device once", seen)
	}
	g.advance(6*time.Hour - time.Minute)
	g.r.pass(t.Context())
	if _, _, d := g.takeAll(); len(d) != 0 {
		t.Fatalf("data read again before six hours: %v", d)
	}
	g.advance(2 * time.Minute)
	var dataN, identN int
	for i := 0; i < 3; i++ {
		g.r.pass(t.Context())
		_, ident, d := g.takeAll()
		dataN, identN = dataN+len(d), identN+len(ident)
	}
	if dataN != 3 || identN != 0 {
		t.Fatalf("after six hours: %d data reads (want 3), %d identity reads (want 0, due after a day)", dataN, identN)
	}
	g.advance(24 * time.Hour)
	identN = 0
	for i := 0; i < 3; i++ {
		g.r.pass(t.Context())
		_, ident, _ := g.takeAll()
		identN += len(ident)
	}
	if identN != 3 {
		t.Fatalf("after a day: %d identity reads, want 3", identN)
	}
}

// Why: one request at a time per device. The reader is one goroutine and a visit does its steps one
// after the other, so a device never gets the broker read, the identity probe and the settings read
// together.
func TestVisitDoesItsStepsOneAfterTheOther(t *testing.T) {
	g := newRefreshRig(1)
	g.r.wantRestart(1)
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if p := g.peak.Load(); p != 1 {
		t.Fatalf("%d steps ran at the same time, want 1", p)
	}
	if b, i, d := g.takeAll(); len(b) != 1 || len(i) != 1 || len(d) != 1 {
		t.Fatalf("steps not all run: %v %v %v", b, i, d)
	}
}

// Why: a device that is offline (or not reached over HTTP) would hold the reader for every timeout, and a
// device a job or the person's restore holds must not get requests in the middle of it. Neither gets
// anything, and a busy device is visited as soon as it is free, not only after the next interval.
func TestOfflineAndBusyDevicesAreSkipped(t *testing.T) {
	g := newRefreshRig(1, 2)
	g.online[1] = false
	g.busy[2] = true
	g.r.wantRestart(1)
	g.r.wantRestart(2)
	g.advance(brokerHostSettle)
	for i := 0; i < 3; i++ {
		g.r.pass(t.Context())
	}
	if b, i, d := g.takeAll(); len(b)+len(i)+len(d) != 0 {
		t.Fatalf("offline or busy devices were read: %v %v %v", b, i, d)
	}
	g.busy[2] = false
	g.r.pass(t.Context())
	if b, i, d := g.takeAll(); !slices.Equal(b, []model.DeviceID{2}) || len(i) != 1 || len(d) != 1 {
		t.Fatalf("freed device: %v %v %v, want device 2 refreshed", b, i, d)
	}
}

// Why: the data read runs without the device lock, so the lock is only tried first; a job that takes the
// device between the broker read and the data read keeps the data read away.
func TestDataReadIsSkippedWhenAJobHoldsTheDevice(t *testing.T) {
	g := newRefreshRig(1)
	calls := 0
	g.r.enter = func(context.Context, model.DeviceID, string) (func(), error) {
		calls++
		if calls == 2 { // the data read's check
			return nil, devlock.BusyError{Holder: "update"}
		}
		return func() {}, nil
	}
	g.r.pass(t.Context())
	if _, _, d := g.takeAll(); len(d) != 0 {
		t.Fatalf("data read although a job held the device: %v", d)
	}
	g.r.enter = func(context.Context, model.DeviceID, string) (func(), error) { return func() {}, nil }
	g.r.pass(t.Context())
	if _, _, d := g.takeAll(); len(d) != 1 {
		t.Fatalf("data read after the job: %v, want one at the next visit", d)
	}
}

// Why: a failed data read must not be retried at every 30-second pass (a half-dead device would be
// hammered), nor wait six hours.
func TestFailedDataReadWaitsForTheRetry(t *testing.T) {
	g := newRefreshRig(1)
	g.r.readData = func(_ context.Context, d model.Device) error {
		g.data = append(g.data, d.ID)
		return errors.New("timeout")
	}
	g.r.pass(t.Context())
	g.advance(brokerHostTick)
	g.r.pass(t.Context())
	if len(g.data) != 1 {
		t.Fatalf("%d data reads, want 1 until the retry delay is over", len(g.data))
	}
	g.advance(brokerHostRetry)
	g.r.pass(t.Context())
	if len(g.data) != 2 {
		t.Fatalf("%d data reads after the retry delay, want 2", len(g.data))
	}
}

// Why: the point of #600. The refresh must land where the pages read from: the Settings tab's last-known
// values (#599), the policy values and the GPIO template reading, so the pages show them without a click.
// The device is the simulator, which records every command: Status 0 never reaches it (heap bursts
// corrupt ESP8266 replies), and the identity probe goes over HTTP.
func TestRefreshFillsThePageCachesAndNeverSendsStatus0(t *testing.T) {
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	sim, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M})
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitAddr(t, sim.Addr())
	dev := model.Device{ID: 1, Name: "plug", Address: host, Port: port, Identity: model.Identity{Family: model.ChipESP8266}}
	cl := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()})
	set := settings.New(settings.Options{Devices: addrGetter{host, port}, Client: cl})
	rep := &policyReporter{devices: fakeLister{dev}, reader: set, now: time.Now}
	rep.setRules([]policy.Rule{{ID: "r1", Command: "PowerOnState", Want: "3"}})
	tpl := templates.New(templates.Options{Devices: tplDevices{dev}, Client: cl})
	a := &runtime{settings: set, policyLive: rep, templates: tpl}
	if _, ok := set.Last(1); ok {
		t.Fatal("the Settings cache was filled before the refresh")
	}
	if err := a.refreshData(t.Context(), dev); err != nil {
		t.Fatal(err)
	}
	if snap, ok := set.Last(1); !ok || len(snap.Values) == 0 {
		t.Fatalf("the Settings tab has no last-known values after the refresh: %v", snap)
	}
	if v, ok := rep.vals.get(1)["PowerOnState"]; !ok || v.value == "" {
		t.Fatalf("the policy values were not refreshed: %v", rep.vals.get(1))
	}
	ov, err := tpl.Overview(t.Context(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	live := false
	for _, gv := range append(ov.Groups, ov.BuiltIn...) {
		for _, v := range gv.Devices {
			live = live || v.Source == templates.SourceLive
		}
	}
	for _, v := range ov.NotJudged {
		live = live || v.Source == templates.SourceLive
	}
	if !live {
		t.Fatal("the GPIO template reading was not cached")
	}
	for _, c := range sim.Commands() {
		if strings.EqualFold(strings.Join(strings.Fields(c), " "), "status 0") {
			t.Fatalf("Status 0 reached the device: %v", sim.Commands())
		}
	}
}

type tplDevices struct{ d model.Device }

func (t tplDevices) ListDevices(context.Context) ([]model.Device, error) {
	return []model.Device{t.d}, nil
}
func (t tplDevices) GetDevice(context.Context, model.DeviceID) (model.Device, error) {
	return t.d, nil
}

// Why: the app must really feed the reader; a wiring that forgets the identity probe or the data read, or
// the tracker's restart hook, would leave every test above green.
func TestAppWiresTheDeviceRefresh(t *testing.T) {
	a := &runtime{svc: &Services{Prober: &Prober{}}}
	r := newBrokerHostReader(nil, nil, nil, nil, nil)
	a.wireDeviceRefresh(r)
	if r.readIdentity == nil || r.readData == nil {
		t.Fatal("the reader has no identity or data read")
	}
	a.brokerHost = r
	stab, ok := a.healthDeps().Stability.(bootWatch)
	if !ok {
		t.Fatalf("health records restarts through %T", a.healthDeps().Stability)
	}
	stab.onBoot(7)
	if len(r.kickRestart) != 1 {
		t.Fatal("a recorded restart does not ask for a refresh")
	}
}

// Why: the whole chain as the app wires it (startBrokerHost): an online device that was never refreshed is
// visited by the broker reader's own pass, and afterwards the Settings tab has its last-known values and
// the identity was probed over HTTP, with no page open and no click. The reader's loop is not running (the
// context is already over when it starts) so the pass is driven by hand.
func TestReaderPassRefreshesWhatThePagesShow(t *testing.T) {
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	sim, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M})
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitAddr(t, sim.Addr())
	cl := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()})
	st := openBrokerStore(t)
	p := &Prober{Client: cl, Gap: time.Millisecond, Broker: fixedBroker{host: "broker.example"}}
	desc, err := p.Describe(t.Context(), model.Device{Address: host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.CreateDevice(t.Context(), desc)
	if err != nil {
		t.Fatal(err)
	}
	p.Devices = st
	dev, _ := st.GetDevice(t.Context(), id)
	set := settings.New(settings.Options{Devices: st, Client: cl})
	a := &runtime{
		svc: &Services{Store: st, Prober: p}, brokerHost: newBrokerHostReader(nil, nil, p.ReadBroker, nil, slog.New(slog.DiscardHandler)),
		fwHolds: devlock.NewFirmwareHolds(time.Now, time.Minute), tracker: health.New(health.Deps{}, health.Options{}),
		settings: set, policyLive: &policyReporter{devices: fakeLister{dev}, reader: set},
		templates: templates.New(templates.Options{Devices: tplDevices{dev}, Client: cl}),
	}
	over, stop := context.WithCancel(t.Context())
	stop()
	a.startBrokerHost(over)
	a.bgWG.Wait()
	a.brokerHost.list = func(context.Context) ([]model.Device, error) { return []model.Device{dev}, nil }
	a.brokerHost.httpUp = func(model.DeviceID) bool { return true }
	before := len(sim.Commands())
	a.brokerHost.pass(t.Context())
	if snap, ok := set.Last(id); !ok || len(snap.Values) == 0 {
		t.Fatal("the pass left the Settings tab without last-known values")
	}
	cmds := sim.Commands()[before:]
	if !slices.Contains(cmds, "Status 2") {
		t.Fatalf("the identity was not probed: %v", cmds)
	}
	if !slices.Contains(cmds, "Status 6") {
		t.Fatalf("the broker was not read: %v", cmds)
	}
}

// Why: the schedule refreshes facts, it must not rename things on its own. An entry that still carries a
// name TasmoFleet filled in follows the device's DeviceName on an explicit refresh (#91), which is what the
// person asked for there; done in the background every day it would change the names in a list the person
// knows, 30 seconds after an upgrade, with no click behind it. The contrast (Refresh does rename) proves
// the device really reports another name.
func TestScheduledIdentityRefreshRenamesNothing(t *testing.T) {
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	sim, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP32})
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitAddr(t, sim.Addr())
	cl := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()})
	st := openBrokerStore(t)
	p := &Prober{Client: cl, Gap: time.Millisecond, Broker: fixedBroker{host: "broker.example"}, Devices: st}
	desc, err := p.Describe(t.Context(), model.Device{Address: host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	desc.Name = inventory.CleanName(desc.Topic) // a name TasmoFleet filled in from the topic
	id, err := st.CreateDevice(t.Context(), desc)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RefreshFacts(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.GetDevice(t.Context(), id); d.Name != desc.Name {
		t.Fatalf("the scheduled refresh renamed %q to %q", desc.Name, d.Name)
	}
	if err := p.Refresh(t.Context(), id, ""); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.GetDevice(t.Context(), id); d.Name == desc.Name {
		t.Fatal("an explicit refresh did not rename either: the test proves nothing")
	}
}
