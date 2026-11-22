package backup

import (
	"context"
	"encoding/json"
	"maps"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// Why (#1209): a device without energy monitoring answers Status 9 with a Command Error by design, and the snapshot
// stores that as the expected _error marker. Over MQTT the same device must leave the same value: the real chain
// (simulator, embedded broker, bus, command channel, router) pinned to MQTT, with SetOption4 0 (the failure on RESULT)
// and 1 (on stat/<t>/STATUS), compared with the HTTP snapshot of the same device. Before, MQTT stored the simulator's
// error object as data, and a real SetOption4 1 device timed out and left a gap: two shapes of one device for the
// Changes tab and for every comparison of backups.
func TestStatus9OfANonEnergyDeviceIsTheSameMarkerOverMQTTAndHTTP(t *testing.T) {
	t.Parallel()
	for _, so4 := range []string{"0", "1"} {
		t.Run("SetOption4 "+so4, func(t *testing.T) {
			t.Parallel()
			m, target := newStatus9Rig(t, so4)
			snap := func(p transport.Path) json.RawMessage {
				t.Helper()
				data, failed, _, err := m.snapshot(transport.WithPath(t.Context(), p), target)
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range failed {
					if f == "Status 9" {
						t.Errorf("over %s Status 9 is listed as missing", p)
					}
				}
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(data, &doc); err != nil {
					t.Fatal(err)
				}
				return doc["Status9"]
			}
			marker := func(raw json.RawMessage) map[string]string {
				var v map[string]string
				_ = json.Unmarshal(raw, &v) // a value that is no flat object of strings stays nil and fails below
				return v
			}
			overHTTP, overMQTT := snap(transport.HTTP), snap(transport.MQTT)
			want := marker(errorValue(tasmota.ErrCommandError))
			if !maps.Equal(marker(overHTTP), want) {
				t.Fatalf("over HTTP Status9 = %s, want the Command Error marker", overHTTP)
			}
			if !maps.Equal(marker(overMQTT), want) {
				t.Fatalf("Status9 over MQTT = %s, over HTTP = %s: one device, two shapes", overMQTT, overHTTP)
			}
		})
	}
}

// newStatus9Rig is a backup Manager whose client is a real router in front of a simulated ESP32 without energy
// monitoring, reachable over HTTP and over the real command channel on an embedded broker.
func newStatus9Rig(t *testing.T, so4 string) (*Manager, tasmota.Target) {
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
	dev, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "guest", Profile: simulator.ProfileESP32, Version: "16.0.0", Settings: map[string]string{"setoption4": so4}})
	if err != nil {
		t.Fatal(err)
	}
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: br.URL(), ClientID: "backup-status9-test"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !bus.Connected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("bus never connected")
		}
	}
	ch := mqttcmd.New(bus, mqttcmd.Options{Gap: 5 * time.Millisecond, ReplyWait: 3 * time.Second})
	t.Cleanup(ch.Close)
	host, portStr, err := net.SplitHostPort(dev.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	d := model.Device{ID: 7, Name: "guest", Address: host, Port: port, Topic: dev.Topic(), MQTT: model.MQTTOurs}
	mem := transport.NewMemory(transport.DefaultFailFor, time.Now)
	client := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	rt := transport.NewRouter(transport.Options{HTTP: client, Devices: devs{d}, Facts: mqttFacts{mem}, Memory: mem})
	rt.SetMQTT(ch)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := ch.Do(ctx, d, "Status 5"); err == nil {
			break // the channel has seen the device come online
		}
		if time.Now().After(deadline) {
			t.Fatal("device never answered over MQTT")
		}
	}
	e := newEnv(t, func(o *Options) { o.Client, o.Devices = rt, devs{d} })
	return e.m, tasmota.TargetFor(d)
}
