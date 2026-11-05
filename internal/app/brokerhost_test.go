package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/web"
)

// brokerRig is a reader with a fake clock whose reads are recorded.
type brokerRig struct {
	r      *brokerHostReader
	clock  time.Time
	reads  []model.DeviceID
	online map[model.DeviceID]bool
	busy   map[model.DeviceID]bool
	fail   map[model.DeviceID]bool
}

func newBrokerRig(ids ...model.DeviceID) *brokerRig {
	g := &brokerRig{clock: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), online: map[model.DeviceID]bool{}, busy: map[model.DeviceID]bool{}, fail: map[model.DeviceID]bool{}}
	var devs []model.Device
	for _, id := range ids {
		devs = append(devs, model.Device{ID: id})
		g.online[id] = true
	}
	g.r = newBrokerHostReader(
		func(context.Context) ([]model.Device, error) { return devs, nil },
		func(id model.DeviceID) bool { return g.online[id] },
		func(_ context.Context, id model.DeviceID) error {
			g.reads = append(g.reads, id)
			if g.fail[id] {
				return errors.New("timeout")
			}
			return nil
		},
		func(_ context.Context, id model.DeviceID, _ string) (func(), error) {
			if g.busy[id] {
				return nil, devlock.BusyError{Holder: "restore"}
			}
			return func() {}, nil
		}, slog.New(slog.DiscardHandler))
	g.r.now = func() time.Time { return g.clock }
	return g
}

func (g *brokerRig) advance(d time.Duration) { g.clock = g.clock.Add(d) }

func (g *brokerRig) take() []model.DeviceID {
	out := g.reads
	g.reads = nil
	return out
}

// Why: a restart is the moment a changed broker becomes visible (Tasmota restarts after MqttHost,
// MqttPort and a config restore), so the restart the tracker records must lead to a read of that
// device, but only after it settled and not for the other devices.
func TestRestartRecordedByTheTrackerLeadsToABrokerRead(t *testing.T) {
	g := newBrokerRig(1, 2)
	g.online[1] = false                  // not reachable: never read here
	g.r.next[2] = g.clock.Add(time.Hour) // its periodic read is not due: only the restart may cause one
	stab := bootWatch{StabilityStore: failingBootStore{}, onBoot: g.r.want}
	if err := stab.AppendBoot(t.Context(), model.DeviceBoot{DeviceID: 2}); err != nil {
		t.Fatal(err)
	}
	g.r.pass(t.Context())
	if len(g.take()) != 0 {
		t.Fatal("the device was read before it had time to finish restarting")
	}
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if got := g.take(); !slices.Equal(got, []model.DeviceID{2}) {
		t.Fatalf("read %v after a restart of device 2, want [2]", got)
	}
	g.advance(time.Minute)
	g.r.pass(t.Context())
	if len(g.take()) != 0 {
		t.Fatal("one restart must cause one read")
	}
}

type failingBootStore struct {
	store.StabilityStore
	err error
}

func (f failingBootStore) AppendBoot(context.Context, model.DeviceBoot) error { return f.err }

// Why: a restart that could not be stored was not recorded either, so it must not trigger anything.
func TestBootWatchIgnoresAFailedStore(t *testing.T) {
	called := false
	w := bootWatch{StabilityStore: failingBootStore{err: errors.New("disk full")}, onBoot: func(model.DeviceID) { called = true }}
	if err := w.AppendBoot(t.Context(), model.DeviceBoot{DeviceID: 1}); err == nil || called {
		t.Fatalf("err = %v, called = %v", err, called)
	}
}

// Why: the periodic read must stay negligible: an online device is read about every six hours (four
// small reads a day), an offline one never, and a whole fleet is spread over passes (one device each),
// not read in one burst. The six hours are spelled out: a shorter brokerHostEvery must fail here.
func TestPeriodicBrokerReadIsLowRate(t *testing.T) {
	g := newBrokerRig(1, 2, 3)
	g.online[3] = false
	for i := 0; i < 2; i++ {
		g.r.pass(t.Context())
		if n := len(g.take()); n != 1 {
			t.Fatalf("pass %d read %d devices, want one", i, n)
		}
	}
	g.r.pass(t.Context())
	g.advance(6*time.Hour - time.Minute)
	g.r.pass(t.Context())
	if got := g.take(); len(got) != 0 {
		t.Fatalf("read %v before six hours had passed", got)
	}
	g.advance(2 * time.Minute)
	g.r.pass(t.Context())
	g.r.pass(t.Context())
	got := g.take()
	slices.Sort(got)
	if !slices.Equal(got, []model.DeviceID{1, 2}) {
		t.Fatalf("after the interval read %v, want [1 2] (device 3 is offline)", got)
	}
}

// Why: a device that does not answer must not be asked at every pass.
func TestFailedPeriodicReadWaitsForTheRetry(t *testing.T) {
	g := newBrokerRig(1)
	g.fail[1] = true
	g.r.pass(t.Context())
	g.advance(brokerHostRetry - time.Minute)
	g.r.pass(t.Context())
	if n := len(g.take()); n != 1 {
		t.Fatalf("%d reads before the retry time, want the first one only", n)
	}
	g.advance(2 * time.Minute)
	g.r.pass(t.Context())
	if n := len(g.take()); n != 1 {
		t.Fatalf("%d reads after the retry time, want 1", n)
	}
}

// Why: a restore, an update or a backup works on the device: a read must never overlap it. A busy
// device is not read at all, and the requested read follows once the device is free.
func TestBrokerReadWaitsWhileAnotherOperationHoldsTheDevice(t *testing.T) {
	g := newBrokerRig(1)
	g.r.next[1] = g.clock.Add(time.Hour) // its periodic read is not due: only the requested one may happen
	g.busy[1] = true
	g.r.want(1)
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	g.advance(brokerHostTick)
	g.r.pass(t.Context())
	if len(g.take()) != 0 {
		t.Fatal("device read while another operation held it")
	}
	g.busy[1] = false
	g.advance(brokerHostTick)
	g.r.pass(t.Context())
	if got := g.take(); !slices.Equal(got, []model.DeviceID{1}) {
		t.Fatalf("read %v once the device was free, want [1]", got)
	}
}

// Why: the same rule with the real guard: a device lock held by a restore, or an active job, keeps
// the reader away and the reader's own lock keeps writers away while it reads.
func TestBrokerReadUsesTheDeviceGuard(t *testing.T) {
	locks := devlock.New()
	jobs := fakeActiveJobs{active: map[model.DeviceID]bool{2: true}}
	enter := deviceGuard{locks: locks, jobs: jobs}.enter
	release, _, _ := locks.TryAcquire(1, "restore")
	for _, id := range []model.DeviceID{1, 2} { // 1: locked by a restore, 2: an active update job
		g := newBrokerRig(id)
		g.r.enter = enter
		g.r.want(id)
		g.advance(brokerHostSettle)
		g.r.pass(t.Context())
		if len(g.take()) != 0 {
			t.Fatalf("device %d read while it was held", id)
		}
	}
	release()
	g := newBrokerRig(1)
	g.r.enter = enter
	g.r.read = func(context.Context, model.DeviceID) error {
		if _, holder, ok := locks.TryAcquire(1, "console"); ok || holder != "broker read" {
			t.Errorf("a writer got the device during the read (ok=%v holder=%q)", ok, holder)
		}
		return nil
	}
	g.r.want(1)
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if _, _, ok := locks.TryAcquire(1, "x"); !ok {
		t.Fatal("the lock was not released after the read")
	}
}

// Why: only a command that sets the broker moves a device; reading it or setting anything else must
// not cause a read.
func TestSetsBrokerTarget(t *testing.T) {
	for cmd, want := range map[string]bool{
		"MqttHost 10.0.0.2":                 true,
		"mqttport 1884":                     true,
		"Backlog MqttHost a; MqttPort 1883": true,
		"Backlog Power 1; MqttHost core":    true,
		"MqttHost":                          false,
		"MqttPort ":                         false,
		"Status 6":                          false,
		"MqttUser bob":                      false,
		"Backlog Power 1; Topic MqttHost x": false,
		"Topic mqtthost":                    false,
	} {
		if got := setsBrokerTarget(cmd); got != want {
			t.Errorf("setsBrokerTarget(%q) = %v, want %v", cmd, got, want)
		}
	}
}

type runStub struct {
	devicecmd.Service
	err error
}

func (r runStub) Run(context.Context, model.Principal, model.DeviceID, string) (json.RawMessage, error) {
	return json.RawMessage(`{}`), r.err
}

// Why: a typed or bulk MqttHost moves the device whatever the person meant; the read follows only
// when the device took the command (a refused or failed one changed nothing).
func TestConsoleMqttHostCommandAsksForABrokerRead(t *testing.T) {
	var asked []model.DeviceID
	run := func(err error, cmd string) {
		h := holdingCmd{Service: runStub{err: err}, onBroker: func(id model.DeviceID) { asked = append(asked, id) }}
		_, _ = h.Run(t.Context(), model.Principal{}, 7, cmd)
	}
	run(nil, "Status 6")
	run(cmdpolicy.ErrForbidden, "MqttHost x")
	run(errors.New("timeout"), "MqttHost x")
	if len(asked) != 0 {
		t.Fatalf("asked %v for commands that changed nothing", asked)
	}
	run(nil, "MqttHost broker.example")
	if !slices.Equal(asked, []model.DeviceID{7}) {
		t.Fatalf("asked %v after MqttHost, want [7]", asked)
	}
}

type applyStub struct {
	web.SettingsService
	res []settings.Result
}

// Why: the settings page sets MqttHost and MqttPort too; only a field the device accepted counts.
func TestSettingsMqttHostAsksForABrokerRead(t *testing.T) {
	var asked []model.DeviceID
	for _, tc := range []struct {
		res  []settings.Result
		want int
	}{
		{[]settings.Result{{Key: "MqttHost", Command: "MqttHost x"}}, 1},
		{[]settings.Result{{Key: "MqttPort", Command: "MqttPort 1"}}, 1},
		{[]settings.Result{{Key: "MqttHost", Err: "refused"}}, 0},
		{[]settings.Result{{Key: "Hostname"}}, 0},
	} {
		asked = nil
		l := lockedSettings{SettingsService: applyStub{res: tc.res}, g: deviceGuard{locks: devlock.New(), jobs: fakeActiveJobs{}},
			onBroker: func(id model.DeviceID) { asked = append(asked, id) }}
		if _, err := l.Apply(t.Context(), "a", 3, map[string]string{"x": "y"}); err != nil {
			t.Fatal(err)
		}
		if len(asked) != tc.want {
			t.Errorf("results %+v: asked %d times, want %d", tc.res, len(asked), tc.want)
		}
	}
}

type okCommander struct{ sent []string }

func (c *okCommander) Command(_ context.Context, _ tasmota.Target, cmd string) (json.RawMessage, error) {
	c.sent = append(c.sent, cmd)
	return json.RawMessage(`{}`), nil
}

// Why: a Make compliant rule may set the broker (policy rules cover MqttHost); the applier must ask
// for the read, and for nothing else.
func TestPolicyMqttHostAsksForABrokerRead(t *testing.T) {
	for lines, want := range map[string]int{"MqttHost broker.example": 1, "SetOption3 1": 0} {
		rt := &runtime{brokerHost: newBrokerHostReader(nil, nil, nil, nil, slog.New(slog.DiscardHandler))}
		rt.fixer = &policy.Fixer{Client: &okCommander{}, Wait: func(context.Context, time.Duration) error { return nil }}
		_ = policyApplier{a: rt}.Apply(t.Context(), model.Device{ID: 4}, []string{lines})
		if got := len(rt.brokerHost.kick); got != want {
			t.Errorf("%q: %d reads asked, want %d", lines, got, want)
		}
	}
}

// Why: the point of the issue: a device moved to another broker outside TasmoFleet keeps our host
// in the database until something reads it. ReadBroker must store the new host, port and capability
// with Status 6 only (never Status 0, asserted on what the simulated device received), and leave
// everything else a probe owns alone.
func TestReadBrokerStoresTheMovedHostWithoutStatus0(t *testing.T) {
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	sim, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitAddr(t, sim.Addr())
	client := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()})
	p := &Prober{Client: client, Gap: time.Millisecond, Broker: fixedBroker{host: "broker.alias.example"}}
	desc, err := p.Describe(t.Context(), model.Device{Address: host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	id, err := st.CreateDevice(t.Context(), desc)
	if err != nil {
		t.Fatal(err)
	}
	p.Devices = st
	before, _ := st.GetDevice(t.Context(), id)
	target := tasmota.TargetFor(before)
	if _, err := client.Command(t.Context(), target, "MqttHost broker.alias.example"); err != nil {
		t.Fatal(err)
	}
	if err := p.ReadBroker(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.GetDevice(t.Context(), id); d.MQTTHost != "broker.alias.example" || d.MQTT != model.MQTTOurs {
		t.Fatalf("after the device moved to our broker: host %q, capability %q", d.MQTTHost, d.MQTT)
	}
	if _, err := client.Command(t.Context(), target, "MqttHost old-broker.example"); err != nil {
		t.Fatal(err)
	}
	if err := p.ReadBroker(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetDevice(t.Context(), id)
	if after.MQTTHost != "old-broker.example" {
		t.Fatalf("host %q after the move, want old-broker.example", after.MQTTHost)
	}
	if after.Identity != before.Identity || after.Name != before.Name || after.Net != before.Net {
		t.Fatal("reading the broker changed identity, name or network facts")
	}
	cmds := sim.Commands()
	if !slices.Contains(cmds, "Status 6") {
		t.Fatalf("the device received %v, want a Status 6", cmds)
	}
	for _, c := range cmds {
		if strings.EqualFold(strings.Join(strings.Fields(c), " "), "status 0") {
			t.Fatalf("Status 0 reached the device: %v", cmds)
		}
	}
}

// Why: the periodic read is HTTP. A device that is online over MQTT alone (HTTP blocked) would hold
// its device lock for the whole timeout at every retry, so only a device the HTTP poll reached
// lately qualifies.
func TestBrokerHTTPUpNeedsAFreshHTTPPoll(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		h    model.Health
		want bool
	}{
		"polled lately": {model.Health{State: model.HealthOnline, SeenHTTP: now.Add(-time.Minute)}, true},
		"MQTT only":     {model.Health{State: model.HealthOnline, SeenMQTT: now}, false},
		"HTTP long ago": {model.Health{State: model.HealthOnline, SeenHTTP: now.Add(-brokerHostHTTPFresh - time.Minute)}, false},
		"offline":       {model.Health{State: model.HealthOffline, SeenHTTP: now.Add(-time.Minute)}, false},
	} {
		if got := brokerHTTPUp(tc.h, now); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// Why: the reader only helps if the app really feeds it. The tracker must record restarts through
// bootWatch, and the console service must tell the reader about MqttHost; a refactor that drops
// either would leave every other test green and bring the stale broker back.
func TestAppFeedsTheBrokerReader(t *testing.T) {
	a := &runtime{brokerHost: newBrokerHostReader(nil, nil, nil, nil, slog.New(slog.DiscardHandler)), svc: &Services{Hub: events.NewHub(8)}}
	if _, ok := a.healthDeps().Stability.(bootWatch); !ok {
		t.Fatalf("health records restarts through %T, want bootWatch", a.healthDeps().Stability)
	}
	a.restartAsks = newRestartAsks(time.Now)
	a.tracker = health.New(health.Deps{}, health.Options{})
	ctx, stop := context.WithCancel(t.Context())
	a.bgContext = ctx
	a.setupDeviceCmd()
	h, ok := a.deviceCmd.(holdingCmd)
	if !ok || h.onBroker == nil {
		t.Fatalf("console service %T has no broker hook", a.deviceCmd)
	}
	stop()
	a.bgWG.Wait()
}

func (a applyStub) Apply(context.Context, string, model.DeviceID, map[string]string) ([]settings.Result, error) {
	return a.res, nil
}

// Why: the read is HTTP. A device HTTP does not reach (online over MQTT alone, or gone) would be tried
// every two minutes for half an hour after each restart, each try holding its device lock for the
// timeout and refusing the person's console, settings, restore or update start as busy. A requested
// read waits, without sending anything, until the HTTP poll reaches the device, and it never goes to
// minimal or safeboot firmware, which answer Unknown (#558).
func TestRequestedBrokerReadWaitsUntilTheReadCanReachTheDevice(t *testing.T) {
	g := newBrokerRig(1)
	g.online[1] = false
	g.r.want(1)
	for range 10 {
		g.advance(brokerHostTick)
		g.r.pass(t.Context())
	}
	if n := len(g.take()); n != 0 {
		t.Fatalf("%d reads of a device HTTP does not reach", n)
	}
	g.online[1] = true
	g.r.pass(t.Context())
	if got := g.take(); !slices.Equal(got, []model.DeviceID{1}) {
		t.Fatalf("read %v once HTTP reached the device, want [1]", got)
	}

	limited := newBrokerRig(2)
	limited.r.list = func(context.Context) ([]model.Device, error) {
		return []model.Device{{ID: 2, Identity: model.Identity{Variant: "minimal"}}}, nil
	}
	limited.r.want(2)
	limited.advance(brokerHostSettle)
	limited.r.pass(t.Context())
	if n := len(limited.take()); n != 0 {
		t.Fatalf("%d reads of a device on minimal firmware", n)
	}
}

// newBrokerReadRig stores dev at the address of a simulated device of the given profile.
func newBrokerReadRig(t *testing.T, profile simulator.Profile, dev model.Device) (*Prober, *store.SQLite, model.DeviceID, simulator.Controller) {
	t.Helper()
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	sim, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "plug", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := simulator.Control(sim)
	dev.Address, dev.Port = splitAddr(t, sim.Addr())
	st := openBrokerStore(t)
	id, err := st.CreateDevice(t.Context(), dev)
	if err != nil {
		t.Fatal(err)
	}
	p := &Prober{Client: tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()}), Devices: st, Gap: time.Millisecond,
		Broker: fixedBroker{host: "mqtt.home"}}
	return p, st, id, ctl
}

func openBrokerStore(t *testing.T) *store.SQLite {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	return st
}

// Why: a device on minimal or safeboot firmware right now (an update outside a job: its stored identity
// still says regular firmware) answers Status 6 with Unknown. That says nothing about its broker, so the
// stored one must stay: storing "none" would end "ours", and with it the transport's MQTT route, for a
// device that is back on our broker a minute later.
func TestReadBrokerKeepsTheBrokerWhenTheDeviceAnswersLikeMinimal(t *testing.T) {
	p, st, id, _ := newBrokerReadRig(t, simulator.ProfileESP8266Min, model.Device{Name: "plug", MQTT: model.MQTTOurs, MQTTHost: "mqtt.home", MQTTPort: 1883,
		Identity: model.Identity{Version: "15.1.0", Variant: "tasmota", Family: "esp8266"}})
	if err := p.ReadBroker(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.GetDevice(t.Context(), id); d.MQTT != model.MQTTOurs || d.MQTTHost != "mqtt.home" || d.MQTTPort != 1883 {
		t.Fatalf("an Unknown answer changed the broker: %q %q:%d", d.MQTT, d.MQTTHost, d.MQTTPort)
	}
}

// Why: a read that fails half way proves nothing. Before #598 only a probe a person started wrote these
// facts; the re-read runs unattended several times a day, so a guess here (a lost SetOption3 counted as
// "on") would turn a device with MQTT switched off into "ours" and route its commands over MQTT for
// hours, and a lost Status 6 must not store anything at all.
func TestReadBrokerStoresNothingFromAFailedRead(t *testing.T) {
	for _, lost := range []string{"Status 6", "SetOption3"} {
		t.Run(lost, func(t *testing.T) {
			// What a probe stores for a device configured for our broker with MQTT switched off.
			p, st, id, ctl := newBrokerReadRig(t, simulator.ProfileESP8266_1M, model.Device{Name: "plug", MQTT: model.MQTTNone, MQTTHost: "mqtt.home", MQTTPort: 1883,
				Identity: model.Identity{Version: "15.1.0", Variant: "tasmota", Family: "esp8266"}})
			before, _ := st.GetDevice(t.Context(), id)
			for _, c := range []string{"MqttHost mqtt.home", "MqttPort 1883", "SetOption3 0"} {
				if _, err := p.Client.Command(t.Context(), tasmota.TargetFor(before), c); err != nil {
					t.Fatal(err)
				}
			}
			ctl.AddFault(simulator.Fault{After: lost, Kind: "drop"})
			if err := p.ReadBroker(t.Context(), id); err == nil {
				t.Fatal("a lost answer was taken as a read")
			}
			if d, _ := st.GetDevice(t.Context(), id); d.MQTT != model.MQTTNone || d.MQTTHost != "mqtt.home" || d.MQTTPort != 1883 {
				t.Fatalf("a failed read stored %q %q:%d", d.MQTT, d.MQTTHost, d.MQTTPort)
			}
		})
	}
}

// Why: the read must stay off a device the way a scheduled backup does: while another operation holds
// its lock, and while a firmware update typed in the console is still flashing it (it passes through
// the minimal image); and it must go only to a device the HTTP health poll reached. A wiring that
// drops either would leave every other reader test green.
func TestBrokerReaderIsGuardedLikeABackup(t *testing.T) {
	a := &runtime{brokerHost: newBrokerHostReader(nil, nil, nil, nil, slog.New(slog.DiscardHandler)),
		svc: &Services{Store: openBrokerStore(t)}, fwHolds: devlock.NewFirmwareHolds(time.Now, time.Minute),
		tracker: health.New(health.Deps{}, health.Options{})}
	ctx, stop := context.WithCancel(t.Context())
	a.startBrokerHost(ctx)
	defer func() { stop(); a.bgWG.Wait() }()
	if a.brokerHost.httpUp(3) {
		t.Error("a device the HTTP poll never reached counts as reachable")
	}
	release, _, _ := a.deviceLocks().TryAcquire(1, "restore")
	defer release()
	a.fwHolds.Start(2, 0)
	for _, id := range []model.DeviceID{1, 2} {
		if _, err := a.brokerHost.enter(ctx, id, "broker read"); !errors.Is(err, devlock.ErrBusy) {
			t.Errorf("device %d: err = %v, want busy", id, err)
		}
	}
	rel, err := a.brokerHost.enter(ctx, 3, "broker read")
	if err != nil {
		t.Fatalf("a free device was refused: %v", err)
	}
	rel()
}

// Why: the probe keeps its own reading (#598 made only the re-read strict): it has proved regular firmware
// first, and a host the device is configured for must not be hidden because SetOption3 did not answer.
func TestProbeCountsAnUnansweredSetOption3AsOn(t *testing.T) {
	p, st, id, ctl := newBrokerReadRig(t, simulator.ProfileESP8266_1M, model.Device{Name: "plug"})
	d, _ := st.GetDevice(t.Context(), id)
	if _, err := p.Client.Command(t.Context(), tasmota.TargetFor(d), "MqttHost mqtt.home"); err != nil {
		t.Fatal(err)
	}
	ctl.AddFault(simulator.Fault{After: "SetOption3", Kind: "drop"})
	got, err := p.Describe(t.Context(), model.Device{Address: d.Address, Port: d.Port})
	if err != nil {
		t.Fatal(err)
	}
	if got.MQTT != model.MQTTOurs || got.MQTTHost != "mqtt.home" {
		t.Fatalf("probe with SetOption3 lost: %q %q, want ours mqtt.home", got.MQTT, got.MQTTHost)
	}
}
