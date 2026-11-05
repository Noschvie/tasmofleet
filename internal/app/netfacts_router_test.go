package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// Why (#456 A): the network facts are reads. They go through the router (MQTT first for a device on
// our broker), so a device HTTP cannot reach gets its hostname, IP and Wi-Fi facts too. The
// Prober's own Client stays the raw HTTP client: identity probes verify HTTP and must never be
// rerouted.
func TestReadNetReadsThroughTheRouterNotTheRawHTTPClient(t *testing.T) {
	p, st, id, _, _ := raceSetup(t)
	var viaHTTP, viaRouter []string
	p.Client = recorder{override{Client: p.Client, reply: map[string]string{}}, &viaHTTP}
	p.Reads = recorder{override{Client: p.Client, reply: map[string]string{"Status 5": status5Net, "Status 11": status11Net}}, &viaRouter}
	p.Now = func() time.Time { return time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC) }
	if err := p.ReadNet(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(viaHTTP) != 0 {
		t.Errorf("the raw HTTP client was used: %v", viaHTTP)
	}
	if strings.Join(viaRouter, ",") != "Status 5,Status 11" {
		t.Errorf("router got %v, want Status 5 then Status 11 (never Status 0)", viaRouter)
	}
	d, err := st.GetDevice(context.Background(), id)
	if err != nil || d.Net.IP != "192.0.2.10" || d.Net.SSID != "IoT" {
		t.Fatalf("Net = %+v, err = %v", d.Net, err)
	}
}

// Why: before #456 a "Commands == MQTT" answer meant "HTTP is failing, so MQTT is all there is", which
// is what makes the console ask before a change that can cut the device off (cmdpolicy.WithMQTTOnly).
// Reads now go MQTT first for every live device on our broker; that must not make every such device
// look cut off.
func TestReachedOnlyOverMQTTMeansHTTPFails(t *testing.T) {
	f := transport.Facts{
		HTTP: transport.HTTPState{State: transport.HTTPOK},
		MQTT: transport.MQTTFacts{BusConnected: true, Ours: true, Topic: "plug", TopicExclusive: true, LWTOnline: true},
	}
	if reachedOnlyOverMQTT(f) {
		t.Error("HTTP works: the device is not reached over MQTT only")
	}
	f.HTTP = transport.HTTPState{}
	if reachedOnlyOverMQTT(f) {
		t.Error("HTTP unknown is not failing")
	}
	f.HTTP = transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailUnreachable}
	if !reachedOnlyOverMQTT(f) {
		t.Error("HTTP failing and MQTT usable: MQTT is the only way")
	}
	f.MQTT.LWTOnline = false
	if reachedOnlyOverMQTT(f) {
		t.Error("MQTT unusable: nothing travels over it")
	}
}
