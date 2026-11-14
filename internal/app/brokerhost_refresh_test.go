package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/devlock"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/settings"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/templates"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// #676 part 1: a device whose HTTP API is off but that MQTT serves never got its data refreshed in the
// background, because the reader visits only devices the HTTP health poll reached lately (readable) and an
// API refusal never moves that fact. These tests drive the reader's own pass() against a simulated device
// with the API switched off, on a real broker with the real bus, command channel, router and transport facts.

// httpCounter counts what is asked over HTTP; the simulator records only what reaches the device.
type httpCounter struct {
	tasmota.Client
	all, status5 atomic.Int32
}

func (c *httpCounter) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	c.all.Add(1)
	if strings.EqualFold(strings.TrimSpace(cmnd), "Status 5") {
		c.status5.Add(1)
	}
	return c.Client.Command(ctx, t, cmnd)
}

type apiOffOpts struct {
	mac       string // stored MAC; "" = the device's own
	noMAC     bool   // stored without a MAC
	mqttOff   bool   // MQTT is off: the app has no transport facts
	ours      bool   // the device uses TasmoFleet's broker
	lwtOnline bool
	variant   string
}

type apiOffRig struct {
	st    *gtStore
	sim   simulator.Controller
	http  *httpCounter
	pr    *Prober
	set   *settings.Manager
	rep   *policyReporter
	tpl   *templates.Manager
	r     *brokerHostReader
	held  atomic.Bool // a job holds the device
	clock time.Time
}

func newAPIOffRig(t *testing.T, o apiOffOpts) *apiOffRig {
	t.Helper()
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() }) // test teardown
	fleet, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", MQTTURL: br.URL(), TelePeriod: time.Hour}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() }) // test teardown
	dev, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "api-off", Profile: simulator.ProfileESP32, Version: "15.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	bus, err := mqttbus.Connect(t.Context(), mqttbus.Options{BrokerURL: br.URL(), ClientID: "api-off-test"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !bus.Connected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("bus never connected")
		}
	}
	ctl, _ := simulator.Control(dev)
	readyCtx, readyCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer readyCancel()
	if err := ctl.WaitFor(readyCtx, func(c simulator.Controller) bool { return c.MQTTReady() }); err != nil {
		t.Fatalf("simulated device never subscribed to its command topic: %v", err)
	}
	ch := mqttcmd.New(bus, mqttcmd.Options{Gap: 5 * time.Millisecond})
	t.Cleanup(ch.Close)

	// The HTTP API is off: /cm drops the connection (SetOption128 0), nothing is executed or recorded.
	ctl.AddFault(simulator.Fault{Kind: simulator.FaultHTTPAPIOff})
	host, port := splitAddr(t, ctl.Addr())
	mac := o.mac
	if mac == "" && !o.noMAC {
		mac = ctl.MAC()
	}
	capability := model.MQTTUnknown
	if o.ours {
		capability = model.MQTTOurs
	}
	st := &gtStore{now: time.Now, devs: []model.Device{
		{ID: 1, Name: "api-off", Topic: dev.Topic(), Address: host, Port: port, MQTT: capability, MAC: mac, Identity: model.Identity{Variant: o.variant}},
	}}
	g := &apiOffRig{st: st, sim: ctl, http: &httpCounter{Client: tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets(), Timeout: 2 * time.Second})}}
	router := transport.NewRouter(transport.Options{HTTP: g.http, Devices: st})
	lwt := "Offline"
	if o.lwtOnline {
		lwt = "Online"
	}
	facts := &TransportFacts{Devices: st, Health: fakeFactsHealth{1: {LWT: lwt}}, Memory: router.Memory(), Unanswered: router.UnansweredReads(),
		BusConnected: bus.Connected, FullTopic: func() string { return "%prefix%/%topic%/" }, FirmwareBaseURL: func() string { return "" },
		BrokerPassword: func() string { return "" }, Now: time.Now}
	router.SetFacts(facts)
	router.SetMQTT(ch)
	g.pr = &Prober{Client: router.HTTPOnly(), Reads: router, Gap: time.Millisecond, Devices: nil}

	g.set = settings.New(settings.Options{Devices: st, Client: router, ReadTimeout: 30 * time.Second, Verify: g.pr.VerifyRead})
	g.rep = &policyReporter{devices: fakeLister(st.devs), reader: g.set, verify: g.pr.VerifyRead, now: time.Now}
	g.rep.setRules([]policy.Rule{{ID: "r1", Command: "PowerOnState", Want: "3"}})
	g.tpl = templates.New(templates.Options{Devices: st, Client: router, Backups: noBackups{}})
	a := &runtime{svc: &Services{Prober: g.pr}, settings: g.set, policyLive: g.rep, templates: g.tpl, facts: facts}
	if o.mqttOff {
		a.facts = nil
	}

	g.clock = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	g.r = newBrokerHostReader(st.list,
		func(model.DeviceID) bool { return false }, // the HTTP health poll has not reached the device: its API is off
		g.pr.ReadBroker,
		func(_ context.Context, _ model.DeviceID, _ string) (func(), error) {
			if g.held.Load() {
				return nil, devlock.BusyError{Holder: "restore"}
			}
			return func() {}, nil
		}, slog.New(slog.DiscardHandler))
	g.r.now = func() time.Time { return g.clock }
	a.wireDeviceRefresh(g.r) // as the app wires it (startBrokerHost)
	return g
}

// sent returns what reached the device, which is only MQTT while its HTTP API is off.
func (g *apiOffRig) sent() []string { return g.sim.Commands() }

func countCmd(cmds []string, want string) int {
	n := 0
	for _, c := range cmds {
		if strings.EqualFold(strings.Join(strings.Fields(c), " "), want) {
			n++
		}
	}
	return n
}

func (g *apiOffRig) liveTemplate(t *testing.T) bool {
	t.Helper()
	return liveTemplate(t, g.tpl)
}

// liveTemplate says whether tpl holds a live template reading of any device.
func liveTemplate(t *testing.T, tpl *templates.Manager) bool {
	t.Helper()
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
	return live
}

// Why (#676): a device whose HTTP API is off but that MQTT serves showed stale or no last-known values for as
// long as the API stayed off: the background refresh never visited it. The pass must refresh the three caches the
// pages read from (the Settings tab's last-known values, the policy values, the GPIO template reading) over MQTT,
// and touch nothing that is HTTP: no broker read (Status 6, HTTP by design), no identity probe (Status 2, 4), no
// request over HTTP at all, and never Status 0.
func TestPassRefreshesTheDataOfADeviceWhoseHTTPAPIIsOff(t *testing.T) {
	g := newAPIOffRig(t, apiOffOpts{ours: true, lwtOnline: true})
	g.r.pass(t.Context())

	if snap, ok := g.set.Last(1); !ok || len(snap.Values) == 0 {
		t.Errorf("the Settings tab has no last-known values after the pass: %v (sent over MQTT: %v)", snap, g.sent())
	}
	if v := g.rep.vals.get(1)["PowerOnState"]; v.value == "" {
		t.Errorf("the policy values were not refreshed: %v", g.rep.vals.get(1))
	}
	if !g.liveTemplate(t) {
		t.Error("the GPIO template reading was not cached")
	}
	cmds := g.sent()
	for _, c := range []string{"Status 0", "Status 6", "Status 2", "Status 4"} {
		if n := countCmd(cmds, c); n != 0 {
			t.Errorf("%d x %q reached the device: the visit is data only (the broker and the identity are HTTP reads)", n, c)
		}
	}
	if n := g.http.all.Load(); n != 0 {
		t.Errorf("%d request(s) over HTTP to a device whose HTTP API is off", n)
	}
}

// Why (#676): the visit asked Status 5 up to three times for the same question, "is this the stored device":
// before the reads, after the Settings read, after the policy read. Over MQTT the answer cannot change inside
// one visit (the device is addressed by its topic, not by an address a DHCP lease can hand to another device), so
// one is enough. Counted at the device, with all three data parts active. A second visit, a day later, asks again.
func TestAVisitAsksStatus5OncePerTransportOverMQTT(t *testing.T) {
	g := newAPIOffRig(t, apiOffOpts{ours: true, lwtOnline: true})
	g.r.pass(t.Context())
	if n := countCmd(g.sent(), "Status 5"); n != 1 {
		t.Errorf("%d x Status 5 over MQTT in one visit, want 1: %v", n, g.sent())
	}
	if n := g.http.status5.Load(); n != 0 {
		t.Errorf("%d x Status 5 over HTTP in a visit that goes over MQTT", n)
	}
	g.clock = g.clock.Add(dataEvery + time.Minute)
	g.r.pass(t.Context())
	if n := countCmd(g.sent(), "Status 5"); n != 2 {
		t.Errorf("%d x Status 5 after the second visit, want 2: a check of the last visit must not stand in for this one", n)
	}
}

// Why (#676): the sharing is the visit's own. The Settings tab and the policy check run the same check outside a
// visit (a page open) and must always ask: nothing carries over from a refresh that ran minutes ago.
func TestAPageCheckIsNeverSharedWithAnotherRead(t *testing.T) {
	g := newAPIOffRig(t, apiOffOpts{ours: true, lwtOnline: true})
	d, _ := g.st.GetDevice(t.Context(), 1)
	rctx, read := transport.WithTrace(t.Context()) // a page's read, over MQTT like every read of this device
	if _, err := g.pr.Reads.Command(rctx, tasmota.TargetFor(d), "Status 3"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := g.pr.VerifyRead(t.Context(), d, read); err != nil {
			t.Fatal(err)
		}
	}
	if n := countCmd(g.sent(), "Status 5"); n != 2 {
		t.Errorf("%d x Status 5 for two separate checks, want 2", n)
	}
}

// Why (#676, #662): fail closed. A device on the stored topic that reports another MAC (the entry belongs to a
// different device) must not fill this entry's caches, and must receive nothing but the one MAC question. The
// visit must really have happened (one Status 5), else this would pass for a reader that never looks at the device,
// and it must not be repeated at every tick (the broker's interval, like any refused data read).
func TestAPIOffRefreshOfAnotherDeviceIsRefused(t *testing.T) {
	g := newAPIOffRig(t, apiOffOpts{ours: true, lwtOnline: true, mac: "02:00:00:00:00:99"})
	g.r.pass(t.Context())
	if cmds := g.sent(); len(cmds) != 1 || !strings.EqualFold(cmds[0], "Status 5") {
		t.Fatalf("the device received %v, want only the MAC question", cmds)
	}
	if _, ok := g.set.Last(1); ok {
		t.Error("the Settings cache was filled from another device")
	}
	if len(g.rep.vals.get(1)) != 0 {
		t.Error("the policy values were filled from another device")
	}
	if g.liveTemplate(t) {
		t.Error("the template reading was filled from another device")
	}
	g.clock = g.clock.Add(brokerHostTick)
	g.r.pass(t.Context())
	if n := len(g.sent()); n != 1 {
		t.Errorf("the refused device was asked again at the next tick: %v", g.sent())
	}
}

// Why (#676): the visit goes only where the read really goes over MQTT. A device MQTT cannot serve (not on our
// broker, offline on MQTT) would get its reads over HTTP, which is off: each try would wait for a failure. A device
// stored without a MAC has no MQTT check to pin the reads to MQTT. And limited firmware (minimal, safeboot) answers
// none of the reads. Nothing is sent to any of them.
func TestNoDataOnlyVisitWhenTheReadsDoNotGoOverMQTT(t *testing.T) {
	for name, o := range map[string]apiOffOpts{
		"not on our broker":   {ours: false, lwtOnline: true},
		"offline on MQTT":     {ours: true, lwtOnline: false},
		"minimal firmware":    {ours: true, lwtOnline: true, variant: "minimal"},
		"safeboot, same rule": {ours: true, lwtOnline: true, variant: "safeboot"},
		// the refresh reads over MQTT only after the stored device showed its MAC there; without a stored MAC the
		// check sends nothing, so the reads would go over HTTP, which is off
		"no stored MAC": {ours: true, lwtOnline: true, noMAC: true},
		"MQTT off":      {ours: true, lwtOnline: true, mqttOff: true},
	} {
		t.Run(name, func(t *testing.T) {
			g := newAPIOffRig(t, o)
			g.r.pass(t.Context())
			if cmds := g.sent(); len(cmds) != 0 {
				t.Errorf("sent over MQTT: %v", cmds)
			}
			if n := g.http.all.Load(); n != 0 {
				t.Errorf("%d request(s) over HTTP", n)
			}
		})
	}
}

// Why (#676): a job or a typed firmware update holds the device: nothing is sent, and the visit does not use up
// the pass (the next device is looked at), like a busy device in any other visit. Due again at the next visit.
func TestDataOnlyVisitWaitsWhileAJobHoldsTheDevice(t *testing.T) {
	g := newAPIOffRig(t, apiOffOpts{ours: true, lwtOnline: true})
	g.held.Store(true)
	g.r.pass(t.Context())
	if cmds := g.sent(); len(cmds) != 0 {
		t.Fatalf("sent to a device a job holds: %v", cmds)
	}
	g.held.Store(false)
	g.clock = g.clock.Add(brokerHostTick)
	g.r.pass(t.Context())
	if _, ok := g.set.Last(1); !ok {
		t.Error("the device was not refreshed once the job let go")
	}
}

// Why (#676): a refreshed device is not visited again before the data interval; the visit must not turn into a
// read at every tick of a device nobody can reach over HTTP.
func TestDataOnlyVisitKeepsTheDataInterval(t *testing.T) {
	g := newAPIOffRig(t, apiOffOpts{ours: true, lwtOnline: true})
	g.r.pass(t.Context())
	n := len(g.sent())
	if n == 0 {
		t.Fatal("no visit")
	}
	for range 5 {
		g.clock = g.clock.Add(brokerHostTick)
		g.r.pass(t.Context())
	}
	if m := len(g.sent()); m != n {
		t.Errorf("%d more commands within the data interval", m-n)
	}
}

// Why (#676, #662): sharing the check inside a refresh is for MQTT only. Over HTTP the answer comes from whoever
// holds the address, and the check after the reads exists for an address that changes hands between the first
// check and the reads: the first Status 5 shows the stored device, the second another one. The refresh, with its
// checks read through the router as production wires them, must still refuse and remember nothing.
func TestSharedCheckDoesNotCoverAnAddressThatChangedHandsOverHTTP(t *testing.T) {
	o, swap := storedDevice(t)
	// routers without MQTT, as production wires them: the reads and the checks go over HTTP and the traces say so
	o.p.Reads = transport.NewRouter(transport.Options{HTTP: swap, Devices: o.st})
	set := settings.New(settings.Options{Devices: o.st, Client: transport.NewRouter(transport.Options{HTTP: o.cl, Devices: o.st}), Verify: o.p.VerifyRead})
	a := &runtime{svc: &Services{Prober: o.p}, settings: set}
	if err := a.refreshData(t.Context(), o.stored); !errors.Is(err, errAnotherDevice) {
		t.Fatalf("refreshData = %v, want errAnotherDevice", err)
	}
	if snap, ok := set.Last(o.id); ok {
		t.Fatalf("the Settings cache holds %d values of another device", len(snap.Values))
	}
}

// Why (#676): a refresh pinned to MQTT sends nothing the command channel cannot match. A policy rule may name
// State, whose reply never parses to a value (it has no "State" key: the reading is unknown over any transport);
// over MQTT it would only wait for the timeout and fail the policy part, and with it the template part, at every
// visit. Left out, the rest of the refresh goes on.
func TestAStateRuleDoesNotCostAnMQTTRefreshItsOtherParts(t *testing.T) {
	g := newAPIOffRig(t, apiOffOpts{ours: true, lwtOnline: true})
	g.rep.setRules([]policy.Rule{{ID: "r1", Command: "PowerOnState", Want: "3"}, {ID: "r2", Command: "State", Want: "x"}})
	g.r.pass(t.Context())
	if n := countCmd(g.sent(), "State"); n != 0 {
		t.Errorf("%d x State over MQTT, which never matches its reply", n)
	}
	if v := g.rep.vals.get(1)["PowerOnState"]; v.value == "" {
		t.Errorf("the policy values were not refreshed: %v", g.rep.vals.get(1))
	}
	if !g.liveTemplate(t) {
		t.Error("the GPIO template reading was not cached: the State rule cost the refresh its template part")
	}
}

// scriptedMQTT stands in for the command channel: it answers Status 5 as status5 says and every other command as
// other says (nil: a timeout, the device went quiet on MQTT), counting what it was asked.
type scriptedMQTT struct {
	status5 func(n int) (json.RawMessage, error)
	other   func(cmd string) (json.RawMessage, error)
	n5      atomic.Int32
	nOther  atomic.Int32
}

func (m *scriptedMQTT) Connected() bool { return true }
func (m *scriptedMQTT) Do(_ context.Context, _ model.Device, cmd string) (json.RawMessage, error) {
	if cmd == "Status 5" {
		return m.status5(int(m.n5.Add(1)))
	}
	m.nOther.Add(1)
	if m.other != nil {
		return m.other(cmd)
	}
	return nil, tasmota.ErrTimeout
}
func (m *scriptedMQTT) DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return m.Do(ctx, d, cmd)
}

// answers answers every command over MQTT as the stored device would (storedAnswer, anything else with an empty
// object), except fail, which gets err.
func answers(fail string, err error) func(string) (json.RawMessage, error) {
	return func(cmd string) (json.RawMessage, error) {
		switch cmd {
		case fail:
			return nil, err
		case "Module", "Status":
			return storedAnswer(cmd), nil
		}
		return json.RawMessage(`{}`), nil
	}
}

// storedAnswer is a device's answer to Module (a built-in module, a reading the template cache keeps) and to
// Status (PowerOnState, the value the policy rule reads).
func storedAnswer(cmd string) json.RawMessage {
	if cmd == "Module" {
		return json.RawMessage(`{"Module":{"1":"Sonoff Basic"}}`)
	}
	return json.RawMessage(`{"Status":{"PowerOnState":3}}`)
}

// atAddress is the device that holds the stored address over HTTP: it answers Status 5 with mac, Module and Status
// as storedAnswer and every other command with an empty object, counting what it was asked.
type atAddress struct {
	tasmota.Client
	mac       string
	n, status atomic.Int32
}

func (h *atAddress) Command(_ context.Context, _ tasmota.Target, cmnd string) (json.RawMessage, error) {
	h.n.Add(1)
	switch cmnd {
	case "Status 5":
		h.status.Add(1)
		return mac5(h.mac), nil
	case "Module", "Status":
		return storedAnswer(cmnd), nil
	}
	return json.RawMessage(`{}`), nil
}

func mac5(mac string) json.RawMessage {
	return json.RawMessage(`{"StatusNET":{"Mac":"` + mac + `"}}`)
}

const (
	storedMAC = "02:00:00:00:00:01"
	otherMAC  = "02:00:00:00:00:99"
)

type scriptRig struct {
	a    *runtime
	set  *settings.Manager
	rep  *policyReporter
	tpl  *templates.Manager
	http *atAddress
	d    model.Device
}

// refreshOverScript is a refresh of device 1 (stored MAC storedMAC) through the real router, whose MQTT is mqtt and
// whose HTTP is the device that holds the address, reporting httpMAC. All three data parts are wired: Settings,
// policy (one rule) and templates.
func refreshOverScript(t *testing.T, mqtt *scriptedMQTT, httpMAC string) *scriptRig {
	t.Helper()
	d := model.Device{ID: 1, Topic: "plug", Address: "192.0.2.1", Port: 80, MQTT: model.MQTTOurs, MAC: storedMAC}
	st := &gtStore{now: time.Now, devs: []model.Device{d}}
	g := &scriptRig{http: &atAddress{mac: httpMAC}, d: d}
	router := transport.NewRouter(transport.Options{HTTP: g.http, Devices: st})
	router.SetFacts(&TransportFacts{Devices: st, Health: fakeFactsHealth{1: {LWT: "Online"}}, Memory: router.Memory(), Unanswered: router.UnansweredReads(),
		BusConnected: func() bool { return true }, FullTopic: func() string { return "%prefix%/%topic%/" }, FirmwareBaseURL: func() string { return "" },
		BrokerPassword: func() string { return "" }, Now: time.Now})
	router.SetMQTT(mqtt)
	pr := &Prober{Client: router.HTTPOnly(), Reads: router, Gap: time.Millisecond}
	g.set = settings.New(settings.Options{Devices: st, Client: router, ReadTimeout: 5 * time.Second, Verify: pr.VerifyRead})
	g.rep = &policyReporter{devices: fakeLister(st.devs), reader: g.set, verify: pr.VerifyRead, now: time.Now}
	g.rep.setRules([]policy.Rule{{ID: "r1", Command: "PowerOnState", Want: "3"}})
	g.tpl = templates.New(templates.Options{Devices: st, Client: router, Backups: noBackups{}})
	g.a = &runtime{svc: &Services{Prober: pr}, settings: g.set, policyLive: g.rep, templates: g.tpl}
	return g
}

// Why (#676, security check and review of #801): once the stored device answered the refresh's first check over
// MQTT, the check vouches for the topic, not for the address. A read MQTT then leaves unanswered (not published,
// or published and not answered) must not be asked over HTTP, where another device may hold the address now: the
// re-asked check would go over MQTT again, pass, and the other device's values would be remembered. The refresh
// fails instead (the next visit reads again), asks nothing over HTTP and remembers nothing.
func TestAfterAnMQTTCheckNoReadOfTheRefreshFallsBackToHTTP(t *testing.T) {
	for name, err := range map[string]error{"not published": tasmota.ErrUnreachable, "not answered": tasmota.ErrTimeout} {
		t.Run(name, func(t *testing.T) {
			mqtt := &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return mac5(storedMAC), nil }, other: answers("Status 3", err)}
			g := refreshOverScript(t, mqtt, otherMAC)
			if rerr := g.a.refreshData(t.Context(), g.d); rerr == nil {
				t.Error("refreshData = nil, want the read's failure")
			}
			if snap, ok := g.set.Last(1); ok {
				t.Errorf("the Settings cache holds %d values: a read was answered by the device at the address", len(snap.Values))
			}
			if n := g.http.n.Load(); n != 0 {
				t.Errorf("%d request(s) over HTTP in a refresh whose check went over MQTT", n)
			}
		})
	}
}

// Why (#676, review of #801): the template part has no check after its reads; it relies on the refresh's first
// check. A Module read MQTT leaves unanswered must therefore not be answered over HTTP by the device that holds the
// address now: no template reading is stored, and nothing is asked over HTTP.
func TestAModuleReadMQTTLeavesUnansweredStoresNoTemplateReading(t *testing.T) {
	for name, err := range map[string]error{"not published": tasmota.ErrUnreachable, "not answered": tasmota.ErrTimeout} {
		t.Run(name, func(t *testing.T) {
			mqtt := &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return mac5(storedMAC), nil }, other: answers("Module", err)}
			g := refreshOverScript(t, mqtt, otherMAC)
			if rerr := g.a.refreshData(t.Context(), g.d); rerr == nil {
				t.Error("refreshData = nil, want the Module read's failure")
			}
			if liveTemplate(t, g.tpl) {
				t.Error("a template reading was stored from the device that holds the address")
			}
			if n := g.http.n.Load(); n != 0 {
				t.Errorf("%d request(s) over HTTP in a refresh whose check went over MQTT", n)
			}
		})
	}
}

// Why (#676, #662): a refresh whose first check did not go over MQTT vouches for the address, whether MQTT did not
// publish that check or published it and got no answer while the device at the address answered over HTTP. Its
// reads stay on HTTP, and every check after the reads is asked there again, as before #676: a lease handed to
// another device between two reads is what those checks are for. Nothing of the refresh goes over MQTT after its
// first check; were the unanswered check taken for an MQTT one, the reads would go to the topic with no check after
// them, vouched for by nothing.
func TestARefreshWhoseCheckWentOverHTTPStaysOnHTTP(t *testing.T) {
	for name, first := range map[string]error{"not published": tasmota.ErrUnreachable, "not answered": tasmota.ErrTimeout} {
		t.Run(name, func(t *testing.T) {
			mqtt := &scriptedMQTT{status5: func(n int) (json.RawMessage, error) {
				if n == 1 {
					return nil, first
				}
				return mac5(storedMAC), nil
			}, other: answers("", nil)}
			g := refreshOverScript(t, mqtt, storedMAC)
			if err := g.a.refreshData(t.Context(), g.d); err != nil {
				t.Errorf("refreshData = %v", err)
			}
			if n5, n := mqtt.n5.Load(), mqtt.nOther.Load(); n5 != 1 || n != 0 {
				t.Errorf("over MQTT after the first check: %d x Status 5 more and %d read(s), want none", n5-1, n)
			}
			// before the reads, after the Settings read, after the policy read
			if n := g.http.status.Load(); n != 3 {
				t.Errorf("%d x Status 5 over HTTP, want 3: every check of an HTTP refresh is asked", n)
			}
			if _, ok := g.set.Last(1); !ok {
				t.Error("the Settings cache was not filled over HTTP")
			}
		})
	}
}

// Why (#676, review of #801): a policy rule may name a setting the router counts as a write (PulseTime1, Mem1: not
// cmdpolicy.ReportsOnly). Unpinned, the router sends it over HTTP while HTTP works; pinned to MQTT it would need the
// strict write conditions (the GroupTopic of every device on the topic read lately), which fail after a start, an
// adoption or while any device cannot be read. Sent, it would fail the policy part and with it the template part of
// every such refresh, every 10 minutes, where main read all of it over HTTP. Under the pin it is left out (the policy
// poller keeps reading it, under the same gate), and the rest of the refresh goes on, without HTTP.
func TestAWriteClassedRuleDoesNotCostAnMQTTRefreshItsOtherParts(t *testing.T) {
	var writeClassed atomic.Int32
	other := answers("", nil)
	mqtt := &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return mac5(storedMAC), nil }, other: func(cmd string) (json.RawMessage, error) {
		if cmd == "PulseTime1" || cmd == "Mem1" {
			writeClassed.Add(1)
		}
		return other(cmd)
	}}
	g := refreshOverScript(t, mqtt, storedMAC) // stores no GroupTopic: the strict write conditions fail
	g.rep.setRules([]policy.Rule{{ID: "r1", Command: "PowerOnState", Want: "3"}, {ID: "r2", Command: "PulseTime1", Want: "0"},
		{ID: "r3", Command: "Mem1", Want: "x"}})
	if err := g.a.refreshData(t.Context(), g.d); err != nil {
		t.Errorf("refreshData = %v: a write-classed rule cost the refresh its parts", err)
	}
	if n := writeClassed.Load(); n != 0 {
		t.Errorf("%d write-classed read(s) over MQTT", n)
	}
	if v := g.rep.vals.get(1)["PowerOnState"]; v.value == "" {
		t.Errorf("the policy values were not refreshed: %v", g.rep.vals.get(1))
	}
	if !liveTemplate(t, g.tpl) {
		t.Error("the GPIO template reading was not cached")
	}
	if n := g.http.n.Load(); n != 0 {
		t.Errorf("%d request(s) over HTTP in a refresh whose check went over MQTT", n)
	}
}

// Why (#676): an answer over MQTT that cannot vouch for the device (no MAC in it, Status 5 unknown, a timeout) is
// not a check that passed: it is refused as before and nothing is remembered.
func TestAnMQTTAnswerThatDoesNotVouchIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		reply func() (json.RawMessage, error)
		want  error
	}{
		"no MAC in the answer": {func() (json.RawMessage, error) { return json.RawMessage(`{"StatusNET":{}}`), nil }, errAnotherDevice},
		"Status 5 unknown":     {func() (json.RawMessage, error) { return nil, tasmota.ErrUnknownCommand }, settings.ErrNoMAC},
		// the router then asks over HTTP, which is the other device
		"timeout": {func() (json.RawMessage, error) { return nil, tasmota.ErrTimeout }, errAnotherDevice},
	} {
		t.Run(name, func(t *testing.T) {
			mqtt := &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return tc.reply() }}
			g := refreshOverScript(t, mqtt, otherMAC)
			if err := g.a.refreshData(t.Context(), g.d); !errors.Is(err, tc.want) {
				t.Fatalf("refreshData = %v, want %v", err, tc.want)
			}
			if _, ok := g.set.Last(1); ok {
				t.Fatal("the Settings cache was filled although the identity was not vouched for")
			}
		})
	}
}
