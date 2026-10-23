package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

func probeFixture(t *testing.T, spec simulator.DeviceSpec) (*Prober, model.Device) {
	t.Helper()
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	dev, err := fleet.Add(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitAddr(t, dev.Addr())
	client := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()})
	d := model.Device{Name: "", Address: host, Port: port, HTTPUser: spec.User, HTTPPassword: spec.Password}
	return &Prober{Client: client, Gap: time.Millisecond}, d
}

// Why: adding a device must fill MAC, topic and identity from the device so
// that OTA preflight and health can rely on them, and must never send Status 0.
func TestDescribeFillsIdentityWithoutStatus0(t *testing.T) {
	p, d := probeFixture(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M})
	got, err := p.Describe(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if got.MAC == "" || got.Topic == "" || got.Name == "" || got.Identity.Family != model.ChipESP8266 || got.Identity.Version == "" {
		t.Fatalf("incomplete: %+v", got)
	}
	if got.MQTT == model.MQTTUnknown {
		t.Fatal("MQTT capability must be probed")
	}
}

// Why: minimal firmware only answers a few commands; the probe has to take
// version and family from Upgrade and the main page instead of failing.
func TestDescribeMinimalFirmware(t *testing.T) {
	p, d := probeFixture(t, simulator.DeviceSpec{Name: "m", Profile: simulator.ProfileESP8266Min})
	got, err := p.Describe(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity.Variant != "minimal" || got.Identity.Family != model.ChipESP8266 {
		t.Fatalf("identity: %+v", got.Identity)
	}
}

type fixedBroker struct{ host string }

func (b fixedBroker) Same(host string, port int) bool { return host == b.host && port == 1883 }

// override answers selected commands itself and forwards the rest.
type override struct {
	tasmota.Client
	reply map[string]string
}

func (o override) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if r, ok := o.reply[c]; ok {
		return json.RawMessage(r), nil
	}
	return o.Client.Command(ctx, t, c)
}

// Why: "another broker" was shown for every device when tasmofleet had no
// broker at all. The probe must record where the device reports to and
// classify it relative to OUR broker.
func TestDescribeMQTTClassification(t *testing.T) {
	const status6 = `{"StatusMQT":{"MqttHost":"mqtt.example.internal","MqttPort":1883,"MqttUser":"secret-user"}}`
	cases := []struct {
		name     string
		broker   brokerMatcher
		reply    map[string]string
		wantCap  model.MQTTCapability
		wantHost string
	}{
		{"no tasmofleet broker", nil, nil, model.MQTTOtherBroker, "mqtt.example.internal"},
		{"same broker", fixedBroker{"mqtt.example.internal"}, nil, model.MQTTOurs, "mqtt.example.internal"},
		{"different broker", fixedBroker{"other.lan"}, nil, model.MQTTOtherBroker, "mqtt.example.internal"},
		{"empty MqttHost", fixedBroker{"mqtt.example.internal"}, map[string]string{"Status 6": `{"StatusMQT":{"MqttHost":"","MqttPort":1883}}`}, model.MQTTNone, ""},
		{"SetOption3 off", fixedBroker{"mqtt.example.internal"}, map[string]string{"Status 6": status6, "SetOption3": `{"SetOption3":"OFF"}`}, model.MQTTNone, "mqtt.example.internal"},
		{"SetOption3 on", fixedBroker{"mqtt.example.internal"}, map[string]string{"Status 6": status6, "SetOption3": `{"SetOption3":"ON"}`}, model.MQTTOurs, "mqtt.example.internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, d := probeFixture(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
			p.Client = override{Client: p.Client, reply: tc.reply}
			p.Broker = tc.broker
			got, err := p.Describe(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if got.MQTT != tc.wantCap || got.MQTTHost != tc.wantHost {
				t.Fatalf("MQTT = %q host %q, want %q host %q", got.MQTT, got.MQTTHost, tc.wantCap, tc.wantHost)
			}
			if tc.wantHost != "" && got.MQTTPort != 1883 {
				t.Fatalf("port = %d", got.MQTTPort)
			}
			if b, _ := json.Marshal(got); strings.Contains(string(b), "secret-user") {
				t.Fatal("the device's MQTT user leaked into the device record")
			}
		})
	}
}
