package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/settings"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// Keeping the stored identity fresh after a firmware change (#1003): a restart read that met minimal firmware does not
// use up the hour, a new version seen in INFO1 or a backup and a console write of an identity value each trigger an
// identity read, and nothing of those messages or answers is stored itself.

// kicks hands the reader what its loop (run) would take from its queues.
func (g *refreshRig) kicks() {
	for {
		select {
		case id := <-g.r.kick:
			g.r.want(id)
		case id := <-g.r.kickRestart:
			g.r.wantRestart(id)
		case k := <-g.r.kickIdentity:
			g.r.wantIdentity(k)
		default:
			return
		}
	}
}

func info1Of(version string) []byte {
	return fmt.Appendf(nil, `{"Info1":{"Module":"Sonoff Basic","Version":%q}}`, version)
}

// Why (#1003): on an ESP8266 a firmware update goes through the minimal image, which answers no Status 5, so the
// restart read after that first boot cannot tell it is the stored device and reads nothing (settings.ErrNoMAC). If that
// read counted as the restart's read, the boot into the new firmware a minute later would be braked to the end of the
// hour and the page would show the old version and OTA URL for that hour.
func TestARestartReadThatMetMinimalFirmwareDoesNotUseUpTheHour(t *testing.T) {
	g := newRefreshRig(1)
	far := g.clock.Add(48 * time.Hour)
	g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = far, far, far
	minimal := true
	read := g.r.read
	g.r.read = func(ctx context.Context, id model.DeviceID) error {
		if minimal {
			g.reads = append(g.reads, id)
			return settings.ErrNoMAC // minimal firmware: no MAC to check
		}
		return read(ctx, id)
	}
	g.r.wantRestart(1) // the boot into minimal
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if b, i, d := g.takeAll(); len(b) != 1 || len(i)+len(d) != 0 {
		t.Fatalf("on minimal firmware: broker %v, identity %v, data %v, want the broker check only", b, i, d)
	}
	minimal = false
	g.advance(time.Minute)
	g.r.wantRestart(1) // the boot into the new firmware
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if _, i, d := g.takeAll(); len(i) != 1 || len(d) != 1 {
		t.Fatalf("the boot into the new firmware a minute later read the identity %d and the data %d times, want 1 and 1: the read on minimal firmware read nothing, so it must not count", len(i), len(d))
	}
}

// Why (#1003, the reported case end to end): a two-step update outside TasmoFleet (console OtaUrl and Upgrade 1) restarts
// the device into minimal firmware and then into the new version. The page must show the new version and OTA URL within
// a minute of the second boot, with nobody pressing Refresh identity. The device is the simulator; the reader runs on a
// fake clock with the real broker read and the real background identity refresh (Prober.RefreshFacts).
func TestATwoStepUpdateShowsTheNewVersionAndOTAURLWithoutAClick(t *testing.T) {
	const fixtures = "../../testdata/fixtures"
	const newVersion = "16.0.0.5"
	images := map[string][]byte{}
	for _, name := range []string{"tasmota-minimal.bin.gz", "tasmota.bin.gz"} {
		img, err := simulator.NewImage(fixtures, name, newVersion)
		if err != nil {
			t.Fatal(err)
		}
		images["/"+name] = img
	}
	fw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		img, ok := images[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(img) // a loopback test server; a failed write fails the download, and the test with it
	}))
	t.Cleanup(fw.Close)
	fleet, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: fixtures}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	sim, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M})
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := simulator.Control(sim)
	host, port := splitAddr(t, sim.Addr())
	cl := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()})
	st := openBrokerStore(t)
	p := &Prober{Client: cl, Gap: time.Millisecond, Broker: fixedBroker{host: "broker.example"}, Devices: st}
	desc, err := p.Describe(t.Context(), model.Device{Address: host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.CreateDevice(t.Context(), desc)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetDevice(t.Context(), id)
	if before.Identity.Version == newVersion || before.OTAURL == "" {
		t.Fatalf("the device starts at %q with OTA URL %q: the test proves nothing", before.Identity.Version, before.OTAURL)
	}

	clock := time.Date(2026, 11, 19, 8, 40, 0, 0, time.UTC)
	r := newBrokerHostReader(st.ListDevices, func(model.DeviceID) bool { return true }, p.ReadBroker,
		func(context.Context, model.DeviceID, string) (func(), error) { return func() {}, nil }, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return clock }
	r.readIdentity = p.RefreshFacts
	far := clock.Add(48 * time.Hour)
	r.next[id], r.dataNext[id], r.identityNext[id] = far, far, far // only the restarts read
	t.Cleanup(func() {                                             // what the device was asked, for a failure
		if t.Failed() {
			t.Logf("commands: %q", sim.Commands())
		}
	})
	wait := func(what string, cond func(simulator.Controller) bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		if err := ctl.WaitFor(ctx, cond); err != nil {
			t.Fatalf("the device never got to %s: %v", what, err)
		}
	}
	send := func(cmd string) {
		t.Helper()
		if _, err := cl.Command(t.Context(), tasmota.TargetFor(before), cmd); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
	restartRead := func() {
		r.wantRestart(id) // what the health tracker asks for at every boot (bootWatch)
		clock = clock.Add(brokerHostSettle)
		r.pass(t.Context())
	}

	// Step 1: the person flashes the minimal image from the console; the device boots into it and the restart read
	// meets it.
	send("OtaUrl " + fw.URL + "/tasmota-minimal.bin.gz")
	send("Upgrade 1")
	wait("minimal firmware", func(c simulator.Controller) bool { return c.Minimal() && c.Phase() == simulator.PhaseRun })
	restartRead()
	if d, _ := st.GetDevice(t.Context(), id); d.Identity.Version != before.Identity.Version {
		t.Fatalf("the read on minimal firmware stored version %q", d.Identity.Version)
	}

	// Step 2: the minimal image flashes the new version; the device boots into it a minute later.
	send("OtaUrl " + fw.URL + "/tasmota.bin.gz")
	send("Upgrade 1")
	wait("the new firmware", func(c simulator.Controller) bool {
		return !c.Minimal() && c.Phase() == simulator.PhaseRun && c.Version() == newVersion
	})
	clock = clock.Add(time.Minute)
	restartRead() // within a minute of the boot

	d, _ := st.GetDevice(t.Context(), id)
	if d.Identity.Version != newVersion {
		t.Fatalf("20 seconds after the boot into %s the stored version is %q: the read on minimal firmware used up the hour", newVersion, d.Identity.Version)
	}
	if want := ctl.PersistedOtaURL(); d.OTAURL != want || d.OTAURL == before.OTAURL {
		t.Fatalf("stored OTA URL %q, want the device's %q (before the update %q)", d.OTAURL, want, before.OTAURL)
	}
}

// Why (#1003): a live INFO1 is the first thing a device says after a boot into new firmware. A version other than the
// stored one reads the identity at once, inside the hour a restart read brakes, because otherwise the page shows the old
// version until that hour is over. A boot loop keeps its version, so once the new version is stored, further boots stay
// braked: the identity is read about once an hour, not after every boot.
func TestANewVersionInINFO1ReadsTheIdentityAtOnceAndABootLoopOnItStaysBraked(t *testing.T) {
	g := newRefreshRig(1)
	far := g.clock.Add(48 * time.Hour)
	g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = far, far, far
	running := "15.1.0"
	stored := model.Device{ID: 1, Topic: "plug", MQTT: model.MQTTOurs, Identity: model.Identity{Version: running, DetectedAt: g.clock}}
	var ident, data int
	g.r.readIdentity = func(context.Context, model.DeviceID) error { ident++; stored.Identity.Version = running; return nil }
	g.r.readData = func(context.Context, model.Device) error { data++; return nil }
	m := newMQTTIdentity(nil, nil, func(context.Context) ([]model.Device, error) { return []model.Device{stored}, nil }, nil, nil, nil, slog.New(slog.DiscardHandler))
	m.onVersion = g.r.identityChanged
	boot := func() {
		g.r.restarted(1) // the tracker's restart
		m.handle(t.Context(), mqttbus.Message{Kind: mqttbus.KindInfo, Topic: "plug", Suffix: "INFO1", Payload: info1Of(running + "(release-tasmota)")})
		g.kicks()
	}
	boot()
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if ident != 1 || data != 1 {
		t.Fatalf("the first restart read the identity %d and the data %d times, want 1 and 1", ident, data)
	}
	g.advance(2 * time.Minute)
	running = "16.0.0.5"
	boot()
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if ident != 2 || data != 1 {
		t.Fatalf("the boot into %s two minutes later read the identity %d and the data %d times in all, want 2 and 1 (the identity at once, the data braked)", running, ident, data)
	}
	if stored.Identity.Version != running {
		t.Fatalf("stored %q", stored.Identity.Version)
	}
	for range 25 { // the new firmware boot-loops: a restart every seven minutes for three hours
		boot()
		for range 14 {
			g.advance(30 * time.Second)
			g.r.pass(t.Context())
		}
	}
	if ident > 5 || data > 4 {
		t.Fatalf("three hours of boots on the same version read the identity %d and the data %d times, want at most 5 and 4 (the two above, then about once an hour)", ident, data)
	}
}

// Why (#1003): right after a boot the device may not answer yet, so the read a new version asked for can fail. It is
// retried after the usual delay, and a further restart (the device is still settling) must not push that retry to the end
// of the hour: that would bring back the stale page this fix is about. The version is asked about once, so a device whose
// reads keep failing in a boot loop is still read about once an hour, not after every boot.
func TestAFailedReadOfANewVersionIsRetriedSoonAndThenBraked(t *testing.T) {
	g := newRefreshRig(1)
	far := g.clock.Add(48 * time.Hour)
	g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = far, far, far
	running := "15.1.0"
	stored := model.Device{ID: 1, Topic: "plug", MQTT: model.MQTTOurs, Identity: model.Identity{Version: running, DetectedAt: g.clock}}
	fail := false
	var reads []time.Time
	g.r.readIdentity = func(context.Context, model.DeviceID) error {
		reads = append(reads, g.clock)
		if fail {
			return tasmota.ErrTimeout
		}
		stored.Identity.Version = running
		return nil
	}
	g.r.readData = func(context.Context, model.Device) error { return nil }
	m := newMQTTIdentity(nil, nil, func(context.Context) ([]model.Device, error) { return []model.Device{stored}, nil }, nil, nil, nil, slog.New(slog.DiscardHandler))
	m.onVersion = g.r.identityChanged
	boot := func() {
		g.r.restarted(1)
		m.handle(t.Context(), mqttbus.Message{Kind: mqttbus.KindInfo, Topic: "plug", Suffix: "INFO1", Payload: info1Of("16.0.0.5(release-tasmota)")})
		g.kicks()
	}
	tick := func(d time.Duration) {
		for end := g.clock.Add(d); g.clock.Before(end); {
			g.advance(30 * time.Second)
			g.r.pass(t.Context())
		}
	}
	g.r.wantRestart(1) // a restart read a little earlier: the hour's brake is on
	tick(time.Minute)
	reads = nil
	t0 := g.clock
	running, fail = "16.0.0.5", true // the new firmware boots and does not answer yet
	boot()
	tick(5 * time.Minute)
	if len(reads) != 1 {
		t.Fatalf("the boot into a new version read the identity %d times in five minutes, want 1", len(reads))
	}
	boot() // still settling: another restart, the same version
	fail = false
	tick(10 * time.Minute)
	if stored.Identity.Version != "16.0.0.5" {
		t.Fatalf("15 minutes after the boot into a new version the stored version is %q; reads at %v (boot at %v)", stored.Identity.Version, reads, t0)
	}

	// The same device, its reads failing for good while it boot-loops on the new version.
	g2 := newRefreshRig(1)
	g2.r.next[1], g2.r.dataNext[1], g2.r.identityNext[1] = far, far, far
	stored2 := stored
	stored2.Identity.Version = "15.1.0"
	ident := 0
	g2.r.readIdentity = func(context.Context, model.DeviceID) error { ident++; return tasmota.ErrTimeout }
	g2.r.readData = func(context.Context, model.Device) error { return tasmota.ErrTimeout }
	m2 := newMQTTIdentity(nil, nil, func(context.Context) ([]model.Device, error) { return []model.Device{stored2}, nil }, nil, nil, nil, slog.New(slog.DiscardHandler))
	m2.onVersion = g2.r.identityChanged
	for range 26 { // a restart every seven minutes for three hours
		g2.r.restarted(1)
		m2.handle(t.Context(), mqttbus.Message{Kind: mqttbus.KindInfo, Topic: "plug", Suffix: "INFO1", Payload: info1Of("16.0.0.5(release-tasmota)")})
		g2.kicks()
		for range 14 {
			g2.advance(30 * time.Second)
			g2.r.pass(t.Context())
		}
	}
	if ident > 6 {
		t.Fatalf("three hours of boots on a new version whose reads fail read the identity %d times, want at most 6 (once for the version, its retry, then about once an hour)", ident)
	}
}

// Why (#1003, T20): an INFO1 carries no MAC, and whoever can publish on the device's topic can send one, and a version
// change reads the identity outside the hour a restart read brakes. Without a limit of its own, an INFO1 repeated with a
// version the device does not run, two versions in turn, or each after a forged restart would read the identity
// (Status 5, 2, 4 and the facts, the device lock held for up to 30 s, the person's writes refused as busy) after every
// message. A version reads the identity of a device at most once per brokerHostRetry, ten minutes: about six times an
// hour, whatever comes in.
func TestForgedINFO1ReadTheIdentityAtMostOncePerTenMinutes(t *testing.T) {
	for _, c := range []struct {
		name     string
		versions []string
		fail     bool // the identity read gets no answer
		restart  bool // a forged restart (INFO3) before each INFO1
	}{
		{"one version repeated, reads succeed", []string{"16.0.0.5"}, false, false},
		{"two versions in turn, reads fail", []string{"16.0.0.5", "16.0.0.6"}, true, false},
		{"two versions in turn after a restart each", []string{"16.0.0.5", "16.0.0.6"}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRefreshRig(1)
			far := g.clock.Add(48 * time.Hour)
			g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = far, far, far
			g.r.wantRestart(1) // a restart read a little earlier: the hour's brake is on
			g.advance(brokerHostSettle)
			g.r.pass(t.Context())
			stored := model.Device{ID: 1, Topic: "plug", MQTT: model.MQTTOurs, Identity: model.Identity{Version: "15.1.0", DetectedAt: g.clock}}
			ident := 0
			g.r.readIdentity = func(context.Context, model.DeviceID) error { // the device still runs 15.1.0
				ident++
				if c.fail {
					return tasmota.ErrTimeout
				}
				return nil
			}
			m := newMQTTIdentity(nil, nil, func(context.Context) ([]model.Device, error) { return []model.Device{stored}, nil }, nil, nil, nil, slog.New(slog.DiscardHandler))
			m.onVersion = g.r.identityChanged
			for i := range 60 { // one forged INFO1 a minute for an hour
				if c.restart {
					g.r.restarted(1)
				}
				m.handle(t.Context(), mqttbus.Message{Kind: mqttbus.KindInfo, Topic: "plug", Suffix: "INFO1", Payload: info1Of(c.versions[i%len(c.versions)] + "(release-tasmota)")})
				g.kicks()
				for range 2 {
					g.advance(30 * time.Second)
					g.r.pass(t.Context())
				}
			}
			if ident > 7 {
				t.Fatalf("60 forged INFO1 in an hour read the identity %d times, want at most 7 (one per ten minutes, and one more for the end of the hour)", ident)
			}
		})
	}
}

// Why (#1003): the ask of a version waits for its ten minutes; it must not be dropped (a rollback within ten minutes is
// still read) and a stream of further INFO1 must not push it later. A read that moved with every message would never
// come, and the device's periodic reads, which skip a device with a request waiting, would not come either.
func TestAStreamOfINFO1CannotPushTheIdentityReadAway(t *testing.T) {
	g := newRefreshRig(1)
	far := g.clock.Add(48 * time.Hour)
	g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = far, far, far
	stored := model.Device{ID: 1, Topic: "plug", MQTT: model.MQTTOurs, Identity: model.Identity{Version: "15.1.0", DetectedAt: g.clock}}
	var reads []time.Time
	g.r.readIdentity = func(context.Context, model.DeviceID) error { reads = append(reads, g.clock); return nil } // still 15.1.0
	m := newMQTTIdentity(nil, nil, func(context.Context) ([]model.Device, error) { return []model.Device{stored}, nil }, nil, nil, nil, slog.New(slog.DiscardHandler))
	m.onVersion = g.r.identityChanged
	start := g.clock
	for i := range 6 * 30 { // an INFO1 every ten seconds for half an hour, two versions in turn; the reader looks every brokerHostTick
		m.handle(t.Context(), mqttbus.Message{Kind: mqttbus.KindInfo, Topic: "plug", Suffix: "INFO1", Payload: info1Of(fmt.Sprintf("16.0.0.%d(release-tasmota)", 5+i%2))})
		g.kicks()
		g.advance(10 * time.Second)
		if i%3 == 2 {
			g.r.pass(t.Context())
		}
	}
	if len(reads) == 0 {
		t.Fatal("an INFO1 every ten seconds for half an hour read the identity never: every message pushed the read away")
	}
	if first := reads[0].Sub(start); first > brokerHostSettle+brokerHostTick {
		t.Fatalf("the first read came %v after the first INFO1, want within %v", first, brokerHostSettle+brokerHostTick)
	}
	for i := 1; i < len(reads); i++ {
		if gap := reads[i].Sub(reads[i-1]); gap < brokerHostRetry || gap > brokerHostRetry+brokerHostTick {
			t.Fatalf("reads at %v: a gap of %v, want between %v and %v", reads, gap, brokerHostRetry, brokerHostRetry+brokerHostTick)
		}
	}
}

// Why (#1003): a real update can follow a version read within ten minutes, a rollback for one. Its ask waits for the
// ten minutes and is read then, also when it came with a restart whose own read the hour brakes: dropped, it would leave
// the page on the version the device just left until the daily refresh, the stale page this fix is about.
func TestAVersionAskInsideTheTenMinutesIsReadWhenTheyAreOver(t *testing.T) {
	g := newRefreshRig(1)
	far := g.clock.Add(48 * time.Hour)
	g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = far, far, far
	running := "15.1.0"
	stored := model.Device{ID: 1, Topic: "plug", MQTT: model.MQTTOurs, Identity: model.Identity{Version: running, DetectedAt: g.clock}}
	var reads []time.Time
	g.r.readIdentity = func(context.Context, model.DeviceID) error {
		reads = append(reads, g.clock)
		stored.Identity.Version = running
		return nil
	}
	m := newMQTTIdentity(nil, nil, func(context.Context) ([]model.Device, error) { return []model.Device{stored}, nil }, nil, nil, nil, slog.New(slog.DiscardHandler))
	m.onVersion = g.r.identityChanged
	boot := func() {
		g.r.restarted(1)
		m.handle(t.Context(), mqttbus.Message{Kind: mqttbus.KindInfo, Topic: "plug", Suffix: "INFO1", Payload: info1Of(running + "(release-tasmota)")})
		g.kicks()
	}
	tick := func(d time.Duration) {
		for end := g.clock.Add(d); g.clock.Before(end); {
			g.advance(brokerHostTick)
			g.r.pass(t.Context())
		}
	}
	g.r.wantRestart(1) // a restart read a little earlier: the hour's brake is on
	tick(time.Minute)
	reads = nil
	running = "16.0.0.5"
	boot()
	tick(time.Minute)
	if len(reads) != 1 || stored.Identity.Version != running {
		t.Fatalf("the boot into %s read the identity %d times and stored %q", running, len(reads), stored.Identity.Version)
	}
	running = "15.1.0" // rolled back two minutes later
	tick(time.Minute)
	boot()
	tick(brokerHostRetry)
	if len(reads) != 2 || stored.Identity.Version != running {
		t.Fatalf("ten minutes after the boot back into %s the identity was read %d times in all and %q is stored, want 2 and %q", running, len(reads), stored.Identity.Version, running)
	}
	if gap := reads[1].Sub(reads[0]); gap < brokerHostRetry || gap > brokerHostRetry+brokerHostTick {
		t.Fatalf("the rollback was read %v after the update, want between %v and %v", gap, brokerHostRetry, brokerHostRetry+brokerHostTick)
	}
}

// Why (#1003, T20): an INFO1 is published by whoever can write the device's topic; it carries no MAC. It only asks for the
// identity read, and only for a version other than the stored one on regular firmware: a minimal or safeboot INFO1 (the
// first step of an update), a retained one (no reading of now) and one with the stored version ask for nothing.
func TestOnlyALiveINFO1WithAnotherRegularVersionAsksForTheIdentity(t *testing.T) {
	stored := model.Device{ID: 1, Topic: "plug", MQTT: model.MQTTOurs, Identity: model.Identity{Version: "15.1.0", DetectedAt: time.Unix(1700000000, 0)}}
	var asked []string
	m := newMQTTIdentity(nil, nil, func(context.Context) ([]model.Device, error) { return []model.Device{stored}, nil }, nil, nil, nil, slog.New(slog.DiscardHandler))
	m.onVersion = func(id model.DeviceID, v string) { asked = append(asked, fmt.Sprint(id, " ", v)) }
	for _, c := range []struct {
		version  string
		retained bool
		topic    string
	}{
		{"15.1.0(release-tasmota)", false, "plug"},
		{"16.0.0.5(release-minimal)", false, "plug"},
		{"16.0.0.5(release-tasmota)", true, "plug"},
		{"16.0.0.5(release-tasmota)", false, "other"},
		{"not a version", false, "plug"},
	} {
		m.handle(t.Context(), mqttbus.Message{Kind: mqttbus.KindInfo, Topic: c.topic, Suffix: "INFO1", Payload: info1Of(c.version), Retained: c.retained})
	}
	if len(asked) != 0 {
		t.Fatalf("asked %v", asked)
	}
	m.handle(t.Context(), mqttbus.Message{Kind: mqttbus.KindInfo, Topic: "plug", Suffix: "INFO1", Payload: info1Of("16.0.0.5(release-tasmota)")})
	if len(asked) != 1 || asked[0] != "1 16.0.0.5" {
		t.Fatalf("a live INFO1 with a new version asked %v, want [1 16.0.0.5]", asked)
	}
}

// Why (#1003, decision 4): a console write the device accepted for a value TasmoFleet keeps (OtaUrl, a group topic, the
// DeviceName ...) makes the stored value stale. TasmoFleet reads the identity again, with its usual checks (T20), instead
// of taking the value from the answer: the answer is not checked against the MAC. A bulk run of several such writes is one
// read, a failed or refused command and a query ask for nothing, and an identity trigger reads no broker.
func TestAnAcceptedConsoleWriteOfAnIdentityValueTriggersOneIdentityRead(t *testing.T) {
	g := newRefreshRig(1)
	far := g.clock.Add(48 * time.Hour)
	g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = far, far, far
	run := func(err error, cmd string) {
		h := holdingCmd{Service: answerStub{answer: `{"OtaUrl":"http://other.example/x.bin.gz"}`, err: err}, onIdentity: g.r.identityWritten}
		_, _ = h.Run(t.Context(), model.Principal{}, 1, cmd) // the stub's answer and error; the test looks at the reads
	}
	run(nil, "OtaUrl")     // a query
	run(nil, "Status 2")   // a query
	run(nil, "Power 1")    // no identity value
	run(nil, "GroupTopic") // a query
	run(cmdpolicy.ErrForbidden, "OtaUrl http://x.example/a.bin.gz")
	run(tasmota.ErrTimeout, "OtaUrl http://x.example/a.bin.gz") // not accepted (outcome unknown): no answer to read after
	g.kicks()
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if b, i, d := g.takeAll(); len(b)+len(i)+len(d) != 0 {
		t.Fatalf("queries and failed writes caused broker %v, identity %v, data %v reads", b, i, d)
	}
	for _, cmd := range []string{"OtaUrl http://x.example/a.bin.gz", "GroupTopic2 kitchen", "Backlog Power 1; DeviceName Lamp"} {
		run(nil, cmd)
	}
	g.kicks()
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if b, i, d := g.takeAll(); len(i) != 1 || len(b)+len(d) != 0 {
		t.Fatalf("three accepted writes of identity values caused broker %v, identity %v, data %v reads, want one identity read", b, i, d)
	}
}

type answerStub struct {
	runStub
	answer string
	err    error
}

func (a answerStub) Run(context.Context, model.Principal, model.DeviceID, string) (json.RawMessage, error) {
	return json.RawMessage(a.answer), a.err
}

// Why (#1003): a backup reads Status 2, so it can be the first to see that a device runs another version (a firmware
// update done outside TasmoFleet). Its values have no MAC check, so none of them is stored as the identity; another
// version only triggers the identity read, once per version, and a backup of the stored version triggers nothing.
func TestABackupWithAnotherVersionTriggersAnIdentityReadAndStoresNothing(t *testing.T) {
	g := newRefreshRig(1)
	far := g.clock.Add(48 * time.Hour)
	g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = far, far, far
	stored := model.Device{ID: 1, Topic: "plug", OTAURL: "http://ota.example/old.bin.gz",
		Identity: model.Identity{Version: "15.1.0", Variant: "tasmota", DetectedAt: time.Unix(1700000000, 0)}}
	gets := 0
	get := func(_ context.Context, id model.DeviceID) (model.Device, error) {
		gets++
		if id != stored.ID {
			return model.Device{}, errors.New("not found")
		}
		return stored, nil
	}
	hub := events.NewHub(16)
	ch, unsub := hub.Subscribe(events.TopicBackup)
	done := make(chan struct{})
	go func() {
		watchBackupVersions(t.Context(), ch, get, g.r.identityChanged, slog.New(slog.DiscardHandler))
		close(done)
	}()
	for _, b := range []model.Backup{
		{DeviceID: 1, Version: "15.1.0", Kind: model.BackupConfigDump}, // the stored version
		{DeviceID: 0, Version: "16.0.0.5"},                             // a backup of a deleted device
		{DeviceID: 1, Version: "16.0.0.5", Kind: model.BackupConfigDump},
		{DeviceID: 1, Version: "16.0.0.5", Kind: model.BackupDecoded}, // the same run's other file
	} {
		hub.Publish(events.Event{Topic: events.TopicBackup, Payload: b})
	}
	unsub() // the watcher reads what is queued and ends
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher did not end")
	}
	if gets == 0 {
		t.Fatal("the watcher never looked at the stored device")
	}
	g.kicks()
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if b, i, d := g.takeAll(); len(i) != 1 || len(b)+len(d) != 0 {
		t.Fatalf("a backup of another version caused broker %v, identity %v, data %v reads, want one identity read", b, i, d)
	}
	if stored.Identity.Version != "15.1.0" || stored.OTAURL != "http://ota.example/old.bin.gz" {
		t.Fatalf("the backup's values were stored: %+v", stored)
	}
}

// Why (#1003): the read an OtaUrl typed in the console asks for can meet the minimal image of the update the person starts
// right after it (Upgrade 1). Minimal firmware shows no MAC, which is not another device: the device page must not say
// that another device answers. And since nothing was read, the boot into the new firmware is read in full.
func TestAnIdentityReadThatMeetsMinimalFirmwareIsNotReportedAsAnotherDevice(t *testing.T) {
	g := newRefreshRig(1)
	far := g.clock.Add(48 * time.Hour)
	g.r.next[1], g.r.dataNext[1], g.r.identityNext[1] = far, far, far
	identity := g.r.readIdentity
	g.r.readIdentity = func(context.Context, model.DeviceID) error {
		return &RefreshRefusedError{Reason: refusedUnverified}
	}
	g.r.identityWritten(1)
	g.kicks()
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if reason, _, _, ok := g.outcome(1); !ok || reason != settings.FailNoMAC {
		t.Fatalf("the device page is told %q (recorded %v), want %q", reason, ok, settings.FailNoMAC)
	}
	g.r.readIdentity = identity
	g.advance(time.Minute)
	g.r.wantRestart(1)
	g.advance(brokerHostSettle)
	g.r.pass(t.Context())
	if _, i, d := g.takeAll(); len(i) != 1 || len(d) != 1 {
		t.Fatalf("the boot into the new firmware read the identity %d and the data %d times, want 1 and 1", len(i), len(d))
	}
}
