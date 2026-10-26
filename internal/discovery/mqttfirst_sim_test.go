package discovery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

// Why: end to end over a real MQTT connection (embedded broker, simulated
// devices): a device without tasmota/discovery (SetOption19 1, or a build
// without the driver) and a normal one both end up as proposals, the first
// through exactly one Status 5 and never Status 0 (safety rule 2).
func TestSimulatedFleetWithoutDiscoveryConfigIsProposed(t *testing.T) {
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() }) // test teardown
	ctx, cancel := context.WithCancel(bg)
	t.Cleanup(cancel)
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", MQTTURL: br.URL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() }) // test teardown
	plain, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "no-disc", Profile: simulator.ProfileESP8266_4M, Version: "15.1.0", NoDiscovery: true})
	if err != nil {
		t.Fatal(err)
	}
	announced, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "announced", Profile: simulator.ProfileESP32, Version: "15.1.0"})
	if err != nil {
		t.Fatal(err)
	}

	e := newEnv(t, "127.0.0.0/8", nil)
	e.mgr.askGrace = 500 * time.Millisecond // long enough for the discovery configs, short for the test
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: br.URL(), ClientID: "tasmofleet-test"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, bus.Connected)
	unsub := e.mgr.Attach(bus)
	t.Cleanup(unsub)
	e.run(t)

	macOf := func(d simulator.Device) string { return d.(interface{ MAC() string }).MAC() }
	deadline := time.Now().Add(10 * time.Second)
	for {
		byMAC := map[string]Proposal{}
		for _, p := range mustProposals(t, e) {
			byMAC[p.Device.MAC] = p
		}
		p1, ok1 := byMAC[macOf(plain)]
		p2, ok2 := byMAC[macOf(announced)]
		if ok1 && ok2 {
			if p1.Device.Topic != plain.Topic() || p1.Via != ViaMQTT || p2.Device.Topic != announced.Topic() {
				t.Fatalf("proposals = %+v / %+v", p1, p2)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proposals = %+v, want both devices", byMAC)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var status5 int
	for _, c := range plain.Commands() {
		if strings.EqualFold(c, "STATUS 5") {
			status5++
		}
		if strings.EqualFold(strings.TrimSpace(c), "STATUS 0") || strings.EqualFold(strings.TrimSpace(c), "STATUS") {
			t.Errorf("automated code sent %q", c)
		}
	}
	if status5 != 1 {
		t.Errorf("Status 5 sent %d times to the device without discovery, want 1: %q", status5, plain.Commands())
	}
	for _, c := range announced.Commands() {
		if strings.HasPrefix(strings.ToUpper(c), "STATUS") {
			t.Errorf("a device with a discovery config was asked %q", c)
		}
	}
}
