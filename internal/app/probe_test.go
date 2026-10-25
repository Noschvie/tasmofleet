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

const (
	status5Net  = `{"StatusNET":{"Hostname":"plug-1","IPAddress":"192.0.2.10","Gateway":"192.0.2.1","Mac":"AA:BB:CC:00:00:01"}}`
	status11Net = `{"StatusSTS":{"Wifi":{"SSId":"IoT","BSSId":"02:00:5E:00:00:01","Channel":11,"RSSI":50}}}`
)

// recorder remembers every command, to prove the probe never sends Status 0.
type recorder struct {
	override
	sent *[]string
}

func (r recorder) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	*r.sent = append(*r.sent, c)
	return r.override.Command(ctx, t, c)
}

// stalling never answers the commands in stall (it waits for the caller to give up), so a
// test sees whether anything waits for them.
type stalling struct {
	tasmota.Client
	stall map[string]bool
	sent  *[]string
}

func (s stalling) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	*s.sent = append(*s.sent, c)
	if s.stall[c] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.Client.Command(ctx, t, c)
}

// Why (maintainer decision 2026-10-24): the network facts (IP, gateway, Wi-Fi from Status 5 and
// Status 11) are details for the device page. Reading them must never hold up first contact
// (import, add) or "Refresh identity": a device whose Status 11 never answers is identified,
// listed and its refresh returns at once; the facts are read later in the background.
func TestFirstContactAndRefreshDoNotWaitForNetworkFacts(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	var sent []string
	p.Client = stalling{Client: p.Client, stall: map[string]bool{"Status 11": true}, sent: &sent}
	var kicked []model.DeviceID
	p.ReadNetLater = func(id model.DeviceID) { kicked = append(kicked, id) }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := p.Describe(ctx, model.Device{Address: d.Address, Port: d.Port})
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity.Family == "" || got.MAC == "" {
		t.Fatalf("first contact must still identify the device: %+v", got)
	}
	if err := p.Refresh(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("first contact or refresh waited for Status 11")
	}
	for _, c := range sent {
		if c == "Status 11" {
			t.Fatalf("Status 11 sent during first contact or refresh: %v", sent)
		}
	}
	if len(kicked) != 1 || kicked[0] != id {
		t.Fatalf("refresh must queue the background read of its device, queued %v", kicked)
	}
	if dv, _ := st.GetDevice(context.Background(), id); !dv.Net.ReadAt.IsZero() {
		t.Fatalf("no facts were read yet, but some were stored: %+v", dv.Net)
	}
}

// Why: the device page shows hostname, IP, gateway and Wi-Fi; they only exist on the device.
// The background read asks Status 5 and Status 11, one command at a time and never Status 0,
// and stores them on the device.
func TestReadNetStoresNetworkFactsWithoutStatus0(t *testing.T) {
	p, st, id, _, _ := raceSetup(t)
	var sent []string
	p.Client = recorder{override{Client: p.Client, reply: map[string]string{"Status 5": status5Net, "Status 11": status11Net}}, &sent}
	p.Now = func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) }
	if err := p.ReadNet(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	d, err := st.GetDevice(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	want := model.NetFacts{Hostname: "plug-1", IP: "192.0.2.10", Gateway: "192.0.2.1", SSID: "IoT", BSSID: "02:00:5E:00:00:01", Channel: 11, ReadAt: p.Now()}
	if d.Net != want {
		t.Fatalf("Net = %+v, want %+v", d.Net, want)
	}
	if strings.Join(sent, ",") != "Status 5,Status 11" {
		t.Fatalf("sent %v, want Status 5 then Status 11", sent)
	}
}

// Why: Status 11 is a nice-to-have. When it fails the read still stores Status 5's facts, and
// the Wi-Fi facts read earlier stay (a wired board or a flaky reply must not blank them).
func TestReadNetKeepsEarlierWifiFactsWhenStatus11Fails(t *testing.T) {
	p, st, id, _, _ := raceSetup(t)
	base := p.Client
	p.Client = override{Client: base, reply: map[string]string{"Status 5": status5Net, "Status 11": status11Net}}
	if err := p.ReadNet(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	p.Client = override{Client: base, reply: map[string]string{"Status 5": status5Net, "Status 11": `{"Command":"Error"}`}}
	if err := p.ReadNet(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	d, err := st.GetDevice(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Net.Hostname != "plug-1" || d.Net.Gateway != "192.0.2.1" || d.Net.SSID != "IoT" || d.Net.Channel != 11 {
		t.Fatalf("facts after a Status 11 failure: %+v", d.Net)
	}
}

// Real Tasmota 16.0.0 answers (self-compiled ESP32-S3 on Ethernet, issue #52). Plain "Status"
// resolves the topic; the "Topic" command would return the setting "tasmota_%06X".
const (
	status16 = `{"Status":{"Module":0,"DeviceName":"Tasmota TV Wand","FriendlyName":["Tasmota"],"Topic":"tasmota_2BE970","ButtonTopic":"0","Power":"0","PowerLock":"00","PowerOnState":3,"LedState":1,"LedMask":"FFFF","SaveData":1,"SaveState":1,"SwitchTopic":"0","SwitchMode":[0,0,0,0,0,0,0,0],"ButtonRetain":0,"SwitchRetain":0,"SensorRetain":0,"PowerRetain":0,"InfoRetain":0,"StateRetain":0,"StatusRetain":0}}`
	// Wi-Fi station idle, Ethernet up: the top level carries the Wi-Fi values, "Ethernet" the real ones.
	status5Eth = `{"StatusNET":{"Hostname":"tasmota-2BE970-2416","IPAddress":"0.0.0.0","Gateway":"0.0.0.0","Subnetmask":"0.0.0.0","DNSServer1":"0.0.0.0","DNSServer2":"0.0.0.0","Mac":"08:D1:F9:2B:E9:70",` +
		`"Ethernet":{"Hostname":"tasmota-2BE970-eth","IPAddress":"192.168.7.139","Gateway":"192.168.7.1","Subnetmask":"255.255.255.0","DNSServer1":"192.168.7.1","DNSServer2":"0.0.0.0","Mac":"08:D1:F9:2B:E9:71"},` +
		`"Webserver":2,"HTTP_API":1,"WifiConfig":4,"WifiPower":17.0}}`
)

// Why: the device list and the Identity card showed the hostname ("tasmota-2BE970-2416") where
// the user set a DeviceName ("Tasmota TV Wand") on the device. DeviceName is what Tasmota shows
// as the device's name, so it is the default name; the topic must be the resolved one, since
// the health tracker matches MQTT messages by topic and "tasmota_%06X" never matches any.
func TestDescribeNamesTheDeviceAfterItsDeviceNameAndResolvesTheTopic(t *testing.T) {
	p, d := probeFixture(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
	p.Client = override{Client: p.Client, reply: map[string]string{"Status": status16, "Topic": `{"Topic":"tasmota_%06X"}`, "Status 5": status5Eth}}
	got, err := p.Describe(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Tasmota TV Wand" {
		t.Errorf("Name = %q, want the DeviceName", got.Name)
	}
	if got.Topic != "tasmota_2BE970" {
		t.Errorf("Topic = %q, want the resolved topic", got.Topic)
	}
	// A device without DeviceName falls back to the hostname.
	p.Client = override{Client: p.Client, reply: map[string]string{"Status": `{"Status":{"DeviceName":"  ","Topic":"t1"}}`}}
	if got, err = p.Describe(context.Background(), d); err != nil || got.Name != "tasmota-2BE970-2416" {
		t.Errorf("fallback name = %q, %v", got.Name, err)
	}
}

// Why: an Ethernet board has no Wi-Fi address; the page showed "IP address 0.0.0.0" and an empty
// Wi-Fi card. The Ethernet object has the real address, and the device is marked wired so the
// page can show an Ethernet card instead.
func TestReadNetTakesTheEthernetAddressWhenWifiHasNone(t *testing.T) {
	p, st, id, _, _ := raceSetup(t)
	p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": status5Eth, "Status 11": status11Net}}
	p.Now = func() time.Time { return time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC) }
	if err := p.ReadNet(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	d, err := st.GetDevice(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	want := model.NetFacts{Hostname: "tasmota-2BE970-eth", IP: "192.168.7.139", Gateway: "192.168.7.1", Wired: true, ReadAt: p.Now()}
	if d.Net != want {
		t.Fatalf("Net = %+v, want %+v (no SSID: Status 11's Wi-Fi block is not this device's link)", d.Net, want)
	}
}

// Why: with both interfaces up, or Wi-Fi only, the Wi-Fi values stay the device's address.
func TestReadNetKeepsWifiWhenItHasAnAddress(t *testing.T) {
	p, st, id, _, _ := raceSetup(t)
	both := strings.Replace(status5Net, `"Mac":"AA:BB:CC:00:00:01"`, `"Mac":"AA:BB:CC:00:00:01","Ethernet":{"IPAddress":"192.0.2.99","Mac":"AA:BB:CC:00:00:02"}`, 1)
	p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": both, "Status 11": status11Net}}
	if err := p.ReadNet(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.GetDevice(context.Background(), id); d.Net.Wired || d.Net.IP != "192.0.2.10" || d.Net.SSID != "IoT" {
		t.Fatalf("Net = %+v", d.Net)
	}
}

// Why: devices added before the topic was resolved hold "tasmota_%06X". They never matched a
// message ("MQTT: Not connected" while online), so a refresh replaces such a topic with the
// real one. A topic the user chose is never touched.
func TestRefreshRepairsAnUnresolvedTopicOnly(t *testing.T) {
	for _, tc := range []struct{ stored, want string }{
		{"tasmota_%06X", "tasmota_2BE970"},
		{"%prefix%-%06X", "tasmota_2BE970"},
		{"my-own-topic", "my-own-topic"},
	} {
		t.Run(tc.stored, func(t *testing.T) {
			p, st, id, d, _ := raceSetup(t)
			d.Topic = tc.stored
			if err := st.UpdateDevice(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			p.Client = override{Client: p.Client, reply: map[string]string{"Status": status16}}
			if err := p.Refresh(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if got, _ := st.GetDevice(context.Background(), id); got.Topic != tc.want {
				t.Fatalf("Topic = %q, want %q", got.Topic, tc.want)
			}
		})
	}
}

// Why: topic and DeviceName come from the device and end up in MQTT subscriptions and the UI.
// A topic that is a wildcard, a path, an unresolved placeholder or contains control characters
// must never be stored; a name has its control and invisible characters removed.
func TestDescribeRejectsUnsafeTopicsAndCleansTheName(t *testing.T) {
	for _, topic := range []string{"#", "a/b", "a+b", "tasmota_%06X", "a\x00b", "a\nb", "a b", "a‮b", strings.Repeat("x", 101), ""} {
		t.Run(topic, func(t *testing.T) {
			p, d := probeFixture(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
			reply, _ := json.Marshal(map[string]any{"Status": map[string]string{"DeviceName": "x", "Topic": topic}})
			p.Client = override{Client: p.Client, reply: map[string]string{"Status": string(reply)}}
			got, err := p.Describe(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if got.Topic != "" {
				t.Fatalf("Topic = %q was taken from the device", got.Topic)
			}
		})
	}
	p, d := probeFixture(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
	p.Client = override{Client: p.Client, reply: map[string]string{"Status": `{"Status":{"DeviceName":"  Wand‮\u0007 TV ","Topic":"t1"}}`}}
	if got, err := p.Describe(context.Background(), d); err != nil || got.Name != "Wand TV" || got.Topic != "t1" {
		t.Fatalf("name %q topic %q err %v", got.Name, got.Topic, err)
	}
}
