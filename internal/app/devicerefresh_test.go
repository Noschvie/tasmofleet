package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
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

// Why: the schedule is low-rate and spread: the data and the identity are read once a day per device (the
// broker every six hours), and a fleet is visited one device per pass, never in one burst (200 devices at
// once would be thousands of requests in a second).
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
	g.advance(24*time.Hour - time.Minute) // the broker is due again (every six hours), the rest not yet
	var dataN, identN int
	for i := 0; i < 3; i++ {
		g.r.pass(t.Context())
		_, ident, d := g.takeAll()
		dataN, identN = dataN+len(d), identN+len(ident)
	}
	if dataN != 0 || identN != 0 {
		t.Fatalf("before a day had passed: %d data and %d identity reads, want none", dataN, identN)
	}
	g.advance(2 * time.Minute)
	for i := 0; i < 3; i++ {
		g.r.pass(t.Context())
		_, ident, d := g.takeAll()
		dataN, identN = dataN+len(d), identN+len(ident)
	}
	if dataN != 3 || identN != 3 {
		t.Fatalf("after a day: %d data and %d identity reads, want 3 and 3", dataN, identN)
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
	a := &runtime{svc: &Services{Prober: &Prober{Client: cl, Gap: time.Millisecond}}, settings: set, policyLive: rep, templates: tpl}
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

// Why: a device that is still flapping announces every boot, and the read must wait until it settled after
// the last one, not run in the middle of the next boot. A broker command in between (MqttHost typed while it
// was booting) must not drop the restart's refresh either.
func TestRestartReadWaitsForTheLastAnnouncementAndKeepsItsRefresh(t *testing.T) {
	g := newRefreshRig(1)
	g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = g.clock.Add(time.Hour), g.clock.Add(time.Hour), g.clock.Add(time.Hour)
	g.r.wantRestart(1)
	g.advance(15 * time.Second)
	g.r.want(1)
	g.advance(10 * time.Second) // settled after the first announcement, not after the second
	g.r.pass(t.Context())
	if b, i, d := g.takeAll(); len(b)+len(i)+len(d) != 0 {
		t.Fatalf("read %v %v %v 10 s after the last announcement", b, i, d)
	}
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if b, i, d := g.takeAll(); len(b) != 1 || len(i) != 1 || len(d) != 1 {
		t.Fatalf("broker %v, identity %v, data %v once settled, want one each", b, i, d)
	}
}

// Why: a device in a boot loop (the stability page calls three restarts in an hour one) announces every boot,
// and a full refresh after each would send it some 35 requests per boot where the broker re-read of #598 sent
// two: a crashing device, often an ESP8266 low on heap, would be read to death. Its reads mostly fail (it
// reboots in the middle), and a failed read must not bring them back every ten minutes either. A restart reads
// the identity and the data only if no restart read them in the last hour, the rest is read once when that
// hour is over, and the broker is still read after every restart.
func TestBootLoopIsReadInFullOnceAnHour(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"reads succeed", nil},
		{"reads fail, the device reboots in the middle", errors.New("timeout")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRefreshRig(1)
			far := g.clock.Add(48 * time.Hour)
			g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = far, far, far // nothing periodic due
			var ident, data int
			g.r.readIdentity = func(context.Context, model.DeviceID) error { ident++; return tc.err }
			g.r.readData = func(context.Context, model.Device) error { data++; return tc.err }
			broker := 0
			for range 26 { // a restart every seven minutes for three hours
				g.r.wantRestart(1)
				for range 14 {
					g.advance(30 * time.Second)
					g.r.pass(t.Context())
					broker += len(g.take())
				}
			}
			if broker != 26 || ident != 4 || data != 4 {
				t.Fatalf("three hours of restarts every seven minutes read the broker %d, the identity %d and the data %d times, want 26, 4, 4 (the first restart, then once an hour)", broker, ident, data)
			}
			g.advance(2 * time.Hour) // a restart after a quiet spell is read in full at once
			g.r.wantRestart(1)
			g.advance(brokerHostSettle)
			g.r.pass(t.Context())
			if ident != 5 || data != 5 {
				t.Fatalf("a restart after a quiet spell: %d identity and %d data reads, want 5 and 5", ident, data)
			}
		})
	}
}

// Why: a refused identity refresh is an answer, not a failure: another device answers at the stored address
// (its MAC is not the entry's), and the prober logs and audits each refusal. Retried every ten minutes like a
// probe that got no answer, it would write 144 audit rows a day for such a device. A refusal is asked again at
// the regular interval; a probe without an answer is retried after the retry delay.
func TestRefusedIdentityIsNotRetriedLikeAFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		again time.Duration // when the identity is probed again
	}{
		{"refused", &RefreshRefusedError{Reported: "02:00:00:00:00:99", Reason: refusedOther}, 24 * time.Hour},
		{"no answer", tasmota.ErrTimeout, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRefreshRig(1)
			var probes []time.Time
			g.r.readIdentity = func(context.Context, model.DeviceID) error {
				probes = append(probes, g.clock)
				return tc.err
			}
			for range 25 * 60 {
				g.r.pass(t.Context())
				g.advance(time.Minute)
			}
			if len(probes) < 2 {
				t.Fatalf("probed %d times in 25 hours", len(probes))
			}
			if gap := probes[1].Sub(probes[0]); gap < tc.again || gap > tc.again+time.Minute {
				t.Fatalf("probed again after %v, want %v", gap, tc.again)
			}
		})
	}
}

// Why: a device a job holds gets nothing, and must not cost the others their turn: when only its data was
// due, nothing was sent to it, so the pass goes on to the next device. Otherwise a device held through a long
// update batch would stop the schedule of every device listed after it.
func TestBusyDeviceDoesNotStopTheScheduleOfTheOthers(t *testing.T) {
	g := newRefreshRig(1, 2)
	far := g.clock.Add(time.Hour)
	for _, id := range []model.DeviceID{1, 2} {
		g.r.next[id], g.r.identityNext[id] = far, far // only the data is due
	}
	g.busy[1] = true
	g.r.pass(t.Context())
	if _, _, d := g.takeAll(); !slices.Equal(d, []model.DeviceID{2}) {
		t.Fatalf("data read %v in the pass where device 1 is held, want [2]", d)
	}
}

// Why: the data read takes 10 to 30 s per device, and a person who saves the Settings tab meanwhile must not be
// refused as busy: the read only tries the device lock (a job that holds it keeps the read away) and gives it
// back before it sends anything.
func TestDataReadLeavesTheDeviceLockFree(t *testing.T) {
	g := newRefreshRig(1)
	locks := devlock.New()
	g.r.enter = func(_ context.Context, id model.DeviceID, owner string) (func(), error) {
		release, holder, ok := locks.TryAcquire(id, owner)
		if !ok {
			return nil, devlock.BusyError{Holder: holder}
		}
		return release, nil
	}
	saved := false
	g.r.readData = func(_ context.Context, d model.Device) error {
		release, _, ok := locks.TryAcquire(d.ID, "settings") // a save of the Settings tab while the read runs
		if ok {
			release()
		}
		saved = ok
		return nil
	}
	g.r.pass(t.Context())
	if !saved {
		t.Fatal("a save during the data read was refused as busy: the read holds the device lock")
	}
}

// Why: Pause on the policy page and an interval of 0 switch the background policy reads off ("pause or resume
// the background reads", spec 6.4 routes); an admin uses them to stop TasmoFleet asking the fleet for those
// values. The data read keeps to them like the policy poller does. The Settings values and the template
// reading are not policy reads and are still read.
func TestPausedPolicyLiveCheckIsNotReadInTheBackground(t *testing.T) {
	for _, tc := range []struct {
		name         string
		paused, zero bool
		read         bool
	}{
		{"running", false, false, true},
		{"paused on the policy page", true, false, false},
		{"interval 0", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := liveRuntime(t)
			a.svc.Prober = &Prober{} // the device has no MAC: nothing to verify
			if tc.zero {
				if _, err := a.svc.Sys.Apply(t.Context(), sysconfig.Request{Actor: "admin", Values: map[string]string{"POLICY_LIVE_INTERVAL": "0s"}}); err != nil {
					t.Fatal(err)
				}
			}
			lp, err := sysconfig.NewLivePause(t.Context(), &memSettings{rows: map[string]store.SystemSetting{}}, nopAudit{})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := lp.Set(t.Context(), "admin", tc.paused, nil); err != nil {
				t.Fatal(err)
			}
			a.livePause = lp
			reader := &countingReader{}
			a.policyLive = &policyReporter{reader: reader, now: time.Now}
			a.policyLive.setRules([]policy.Rule{{ID: "r1", Command: "SaveData", Want: "0"}})
			if err := a.refreshData(t.Context(), model.Device{ID: 1}); err != nil {
				t.Fatal(err)
			}
			if got := reader.calls > 0; got != tc.read {
				t.Fatalf("policy values read: %v, want %v", got, tc.read)
			}
		})
	}
}

// anotherDevice is a simulator device that the inventory knows under the MAC of a different device: the
// address a DHCP lease handed over, or an entry whose address was typed wrong.
type anotherDevice struct {
	sim       simulator.Device
	cl        tasmota.Client
	p         *Prober
	st        *store.SQLite
	id        model.DeviceID
	stored    model.Device
	setCaches *settings.Manager
}

func newAnotherDevice(t *testing.T) anotherDevice {
	t.Helper()
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
	p := &Prober{Client: cl, Gap: time.Millisecond, Broker: fixedBroker{host: "broker.example"}, Devices: st}
	desc, err := p.Describe(t.Context(), model.Device{Address: host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	desc.MAC, desc.MACs = "02:00:00:00:00:99", nil // not the MAC the device at the address reports
	desc.MQTTHost, desc.MQTTPort = "stored-broker.example", 1883
	id, err := st.CreateDevice(t.Context(), desc)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := st.GetDevice(t.Context(), id)
	return anotherDevice{sim: sim, cl: cl, p: p, st: st, id: id, stored: stored, setCaches: settings.New(settings.Options{Devices: st, Client: cl})}
}

// Why: a device that answers at the address of another entry must not fill that entry. The data read checks
// the MAC first (one Status 5, the rule of ReadNet) and sends nothing else when it does not match: no Settings
// values, policy values or template reading of a stranger under this device's name.
func TestDataReadFailsClosedForAnotherDevice(t *testing.T) {
	o := newAnotherDevice(t)
	rep := &policyReporter{devices: fakeLister{o.stored}, reader: o.setCaches, now: time.Now}
	rep.setRules([]policy.Rule{{ID: "r1", Command: "PowerOnState", Want: "3"}})
	tpl := templates.New(templates.Options{Devices: tplDevices{o.stored}, Client: o.cl, Backups: noBackups{}})
	a := &runtime{svc: &Services{Prober: o.p}, settings: o.setCaches, policyLive: rep, templates: tpl}
	before := len(o.sim.Commands())
	if err := a.refreshData(t.Context(), o.stored); !errors.Is(err, errAnotherDevice) {
		t.Fatalf("refreshData = %v, want errAnotherDevice", err)
	}
	if _, ok := o.setCaches.Last(o.id); ok {
		t.Fatal("the Settings cache was filled from another device")
	}
	if len(rep.vals.get(o.id)) != 0 {
		t.Fatal("the policy values were filled from another device")
	}
	for _, gv := range func() []templates.GroupView {
		ov, _ := tpl.Overview(t.Context(), nil, false)
		return append(ov.Groups, ov.BuiltIn...)
	}() {
		for _, v := range gv.Devices {
			if v.Source == templates.SourceLive {
				t.Fatal("the template reading was filled from another device")
			}
		}
	}
	if cmds := o.sim.Commands()[before:]; len(cmds) != 1 || cmds[0] != "Status 5" {
		t.Fatalf("the other device received %v, want only the MAC check", cmds)
	}
}

// Why: the broker host is stored on the entry; the host of another device must not become this device's.
func TestBrokerReadFailsClosedForAnotherDevice(t *testing.T) {
	o := newAnotherDevice(t)
	if err := o.p.ReadBroker(t.Context(), o.id); !errors.Is(err, errAnotherDevice) {
		t.Fatalf("ReadBroker = %v, want errAnotherDevice", err)
	}
	if d, _ := o.st.GetDevice(t.Context(), o.id); d.MQTTHost != "stored-broker.example" {
		t.Fatalf("the broker host became %q from another device", d.MQTTHost)
	}
}

// Why: the scheduled identity refresh stores IP, hostname, network facts, firmware, broker, group topic and
// relay names; none of it may come from an answer whose MAC is not the stored one.
func TestScheduledIdentityRefreshStoresNothingFromAnotherDevice(t *testing.T) {
	o := newAnotherDevice(t)
	var refused *RefreshRefusedError
	if err := o.p.RefreshFacts(t.Context(), o.id); !errors.As(err, &refused) {
		t.Fatalf("RefreshFacts = %v, want a refusal", err)
	}
	after, _ := o.st.GetDevice(t.Context(), o.id)
	if !reflect.DeepEqual(after.Net, o.stored.Net) || after.Identity != o.stored.Identity || after.MQTTHost != o.stored.MQTTHost ||
		after.GroupTopic != o.stored.GroupTopic || !slices.Equal(after.RelayNames, o.stored.RelayNames) || after.MAC != o.stored.MAC {
		t.Fatalf("facts of another device were stored: %+v", after)
	}
}

// Why: after a refused identity refresh nothing is read from that address for the device until a check
// matches again: the data read is not started in the same visit and not for a day, and the refusal does not
// make the broker read retry every ten minutes either.
func TestRefusedIdentityHoldsBackTheDataRead(t *testing.T) {
	g := newRefreshRig(1)
	g.r.readIdentity = func(context.Context, model.DeviceID) error { return &RefreshRefusedError{Reason: refusedOther} }
	g.r.pass(t.Context())
	if _, _, d := g.takeAll(); len(d) != 0 {
		t.Fatalf("data read in the visit whose identity refresh was refused: %v", d)
	}
	g.advance(23 * time.Hour)
	g.r.pass(t.Context())
	if _, _, d := g.takeAll(); len(d) != 0 {
		t.Fatalf("data read before a day had passed: %v", d)
	}
}

// Why: a data read that finds another device at the address, or a device that cannot show its MAC (minimal
// firmware, #662), leaves the caches alone and is asked again at the broker's interval, not at every pass.
func TestAnotherDeviceInTheDataReadIsNotRetriedAtEveryPass(t *testing.T) {
	for name, refusal := range map[string]error{"another MAC": errAnotherDevice, "no MAC (minimal firmware)": settings.ErrNoMAC} {
		t.Run(name, func(t *testing.T) {
			g := newRefreshRig(1)
			n := 0
			g.r.readData = func(context.Context, model.Device) error { n++; return refusal }
			for range 5 {
				g.r.pass(t.Context())
				g.advance(brokerHostTick)
			}
			g.advance(brokerHostRetry + time.Minute)
			g.r.pass(t.Context())
			if n != 1 {
				t.Fatalf("%d data reads within the retry delay of a failure, want 1: this is not a failure", n)
			}
			g.advance(brokerHostEvery)
			g.r.pass(t.Context())
			if n != 2 {
				t.Fatalf("%d data reads after the broker interval, want 2", n)
			}
		})
	}
}

// Why: a broker read that finds another device at the address is an answer, not a failure, like a refused
// identity refresh: asked again at the regular interval, also when a restart asked for the read. Retried like a
// read that got no answer, the device at the address would get a Status 5 every ten minutes, and after every
// restart announcement one every two minutes for half an hour.
func TestAnotherDeviceInTheBrokerReadIsAskedAgainAtTheRegularInterval(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
	}{
		{"periodic", false},
		{"asked for by a restart", true},
	} {
		for name, refusal := range map[string]error{"another MAC": errAnotherDevice, "no MAC (minimal firmware)": settings.ErrNoMAC} {
			t.Run(tc.name+", "+name, func(t *testing.T) {
				g := newRefreshRig(1)
				far := g.clock.Add(48 * time.Hour)
				g.r.identityNext[1], g.r.dataNext[1] = far, far
				if tc.restart {
					g.r.next[1] = far
					g.r.wantRestart(1)
				}
				var reads []time.Time
				g.r.read = func(context.Context, model.DeviceID) error {
					reads = append(reads, g.clock)
					return refusal
				}
				for range 7 * 120 { // seven hours of passes
					g.advance(brokerHostTick)
					g.r.pass(t.Context())
				}
				if len(reads) != 2 || reads[1].Sub(reads[0]) < brokerHostEvery {
					t.Fatalf("broker read %d times (%v), want twice, %v apart: another device is not a failure", len(reads), reads, brokerHostEvery)
				}
			})
		}
	}
}

// Why: the MAC check of the background reads must not refuse a healthy device, or its broker, Settings values,
// policy values and template reading would quietly stop being refreshed: a board on Ethernet reports its Wi-Fi
// and its Ethernet MAC, and the entry may know either or both. It must refuse what cannot be shown to be this
// device: another MAC, and minimal firmware, which reports none (the rule of a refresh, checkMAC). An entry
// without a MAC has nothing to compare.
func TestVerifyAddressAcceptsEveryMACOfTheStoredSetAndNothingElse(t *testing.T) {
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	wired, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "wired", Profile: simulator.ProfileESP32, Wired: true})
	if err != nil {
		t.Fatal(err)
	}
	minimal, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "minimal", Profile: simulator.ProfileESP8266Min})
	if err != nil {
		t.Fatal(err)
	}
	p := &Prober{Client: tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()}), Gap: time.Millisecond}
	at := func(sim simulator.Device, macs ...string) model.Device {
		host, port := splitAddr(t, sim.Addr())
		d := model.Device{ID: 1, Address: host, Port: port, MACs: macs}
		if len(macs) > 0 {
			d.MAC = macs[0]
		}
		return d
	}
	desc, err := p.Describe(t.Context(), at(wired))
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.MACs) != 2 {
		t.Fatalf("the wired board reports %v, want its Wi-Fi and its Ethernet MAC", desc.MACs)
	}
	wifi, eth := desc.MACs[0], desc.MACs[1]
	for _, tc := range []struct {
		name string
		d    model.Device
		want error
	}{
		{"the Wi-Fi MAC stored", at(wired, wifi), nil},
		{"only the Ethernet MAC stored", at(wired, eth), nil},
		{"both stored", at(wired, wifi, eth), nil},
		{"no MAC stored", at(wired), nil},
		{"another MAC stored", at(wired, "02:00:00:00:00:99"), errAnotherDevice},
		{"minimal firmware, which reports no MAC", at(minimal, wifi), settings.ErrNoMAC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := p.VerifyAddress(t.Context(), tc.d); !errors.Is(err, tc.want) {
				t.Fatalf("VerifyAddress = %v, want %v", err, tc.want)
			}
		})
	}
}

// noBackups: a template overview without any stored backup.
type noBackups struct{}

func (noBackups) List(context.Context, model.DeviceID) ([]model.Backup, error) { return nil, nil }
func (noBackups) Open(context.Context, model.BackupID) ([]byte, model.Backup, error) {
	return nil, model.Backup{}, errors.New("no backups")
}

// swapClient answers like the device behind it until it has answered one Status 5; from then on the
// address belongs to another device (a DHCP lease handed over between two reads).
type swapClient struct {
	tasmota.Client
	status5 atomic.Int32
}

func (s *swapClient) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	raw, err := s.Client.Command(ctx, t, cmnd)
	if err == nil && cmnd == "Status 5" && s.status5.Add(1) > 1 {
		return json.RawMessage(regexp.MustCompile(`(?i)"Mac":\s*"[0-9a-f:]+"`).ReplaceAll(raw, []byte(`"Mac":"02:00:00:00:00:99"`))), nil
	}
	return raw, err
}

// storedDevice is a simulator device stored under its own MAC, with its address answered through swap.
func storedDevice(t *testing.T) (o anotherDevice, swap *swapClient) {
	t.Helper()
	o = newAnotherDevice(t)
	desc, err := o.p.Describe(t.Context(), model.Device{Address: o.stored.Address, Port: o.stored.Port})
	if err != nil || desc.MAC == "" {
		t.Fatalf("describe: %v (MAC %q)", err, desc.MAC)
	}
	if err := o.st.DeleteDevice(t.Context(), o.id); err != nil {
		t.Fatal(err)
	}
	if o.id, err = o.st.CreateDevice(t.Context(), desc); err != nil {
		t.Fatal(err)
	}
	o.stored, _ = o.st.GetDevice(t.Context(), o.id)
	swap = &swapClient{Client: o.cl}
	o.p = &Prober{Client: swap, Gap: time.Millisecond, Broker: fixedBroker{host: "broker.example"}, Devices: o.st}
	return o, swap
}

// Why (#662): the MAC check before the data read (#658) is one request ahead of the reads. An address
// that changes hands after it must still not fill the Settings cache: the service asks again once it has
// the values, and nothing is remembered from an answer whose MAC is not the stored one.
func TestSettingsValuesFromAnAddressThatChangedHandsAreNotRemembered(t *testing.T) {
	o, _ := storedDevice(t)
	set := settings.New(settings.Options{Devices: o.st, Client: o.cl, Verify: o.p.VerifyAddress})
	a := &runtime{svc: &Services{Prober: o.p}, settings: set}
	if err := a.refreshData(t.Context(), o.stored); !errors.Is(err, errAnotherDevice) {
		t.Fatalf("refreshData = %v, want errAnotherDevice", err)
	}
	if snap, ok := set.Last(o.id); ok {
		t.Fatalf("the Settings cache holds %d values of another device", len(snap.Values))
	}
}

// Why (#662): the Settings tab's own read (a page open) had no MAC check at all.
func TestAPageReadOfAnotherDeviceIsRefusedAndNotRemembered(t *testing.T) {
	o := newAnotherDevice(t)
	set := settings.New(settings.Options{Devices: o.st, Client: o.cl, Verify: o.p.VerifyAddress})
	if _, err := set.Read(t.Context(), o.id); !errors.Is(err, errAnotherDevice) {
		t.Fatalf("Read = %v, want errAnotherDevice", err)
	}
	if _, ok := set.Last(o.id); ok {
		t.Fatal("the values of another device are remembered as the last known ones")
	}
}

// Why (#662): the policy poller and the on-demand check cache what they read as the device's values and
// judge it by them; another device's answers must not be among them.
func TestPolicyValuesOfAnotherDeviceAreNotCached(t *testing.T) {
	o := newAnotherDevice(t)
	rep := &policyReporter{devices: fakeLister{o.stored}, reader: o.setCaches, verify: o.p.VerifyAddress, now: time.Now}
	rep.setRules([]policy.Rule{{ID: "r1", Command: "PowerOnState", Want: "3"}})
	if rep.readDevice(t.Context(), o.stored) {
		t.Fatal("a read of another device is reported as successful")
	}
	if len(rep.vals.get(o.id)) != 0 {
		t.Fatalf("the policy values hold %v of another device", rep.vals.get(o.id))
	}
	// the stored device's own answers still arrive: the same rig with its MAC
	own, _ := storedDevice(t)
	rep = &policyReporter{devices: fakeLister{own.stored}, reader: own.setCaches, verify: own.p.VerifyAddress, now: time.Now}
	rep.setRules([]policy.Rule{{ID: "r1", Command: "PowerOnState", Want: "3"}})
	rep.reader = settings.New(settings.Options{Devices: own.st, Client: own.cl})
	if !rep.readDevice(t.Context(), own.stored) || len(rep.vals.get(own.id)) == 0 {
		t.Fatalf("the stored device's values were not cached: %v", rep.vals.get(own.id))
	}
}

// Why (#662): a console write changes device settings without Apply. It must forget what the Settings
// service remembers (and count the write, so a read under way is not remembered): a command that only
// queries, or acts without changing a setting, must not, or every Status in the console would wipe the
// last-known values.
func TestConsoleWritesForgetTheRememberedSettingsAndQueriesDoNot(t *testing.T) {
	o := newAnotherDevice(t)
	set := settings.New(settings.Options{Devices: o.st, Client: o.cl})
	a := &runtime{settings: set}
	h := holdingCmd{Service: errCmd{}, onWrite: a.settingsWritten}
	admin := model.Principal{Role: model.RoleAdmin}
	for _, c := range []struct {
		cmd    string
		forget bool
	}{
		{"Topic", false},
		{"Status 5", false},
		{"Power1 toggle", false},
		{"Restart 1", false},
		{"FriendlyName1 Lamp", true},
		{"Backlog SetOption4 1; Topic x", true},
	} {
		if _, err := set.Read(t.Context(), o.id); err != nil {
			t.Fatal(err)
		}
		if _, ok := set.Last(o.id); !ok {
			t.Fatal("the read did not remember anything")
		}
		_, _ = h.Run(t.Context(), admin, o.id, c.cmd)
		if _, ok := set.Last(o.id); ok == c.forget {
			t.Errorf("%q: values remembered after it = %v, want %v", c.cmd, ok, !c.forget)
		}
	}
	// a command the policy refused never reached the device
	if _, err := set.Read(t.Context(), o.id); err != nil {
		t.Fatal(err)
	}
	_, _ = holdingCmd{Service: errCmd{err: cmdpolicy.ErrForbidden}, onWrite: a.settingsWritten}.Run(t.Context(), admin, o.id, "FriendlyName1 Lamp")
	if _, ok := set.Last(o.id); !ok {
		t.Error("a refused command made the service forget")
	}
}

// Why (#662): Make compliant writes settings through the fixer, not through Apply; a failed run may
// have sent part of its lines. Either way what was remembered no longer describes the device.
func TestMakeCompliantForgetsTheRememberedSettings(t *testing.T) {
	o := newAnotherDevice(t)
	set := settings.New(settings.Options{Devices: o.st, Client: o.cl})
	a := &runtime{settings: set, fixer: &policy.Fixer{Client: o.cl, Reader: set}, policyLive: &policyReporter{now: time.Now}}
	if _, err := set.Read(t.Context(), o.id); err != nil {
		t.Fatal(err)
	}
	if err := a.policyApplier().Apply(t.Context(), o.stored, []string{"FriendlyName1 Lamp"}); err != nil {
		t.Logf("apply: %v (the run may fail; the values must be forgotten either way)", err)
	}
	if _, ok := set.Last(o.id); ok {
		t.Error("Make compliant left the remembered values in place")
	}
}

// Why (#662): the MAC check after the policy read must not swallow the read's own error. A device that
// answered some commands and then timed out was read only in part; when it shows the stored MAC right
// after, the poller must still hear "failed" and read it again soon (and the data refresh must still say
// "policy values not read"), not wait for the next regular round with half the values.
func TestAPolicyReadThatFailedHalfWayStaysAFailure(t *testing.T) {
	rep := &policyReporter{reader: partialReader{"PowerOnState": "3"}, verify: func(context.Context, model.Device) error { return nil }, now: time.Now}
	rep.setRules([]policy.Rule{{ID: "r1", Command: "PowerOnState", Want: "3"}, {ID: "r2", Command: "LedState", Want: "1"}})
	if rep.readDevice(t.Context(), model.Device{ID: 7}) {
		t.Fatal("a read that failed half way is reported as complete")
	}
	if got := rep.vals.get(7); len(got) != 1 { // a dead device keeps what it answered
		t.Fatalf("cached %v, want the one value the device answered", got)
	}
}

// partialReader answers its values and then fails, like a device that stops answering half way.
type partialReader map[string]string

func (r partialReader) ReadKeys(context.Context, model.DeviceID, []string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range r {
		out[k] = v
	}
	return out, tasmota.ErrTimeout
}

// overMQTT stands in for the router's MQTT route to one device: it answers what the device answers on its
// topic (Status 5 with mac), or, once held, nothing until the request's context ends.
type overMQTT struct {
	mu      sync.Mutex
	mac     string
	held    bool
	waiting chan struct{} // closed when the first held command waits
	once    sync.Once
}

func (m *overMQTT) set(mac string, held bool) {
	m.mu.Lock()
	m.mac, m.held = mac, held
	m.mu.Unlock()
}

func (m *overMQTT) Command(ctx context.Context, _ tasmota.Target, cmnd string) (json.RawMessage, error) {
	m.mu.Lock()
	mac, held := m.mac, m.held
	m.mu.Unlock()
	if held {
		m.once.Do(func() { close(m.waiting) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	switch cmnd {
	case "Status 5":
		return json.RawMessage(`{"StatusNET":{"Mac":"` + mac + `"}}`), nil
	case "Topic":
		return json.RawMessage(`{"Topic":"plug"}`), nil
	case "Status": // PowerOnState is read from here, never bare
		return json.RawMessage(`{"Status":{"Topic":"plug","PowerOnState":3,"SaveData":1}}`), nil
	case "FriendlyName1 Lamp":
		return json.RawMessage(`{"FriendlyName1":"Lamp"}`), nil
	}
	return nil, tasmota.ErrUnknownCommand
}

func (*overMQTT) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	return nil, errors.New("not over MQTT")
}

func (*overMQTT) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, errors.New("not over MQTT")
}

// Why (#662): the wiring is where these protections silently disappear: the tests above build the Settings
// service, the policy poller and the console by hand, so a line missing in setupSettings or setupDeviceCmd
// passes them all. The shared read must end with TasmoFleet's stop signal, both MAC checks must go the way the
// reads go (the router: MQTT for a device whose HTTP API is off; asked over HTTP they refused every read of
// such a device, and vouched for the address while the values came from the topic), and a console write must
// make the service forget.
func TestRuntimeWiresTheLastKnownValues(t *testing.T) {
	svc := importServices(t)
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
	desc, err := svc.Prober.Describe(t.Context(), model.Device{Address: host, Port: port})
	if err != nil || desc.MAC == "" {
		t.Fatalf("describe: %v (MAC %q)", err, desc.MAC)
	}
	id, err := svc.Store.CreateDevice(t.Context(), desc)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := svc.Store.GetDevice(t.Context(), id)
	ctl, _ := simulator.Control(sim)
	ctl.AddFault(simulator.Fault{Kind: simulator.FaultHTTPAPIOff}) // from here on only MQTT reads the device
	mqtt := &overMQTT{mac: desc.MAC, waiting: make(chan struct{})}
	svc.Client, svc.Prober.Reads = mqtt, mqtt // production gives both the router; here its MQTT route
	bg, stop := context.WithCancel(context.Background())
	a := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log, bgContext: bg, fw: newFirmwareState(nil)}
	t.Cleanup(func() { stop(); a.bgWG.Wait() })
	a.setupDeviceCmd()
	if err := a.setupSettings(); err != nil {
		t.Fatal(err)
	}
	a.policyLive.setRules([]policy.Rule{{ID: "r1", Command: "PowerOnState", Want: "3"}})

	if _, err := a.settings.Read(t.Context(), id); err != nil {
		t.Fatalf("the stored device, read over MQTT with its HTTP API off: %v", err)
	}
	if _, ok := a.settings.Last(id); !ok {
		t.Fatal("its values are not remembered")
	}
	if !a.policyLive.readDevice(t.Context(), d) || len(a.policyLive.vals.get(id)) == 0 {
		t.Fatal("the policy poller refused the stored device, read over MQTT")
	}
	if _, err := a.deviceCmd.Run(t.Context(), model.Principal{Role: model.RoleAdmin}, id, "FriendlyName1 Lamp"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.settings.Last(id); ok {
		t.Error("a console write left the remembered values in place")
	}

	mqtt.set("02:00:00:00:00:99", false) // another device listens on the topic
	if _, err := a.settings.Read(t.Context(), id); !errors.Is(err, errAnotherDevice) {
		t.Fatalf("Read of another device = %v, want errAnotherDevice", err)
	}
	if _, ok := a.settings.Last(id); ok {
		t.Error("the values of another device are remembered")
	}
	if a.policyLive.readDevice(t.Context(), d) {
		t.Error("the policy poller took the answers of another device")
	}

	mqtt.set(desc.MAC, true) // the device stops answering: the read waits for its context
	done := make(chan error, 1)
	go func() { _, err := a.settings.Read(context.Background(), id); done <- err }()
	select {
	case <-mqtt.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not start")
	}
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the read after the stop signal = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read outlived TasmoFleet's stop signal")
	}
}
