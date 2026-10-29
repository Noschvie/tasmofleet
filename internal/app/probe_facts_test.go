package app

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// factsSetup stores a device for a simulator spec and points a prober at it. The sim's own
// Status 5 MAC is bound, so the refresh is accepted as the stored device.
func factsSetup(t *testing.T, spec simulator.DeviceSpec) (*Prober, *store.SQLite, model.DeviceID, *[]string) {
	t.Helper()
	p, d := probeFixture(t, spec)
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
	desc, err := p.Describe(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.CreateDevice(context.Background(), desc)
	if err != nil {
		t.Fatal(err)
	}
	sent := new([]string)
	p.Client = recorder{override{Client: p.Client}, sent}
	p.Devices = st
	return p, st, id, sent
}

// Why: the Ethernet rows of the Overview (#145) need subnet and DNS, and a wired board reports
// them inside the "Ethernet" object while the Wi-Fi ones stay 0.0.0.0. The unset second DNS
// server is not shown.
func TestReadNetStoresSubnetAndDNSForWifiAndEthernet(t *testing.T) {
	for _, tc := range []struct {
		name          string
		wired         bool
		subnet, dns   string
		wantWired     bool
		wantIPNotZero bool
	}{
		{"wifi", false, "255.255.255.0", "172.31.13.1", false, true},
		{"ethernet", true, "255.255.255.0", "172.31.13.1", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, st, id, _ := factsSetup(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M, Wired: tc.wired})
			if err := p.ReadNet(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			d, _ := st.GetDevice(context.Background(), id)
			if d.Net.Subnet != tc.subnet || d.Net.DNS != tc.dns || d.Net.Wired != tc.wantWired {
				t.Fatalf("Net = %+v, want subnet %q dns %q wired %v", d.Net, tc.subnet, tc.dns, tc.wantWired)
			}
			if d.Net.IP == "" || d.Net.IP == "0.0.0.0" {
				t.Fatalf("IP = %q", d.Net.IP)
			}
		})
	}
}

// Why: a second DNS server is shown next to the first; an unset one (0.0.0.0) is not.
func TestStatus5DNSJoinsOnlySetServers(t *testing.T) {
	var out probed
	n, err := parseStatus5([]byte(`{"StatusNET":{"IPAddress":"10.0.0.5","Subnetmask":"255.255.0.0","DNSServer1":"10.0.0.1","DNSServer2":"9.9.9.9","Mac":"AA:BB:CC:00:00:01"}}`), &out)
	if err != nil || n.DNS != "10.0.0.1, 9.9.9.9" || n.Subnet != "255.255.0.0" {
		t.Fatalf("%+v, %v", n, err)
	}
	n, _ = parseStatus5([]byte(`{"StatusNET":{"IPAddress":"10.0.0.5","Subnetmask":"0.0.0.0","DNSServer1":"0.0.0.0","DNSServer2":"0.0.0.0","Mac":"AA:BB:CC:00:00:01"}}`), &out)
	if n.DNS != "" || n.Subnet != "" {
		t.Fatalf("unset values must read as unknown: %+v", n)
	}
}

// Why: the Overview names each relay (#145). Tasmota keeps them in the FriendlyName array of
// plain "Status", so a refresh stores that array; defaults are stored as reported.
func TestRefreshStoresRelayNamesFromFriendlyName(t *testing.T) {
	p, st, id, sent := factsSetup(t, simulator.DeviceSpec{Name: "strip", Profile: simulator.ProfileESP8266_1M, Relays: 3,
		FriendlyNames: []string{"Pump", "Tasmota2", "Light"}})
	if err := p.Refresh(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	d, _ := st.GetDevice(context.Background(), id)
	if want := []string{"Pump", "Tasmota2", "Light"}; !reflect.DeepEqual(d.RelayNames, want) {
		t.Fatalf("RelayNames = %q, want %q", d.RelayNames, want)
	}
	for _, c := range *sent {
		if c == "Status 0" || c == "Status0" {
			t.Fatal("the probe sent Status 0")
		}
	}
}

// Why (#127): the OTA URL a device would update from is useful on the Overview, but it often
// carries credentials (http://user:pass@host/...). One read-only "Status 1" per refresh reads
// it (never the OtaUrl command, which could change it); the credentials are stripped before
// anything is stored, so they can never reach the page, a backup or the log.
func TestRefreshStoresOtaURLWithoutCredentials(t *testing.T) {
	p, st, id, sent := factsSetup(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
	p.Client = recorder{override{Client: p.Client, reply: map[string]string{
		"Status 1": `{"StatusPRM":{"OtaUrl":"http://admin:s3cret@fw.example.internal:8080/data/tasmota.bin.gz","Uptime":"0T01:00:00"}}`,
	}}, sent}
	if err := p.Refresh(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	d, _ := st.GetDevice(context.Background(), id)
	if d.OTAURL != "http://fw.example.internal:8080/data/tasmota.bin.gz" {
		t.Fatalf("OTAURL = %q", d.OTAURL)
	}
	if strings.Contains(d.OTAURL, "s3cret") || strings.Contains(d.OTAURL, "admin") {
		t.Fatal("credentials stored")
	}
	n := 0
	for i, c := range *sent {
		switch c {
		case "Status 1":
			n++
			if i == 0 || (*sent)[i-1] == "Status 1" {
				t.Fatalf("Status 1 out of order: %v", *sent)
			}
		case "Status 0", "OtaUrl", "Upgrade 1":
			t.Fatalf("forbidden command %q sent: %v", c, *sent)
		}
	}
	if n != 1 {
		t.Fatalf("Status 1 sent %d times, want once: %v", n, *sent)
	}
}

// Why: a Status 1 that fails or whose OtaUrl cannot be parsed safely must keep what was stored
// (best effort, like Status 11), and an unparseable value is dropped instead of stored raw,
// since it might hold credentials.
func TestRefreshKeepsOtaURLWhenStatus1FailsAndDropsUnsafeValues(t *testing.T) {
	p, st, id, sent := factsSetup(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
	base := p.Client
	good := `{"StatusPRM":{"OtaUrl":"http://fw.example/a.bin"}}`
	p.Client = recorder{override{Client: base, reply: map[string]string{"Status 1": good}}, sent}
	if err := p.Refresh(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	for _, reply := range []string{`{"Command":"Unknown"}`, `{"StatusPRM":{"OtaUrl":"http://u:p@[bad/x"}}`} {
		p.Client = recorder{override{Client: base, reply: map[string]string{"Status 1": reply}}, sent}
		if err := p.Refresh(context.Background(), id, ""); err != nil {
			t.Fatal(err)
		}
		d, _ := st.GetDevice(context.Background(), id)
		if d.OTAURL != "http://fw.example/a.bin" {
			t.Fatalf("after %s: OTAURL = %q, want the earlier value", reply, d.OTAURL)
		}
	}
}

// Why: minimal firmware answers neither Status 1 nor Status; flash settings are not readable
// there, so a refresh must keep the values read on the full firmware and send no Status 1.
func TestRefreshOnMinimalFirmwareKeepsDeviceFacts(t *testing.T) {
	p, st, id, sent := factsSetup(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266Min})
	d, _ := st.GetDevice(context.Background(), id)
	g := store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}
	if err := st.UpdateProbe(context.Background(), id, g, store.ProbeResult{SetOTAURL: true, OTAURL: "http://fw.example/a.bin", RelayNames: []string{"Pump"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	d, _ = st.GetDevice(context.Background(), id)
	if d.OTAURL != "http://fw.example/a.bin" || !reflect.DeepEqual(d.RelayNames, []string{"Pump"}) {
		t.Fatalf("minimal refresh changed the facts: %q %q", d.OTAURL, d.RelayNames)
	}
	for _, c := range *sent {
		if c == "Status 1" || c == "Status" {
			t.Fatalf("minimal firmware was sent %q: %v", c, *sent)
		}
	}
}
