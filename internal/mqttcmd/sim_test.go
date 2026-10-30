package mqttcmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// simEnv is a simulated device behind a real embedded broker and a real mqttbus.
type simEnv struct {
	dev  simulator.Device
	ctl  simulator.Controller
	bus  mqttbus.Bus
	ch   *Channel
	dvc  model.Device
	bURL string
}

func newSimEnv(t *testing.T, spec simulator.DeviceSpec) *simEnv {
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
	spec.Name, spec.Version = "sim-plug", "15.1.0"
	if spec.Profile == "" {
		spec.Profile = simulator.ProfileESP32
	}
	dev, err := fleet.Add(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := simulator.Control(dev)
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: br.URL(), ClientID: "mqttcmd-test"})
	if err != nil {
		t.Fatal(err)
	}
	e := &simEnv{dev: dev, ctl: ctl, bus: bus, bURL: br.URL(), dvc: model.Device{ID: 1, Topic: dev.Topic(), MQTT: model.MQTTOurs}}
	e.ch = New(bus, Options{Gap: 5 * time.Millisecond})
	return e
}

func (e *simEnv) waitConnected(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !e.bus.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("bus never connected")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Why (#272): the first command sent the moment the bus says "connected" must be answered. Before
// Connected() meant "subscribed" and the channel registered its listener before it published, a
// fast device answered into the void and the caller waited out its timeout.
func TestFirstCommandAfterConnectIsAnswered(t *testing.T) {
	e := newSimEnv(t, simulator.DeviceSpec{})
	for i := range 15 {
		ctx, cancel := context.WithCancel(t.Context())
		bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: brokerOf(t, e), ClientID: "mqttcmd-fresh-" + string(rune('a'+i))})
		if err != nil {
			t.Fatal(err)
		}
		ch := New(bus, Options{Gap: time.Millisecond, ReplyWait: 3 * time.Second})
		for !bus.Connected() {
			time.Sleep(time.Millisecond)
		}
		raw, err := ch.Do(ctx, e.dvc, "Status 5")
		cancel()
		if err != nil || !strings.Contains(strings.ToLower(string(raw)), e.ctl.MAC()) {
			t.Fatalf("round %d: %s, %v", i, raw, err)
		}
	}
}

func brokerOf(t *testing.T, e *simEnv) string {
	t.Helper()
	return e.bURL
}

// Why (#45): a device whose HTTP port refuses everything but which is on our broker is managed
// over MQTT: reads, a write that takes effect, and a Backlog with one reply per command.
func TestCommandsReachADeviceHTTPCannotReach(t *testing.T) {
	e := newSimEnv(t, simulator.DeviceSpec{Faults: []simulator.Fault{{Kind: simulator.FaultHTTPUnreachable}}})
	e.waitConnected(t)
	ctx := t.Context()
	raw, err := e.ch.Do(ctx, e.dvc, "Status 2")
	if err != nil || !strings.Contains(string(raw), "StatusFWR") {
		t.Fatalf("Status 2: %s, %v", raw, err)
	}
	if raw, err = e.ch.Do(ctx, e.dvc, "Power1 ON"); err != nil || !strings.Contains(string(raw), `"ON"`) || !e.ctl.Power() {
		t.Fatalf("Power1 ON: %s, %v, power %v", raw, err, e.ctl.Power())
	}
	raw, err = e.ch.Do(ctx, e.dvc, "Backlog Power1 OFF; Status 1")
	if err != nil || !strings.Contains(string(raw), "StatusPRM") || !strings.Contains(string(raw), "POWER") || e.ctl.Power() {
		t.Fatalf("Backlog: %s, %v, power %v", raw, err, e.ctl.Power())
	}
}

// Why (safety rule 2): Status 0 never reaches the device, which the simulator's own command log
// proves (the bus-side fake in channel_test.go only shows what we handed to the bus).
func TestStatus0NeverReachesTheSimulatedDevice(t *testing.T) {
	e := newSimEnv(t, simulator.DeviceSpec{})
	e.waitConnected(t)
	for _, c := range []string{"Status 0", "status0", "Backlog Power ON; Status 0"} {
		if _, err := e.ch.Do(t.Context(), e.dvc, c); !errors.Is(err, ErrRefused) {
			t.Errorf("%q: %v", c, err)
		}
	}
	if cmds := e.dev.Commands(); len(cmds) != 0 {
		t.Fatalf("device received %q", cmds)
	}
}

// Why: a restart publishes LWT Offline (the simulator now does too), so the INFO1 of the old boot
// must go and the new boot's INFO1 must replace it; a Do while the device is down is refused
// without publishing, and the command that caused the restart reports its reply.
func TestRestartDropsInfoAndTheNewBootIsSeen(t *testing.T) {
	e := newSimEnv(t, simulator.DeviceSpec{})
	e.waitConnected(t)
	if raw, err := e.ch.Do(t.Context(), e.dvc, "Restart 1"); err != nil || !strings.Contains(string(raw), "Restarting") {
		t.Fatalf("Restart: %s, %v", raw, err)
	}
	waitFor(t, "the INFO1 of the new boot", func() bool {
		i, ok := e.ch.Info(e.dvc.Topic)
		return ok && strings.Contains(string(i.Info1), "15.1.0")
	})
	waitFor(t, "the device to answer again", func() bool {
		_, err := e.ch.Do(t.Context(), e.dvc, "Status 2")
		return err == nil
	})
	if _, err := e.ch.Do(t.Context(), e.dvc, "Hostname"); errors.Is(err, tasmota.ErrUnreachable) {
		t.Fatalf("online device refused: %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
