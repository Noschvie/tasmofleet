package transport

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

const simMQTTPassword = "sim-mqtt-pw-1"

func loopback() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")} }

type simRig struct {
	*rig
	ctl simulator.Controller
	dev simulator.Device
}

// newSimRig is a simulated device behind a real broker, a real bus and the real command channel,
// reached by the real HTTP client.
func newSimRig(t *testing.T, faults ...simulator.Fault) *simRig {
	t.Helper()
	return newSimRigWith(t, nil, faults...)
}

// newSimRigWith is newSimRig for a device that starts with the given settings (lower-case keys).
func newSimRigWith(t *testing.T, settings map[string]string, faults ...simulator.Fault) *simRig {
	t.Helper()
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() }) // test teardown
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	fleet, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", MQTTURL: br.URL(), TelePeriod: time.Hour}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() }) // test teardown
	dev, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "sim-plug", Profile: simulator.ProfileESP32, Version: "15.1.0", MQTTPassword: simMQTTPassword, Faults: faults, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := simulator.Control(dev)
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: br.URL(), ClientID: "router-test"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !bus.Connected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("bus never connected")
		}
	}
	ch := mqttcmd.New(bus, mqttcmd.Options{Gap: 5 * time.Millisecond})
	t.Cleanup(ch.Close)
	host, port := splitAddr(t, dev.Addr())
	mem := NewMemory(DefaultFailFor, time.Now)
	devs := &fakeDevices{d: model.Device{ID: 7, Address: host, Port: port, Topic: dev.Topic(), MQTT: model.MQTTOurs}}
	f := &facts{mem: mem, groupKnown: true}
	cl := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopback()})
	r := NewRouter(Options{HTTP: cl, Devices: devs, Facts: f, Memory: mem})
	r.SetMQTT(ch)
	// wait until the channel has seen the device come online: a command before is refused as offline
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := ch.Do(ctx, devs.d, "Status 5"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("device never answered over MQTT")
		}
	}
	return &simRig{rig: &rig{r: r, devs: devs, mem: mem, f: f}, ctl: ctl, dev: dev}
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	i := strings.LastIndex(addr, ":")
	var p int
	for _, c := range addr[i+1:] {
		p = p*10 + int(c-'0')
	}
	return addr[:i], p
}

func TestSimDeviceHTTPCannotReachIsManagedOverMQTT(t *testing.T) {
	// Why (#45): a device whose HTTP port refuses everything but which is on our broker answers
	// reads and takes writes, each sent exactly once.
	g := newSimRig(t, simulator.Fault{Kind: simulator.FaultHTTPUnreachable})
	ctx, tr := WithTrace(t.Context())
	raw, err := g.r.Command(ctx, g.target(), "Status 2")
	if err != nil || !strings.Contains(string(raw), "StatusFWR") || tr.Via() != "mqtt" {
		t.Fatalf("read: %s, %v, via %s", raw, err, tr.Via())
	}
	before := len(g.dev.Commands())
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 ON"); err != nil || !g.ctl.Power() {
		t.Fatalf("write: %v, power %v", err, g.ctl.Power())
	}
	if n := len(g.dev.Commands()) - before; n != 1 {
		t.Errorf("Power1 ON reached the device %d times, want once: %v", n, g.dev.Commands())
	}
}

func TestSimStatus0CannotBePublishedByAnExecutor(t *testing.T) {
	// Why (safety rule 2): Json {"Status":0}, Publish, Rule, If and WebSend each run a Status 0 on the
	// device; over MQTT the console must not be a way around the channel's refusal.
	g := newSimRig(t, simulator.Fault{Kind: simulator.FaultHTTPUnreachable})
	g.mem.Fail(7, HTTPFailUnreachable)
	before := len(g.dev.Commands())
	for _, c := range []string{`Json {"Status":0}`, "Publish cmnd/" + g.dev.Topic() + "/Status 0", `Rule1 ON System#Boot DO Status 0 ENDON`,
		"WebSend [127.0.0.1] Status 0", "Backlog Power ON; Json {\"Status\":0}", "Status 0", "Backlog Status 0", "If (1==1) Status 0"} {
		_, err := g.r.Command(WithConsole(t.Context()), g.target(), c)
		if !errors.Is(err, mqttcmd.ErrRefused) {
			t.Errorf("%q: err = %v, want a refusal", c, err)
		}
	}
	if got := g.dev.Commands(); len(got) != before {
		t.Fatalf("the device received %v", got[before:])
	}
}

// Why (#1001, reported from a device reached over MQTT only): a Status 0 typed in the console never crosses the
// broker (safety rule 2), and on such a device HTTP only times out. With the real channel and a simulated device,
// TypedRoute names no transport for it while HTTP is not known to work, whether its HTTP port is closed or its state
// is merely unknown, and the send announced so reaches the device over neither transport. With a fresh HTTP answer on
// record the admin's Status 0 still reaches the device, once, over HTTP.
func TestSimStatus0GoesNowhereOnAnMQTTOnlyDevice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		faults []simulator.Fault
		setup  func(g *simRig)
		want   Path
	}{
		{"HTTP port closed, state unknown", []simulator.Fault{{Kind: simulator.FaultHTTPUnreachable}}, func(*simRig) {}, None},
		{"HTTP port closed, failing", []simulator.Fault{{Kind: simulator.FaultHTTPUnreachable}}, func(g *simRig) { g.mem.Fail(7, HTTPFailUnreachable) }, None},
		{"HTTP state unknown", nil, func(*simRig) {}, None},
		{"HTTP answered lately", nil, func(g *simRig) { g.f.httpOK = true }, HTTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newSimRig(t, tc.faults...)
			tc.setup(g)
			if got := g.r.TypedRoute(t.Context(), 7, "Status 0"); got != tc.want {
				t.Fatalf("TypedRoute = %s, want %s", got, tc.want)
			}
			before := len(g.dev.Commands())
			ctx, tr := WithTrace(WithRoute(WithConsole(t.Context()), tc.want))
			raw, err := g.r.Command(ctx, g.target(), "Status 0")
			got := g.dev.Commands()[before:]
			if tc.want == None {
				if err == nil || len(got) != 0 || tr.Via() != "none" {
					t.Fatalf("err=%v via=%s, the device received %v: want nothing sent", err, tr.Via(), got)
				}
				return
			}
			if err != nil || len(got) != 1 || tr.Via() != "http" || !strings.Contains(string(raw), "StatusFWR") {
				t.Fatalf("err=%v via=%s, the device received %v, reply %.80s: want Status 0 once over HTTP", err, tr.Via(), got, raw)
			}
		})
	}
}

func TestSimBacklogAndSaveDataOverMQTT(t *testing.T) {
	// Why (#45): Make compliant sends "Backlog <setting>" and then SaveData; both are writes and
	// both have to reach a device HTTP cannot reach.
	g := newSimRig(t, simulator.Fault{Kind: simulator.FaultHTTPUnreachable})
	g.mem.Fail(7, HTTPFailUnreachable)
	for _, c := range []string{"Backlog PowerOnState 2", "SaveData 1", "PowerOnState"} {
		raw, err := g.r.Command(t.Context(), g.target(), c)
		if err != nil {
			t.Fatalf("%q: %v", c, err)
		}
		t.Logf("%q -> %s", c, raw)
	}
}

// Why (#257 acceptance, backups from both paths must be interchangeable): the dump over MQTT is
// byte for byte what /dl serves. The device here has a working HTTP port, so the same bytes are read
// both ways (HTTP pinned, then MQTT pinned).
func TestSimBackupOverMQTTEqualsDl(t *testing.T) {
	g := newSimRig(t)
	g.devs.d.MQTTPasswordMode, g.devs.d.MQTTPassword = model.MQTTPasswordOwn, simMQTTPassword
	viaHTTP, err := g.r.DownloadConfig(WithPath(t.Context(), HTTP), g.target())
	if err != nil || len(viaHTTP) == 0 {
		t.Fatalf("/dl: %d bytes, %v", len(viaHTTP), err)
	}
	ctx, tr := WithTrace(WithPath(t.Context(), MQTT))
	viaMQTT, err := g.r.DownloadConfig(ctx, g.target())
	if err != nil || string(viaMQTT) != string(viaHTTP) || tr.Via() != "mqtt" {
		t.Fatalf("MQTT: %d bytes vs %d, %v, via %s", len(viaMQTT), len(viaHTTP), err, tr.Via())
	}
}

// Why (D6): a wrong password is marked bad, so nothing more is published until it is changed.
func TestSimWrongPasswordMarksItBad(t *testing.T) {
	g := newSimRig(t, simulator.Fault{Kind: simulator.FaultHTTPUnreachable})
	g.mem.Fail(7, HTTPFailUnreachable)
	g.devs.d.MQTTPasswordMode, g.devs.d.MQTTPassword = model.MQTTPasswordOwn, "not-the-one"
	mk := &fakeMarker{}
	g.r.o.Marker = mk
	_, err := g.r.DownloadConfig(t.Context(), g.target())
	if !errors.Is(err, mqttcmd.ErrPasswordRefused) || len(mk.ids) != 1 || mk.ids[0] != 7 {
		t.Fatalf("%v, marked %v", err, mk.ids)
	}
	if strings.Contains(err.Error(), "not-the-one") {
		t.Fatal("the error carries the password")
	}
}

// Why (#632, #605): a write goes over MQTT when HTTP does not reach the device, and a published write
// is never sent again over HTTP. A device with SetOption4 1 confirms it on stat/<t>/<COMMAND>; missed,
// the write that worked would end as an unknown outcome after the full reply wait.
func TestSimSetOption4DeviceTakesAWriteOverMQTT(t *testing.T) {
	g := newSimRigWith(t, map[string]string{"setoption4": "1"}, simulator.Fault{Kind: simulator.FaultHTTPUnreachable})
	before := len(g.dev.Commands())
	ctx, tr := WithTrace(t.Context())
	start := time.Now()
	raw, err := g.r.Command(ctx, g.target(), "TelePeriod 60")
	if err != nil || !strings.Contains(string(raw), `"TelePeriod":60`) || tr.Via() != "mqtt" {
		t.Fatalf("write: %s, %v, via %s", raw, err, tr.Via())
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the write took %v, it waited for the reply timeout", d)
	}
	if n := len(g.dev.Commands()) - before; n != 1 {
		t.Errorf("TelePeriod 60 reached the device %d times, want once: %v", n, g.dev.Commands())
	}
}

// Why (#632): a device with SetOption4 1 answers on stat/<t>/<COMMAND>. Before the fix such a read
// waited the full 8 s on MQTT, was repeated over HTTP and the device was read over HTTP for ten
// minutes. The read must be answered over MQTT, once, with no HTTP request behind it.
func TestSimSetOption4DeviceIsReadOverMQTTWithoutTimeoutOrHTTPFallback(t *testing.T) {
	g := newSimRigWith(t, map[string]string{"setoption4": "1"})
	before := len(g.dev.Commands())
	ctx, tr := WithTrace(t.Context())
	start := time.Now()
	raw, err := g.r.Command(ctx, g.target(), "TelePeriod")
	if err != nil || !strings.Contains(string(raw), "TelePeriod") || tr.Via() != "mqtt" {
		t.Fatalf("read: %s, %v, via %s", raw, err, tr.Via())
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the read took %v, it waited for the reply timeout", d)
	}
	if n := len(g.dev.Commands()) - before; n != 1 {
		t.Errorf("the device received %d commands, want the one MQTT read: %v", n, g.dev.Commands())
	}
}

func TestSimBareSwitchModeIsReadOverMQTTUnderItsIndex(t *testing.T) {
	// Why (#711): a bare SwitchMode<n> only reports, so it is routed as a read: over MQTT first, and over HTTP a
	// second time when MQTT gets no answer. That is only right if the command channel matches Tasmota's reply,
	// which carries the index ({"SwitchMode1":0}, ResponseCmndIdxNumber); otherwise every read would time out,
	// mark the device and be sent twice. Each switch must get its own value, never another switch's reply.
	g := newSimRigWith(t, map[string]string{"switchmode1": "2", "switchmode2": "1", "switchmode28": "3"})
	for cmd, want := range map[string]string{
		"SwitchMode1":  `{"SwitchMode1":2}`,
		"SwitchMode2":  `{"SwitchMode2":1}`,
		"SwitchMode28": `{"SwitchMode28":3}`, // the highest index routed as a read (MAX_SWITCHES_SET of 12.5.0)
		"SwitchMode":   `{"SwitchMode1":2}`,  // Tasmota reads the bare name as index 1
	} {
		ctx, tr := WithTrace(t.Context())
		raw, err := g.r.Command(ctx, g.target(), cmd)
		if err != nil || string(raw) != want || tr.Via() != "mqtt" {
			t.Errorf("%s: %s, %v, via %s; want %s over MQTT only", cmd, raw, err, tr.Via(), want)
		}
	}
}

// Why (#605): with HTTP working too, the settings page's writes, Make compliant's SaveData, a switch and Restart
// go over MQTT first to a real command channel and a simulated device: each is answered over MQTT (no wait for the
// reply timeout, which would leave the change an unknown outcome), reaches the device exactly once, and the trace
// (the audit's "via") says MQTT.
func TestSimWritesGoOverMQTTFirstWhileHTTPWorks(t *testing.T) {
	g := newSimRig(t)
	for _, c := range []string{"TelePeriod 60", "PowerOnState 1", "SetOption19 1", "Timezone 99", "TimeDst 0,0,3,1,2,120",
		"SaveData 0", "Power1 ON", "Backlog PowerOnState 2; SaveData 1", "Restart 1"} {
		before := len(g.dev.Commands())
		ctx, tr := WithTrace(t.Context())
		start := time.Now()
		raw, err := g.r.Command(ctx, g.target(), c)
		if err != nil || tr.Via() != "mqtt" {
			t.Fatalf("%q: %s, %v, via %s", c, raw, err, tr.Via())
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%q took %v: it waited for the reply timeout", c, d)
		}
		if n := len(g.dev.Commands()) - before; n != 1 {
			t.Errorf("%q reached the device %d times, want once: %v", c, n, g.dev.Commands()[before:])
		}
	}
}

// Why (#605, the product owner's transport rule of 2026-11-14): with HTTP working too, a change typed in the console
// goes over MQTT first whatever it carries (a password, the broker, the topic), and HTTP carries only what the
// command channel provably cannot: commands that run other commands (Json, WebSend, If, a rule text: the channel
// refuses them so that none can run a Status 0 on TasmoFleet's behalf, claim T1) and Backlog2/Backlog3, which publish
// no results to match. The real channel decides here, against a simulated device; the trace names the transport
// that carried each command. Each command gets its own device: MqttPassword, MqttHost and Topic queue the device's
// restart (restart_flag 2, about 2 s, 40 ms at the rig's time scale), and a later command on the same device found it
// rebooting whenever the commands before took longer than that (#1177).
func TestSimOnlyWhatMQTTCannotCarryGoesOverHTTPWhileHTTPWorks(t *testing.T) {
	for _, tc := range []struct {
		cmd, via, why string
	}{
		{"MqttPassword x", "mqtt", "a secret is no reason for HTTP"},
		{"MqttHost broker2", "mqtt", "a change of the MQTT connection while HTTP works: HTTP stays for the next command"},
		{"Topic newtopic", "mqtt", "the same"},
		{"TelePeriod 60", "mqtt", "an ordinary write"},
		{`Json {"Power":1}`, "http", "an executor: the channel refuses it (T1)"},
		{"WebSend [127.0.0.1] Power1 ON", "http", "an executor: the channel refuses it (T1)"},
		{"If (1==1) Power1 ON", "http", "an executor: the channel refuses it (T1)"},
		{"Rule1 ON Power1#State DO Power2 1 ENDON", "http", "a rule text: the channel refuses it (T1)"},
		{"Backlog2 Power1 ON", "http", "publishes no results, so nothing could be matched"},
		{`Template {"NAME":"x","GPIO":[0],"FLAG":0,"BASE":18,"CMND":"Power1 ON"}`, "http", "a Template with CMND runs other commands: the channel refuses it (T1)"},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			g := newSimRig(t)
			g.f.httpOK = true // HTTP answered lately
			before := len(g.dev.Commands())
			ctx, tr := WithTrace(WithConsole(t.Context()))
			_, err := g.r.Command(ctx, g.target(), tc.cmd)
			if tc.via == "mqtt" && err != nil {
				t.Errorf("%q: %v", tc.cmd, err)
			}
			if got := tr.Via(); got != tc.via {
				t.Errorf("%q went over %s, want %s (%s)", tc.cmd, got, tc.via, tc.why)
			}
			if n := len(g.dev.Commands()) - before; n < 1 {
				t.Errorf("%q never reached the device", tc.cmd)
			}
		})
	}
}
