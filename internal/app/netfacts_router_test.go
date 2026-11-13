package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/transport"
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
// Reads now go MQTT first for every live device on our broker; that alone must not make a device look cut off.
// What counts is whether HTTP is known to work (#605 review): only a fresh answer does. Unknown (nothing polls
// the HTTP of a device live on MQTT, a timeout is never recorded as a failure) is not usable, so the console asks
// exactly where the router would refuse the change without a confirmation.
func TestReachedOnlyOverMQTTMeansHTTPIsNotKnownToWork(t *testing.T) {
	f := transport.Facts{
		HTTP: transport.HTTPState{State: transport.HTTPOK},
		MQTT: transport.MQTTFacts{BusConnected: true, Ours: true, Topic: "plug", TopicExclusive: true, LWTOnline: true},
	}
	if reachedOnlyOverMQTT(f) {
		t.Error("HTTP works: the device is not reached over MQTT only")
	}
	f.HTTP = transport.HTTPState{}
	if !reachedOnlyOverMQTT(f) {
		t.Error("HTTP unknown and MQTT usable: HTTP is not known to reach the device, so the console must ask")
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

// Why: the console's "this can cut the device off" question (web/console.go, web/policyfix.go) is fed
// by runtime.onlyOverMQTT with the device's real facts. A device whose HTTP answered lately is not asked
// about; one whose HTTP is unknown or failing is, as the router then refuses the change without a confirmation.
func TestOnlyOverMQTTFollowsWhetherHTTPIsKnownToWork(t *testing.T) {
	d := model.Device{ID: 1, MQTT: model.MQTTOurs, Topic: "plug"}
	a := &runtime{facts: newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Online", SeenHTTP: factsNow.Add(-time.Minute)}}, fakeMemory{})}
	if a.onlyOverMQTT(context.Background(), 1) {
		t.Error("HTTP answered a minute ago: the device is not reached over MQTT only")
	}
	a.facts = newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Online"}}, fakeMemory{})
	if !a.onlyOverMQTT(context.Background(), 1) {
		t.Error("HTTP never answered and MQTT usable: HTTP is not known to reach the device")
	}
	a.facts.Memory = fakeMemory{kind: transport.HTTPFailUnreachable, since: factsNow.Add(-time.Minute), ok: true}
	if !a.onlyOverMQTT(context.Background(), 1) {
		t.Error("HTTP failing and MQTT usable: MQTT is the only way")
	}
}

// Why (#605 review, round 3): the bulk preview and Make compliant's plan refuse a change of the MQTT connection up
// front only for a device known to be reached over MQTT only (HTTP failing), as before #605. "Not proven usable" is
// the normal state of every device live on MQTT; refusing there would stop MqttHost or a broker move for the whole
// fleet although HTTP may well answer. Those sends carry no consent, and the router asks HTTP per device right
// before the change and refuses it where HTTP is silent, so nothing is lost by not refusing up front.
func TestKnownOnlyOverMQTTMeansHTTPIsFailing(t *testing.T) {
	d := model.Device{ID: 1, MQTT: model.MQTTOurs, Topic: "plug"}
	a := &runtime{facts: newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Online"}}, fakeMemory{})}
	if a.knownOnlyOverMQTT(context.Background(), 1) {
		t.Error("HTTP unknown and MQTT usable: not known to be reached over MQTT only, the bulk preview must not refuse")
	}
	a.facts = newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Online", SeenHTTP: factsNow.Add(-time.Minute)}}, fakeMemory{})
	if a.knownOnlyOverMQTT(context.Background(), 1) {
		t.Error("HTTP answered a minute ago: not reached over MQTT only")
	}
	a.facts.Memory = fakeMemory{kind: transport.HTTPFailUnreachable, since: factsNow.Add(-time.Minute), ok: true}
	if !a.knownOnlyOverMQTT(context.Background(), 1) {
		t.Error("HTTP failing and MQTT usable: MQTT is the only way")
	}
	a.facts = newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Offline"}}, fakeMemory{kind: transport.HTTPFailUnreachable, since: factsNow.Add(-time.Minute), ok: true})
	if a.knownOnlyOverMQTT(context.Background(), 1) {
		t.Error("MQTT unusable: nothing travels over it")
	}
}

// Why: a different device on the topic (or the address) must not get its network facts stored on this
// one; over MQTT the answer comes from whoever listens on the topic. The rule is a refresh's (checkMAC,
// #65): the answer names one of the stored MACs. Tasmota always reports its MAC in Status 5, so an
// answer without one proves nothing and is refused; a device stored without a MAC has nothing to compare.
func TestReadNetStoresOnlyAnAnswerOfTheStoredDevice(t *testing.T) {
	const wifi, eth = "aa:bb:cc:00:00:01", "aa:bb:cc:00:00:02"
	withEth := strings.Replace(status5Net, `"Mac":"AA:BB:CC:00:00:01"`, `"Mac":"AA:BB:CC:00:00:01","Ethernet":{"Mac":"AA:BB:CC:00:00:02"}`, 1)
	for _, tc := range []struct {
		name    string
		stored  []string // the device's MACs, primary first; none: stored without a MAC
		status5 string
		stores  bool
	}{
		{"the stored MAC", []string{wifi}, status5Net, true},
		{"another device's MAC", []string{wifi}, strings.Replace(status5Net, "AA:BB:CC:00:00:01", "AA:BB:CC:00:00:99", 1), false},
		{"no MAC in the answer", []string{wifi}, strings.Replace(status5Net, `,"Mac":"AA:BB:CC:00:00:01"`, "", 1), false},
		{"the Ethernet MAC is the stored one", []string{eth}, withEth, true},
		{"stored without a MAC: nothing to compare", nil, status5Net, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, st, id := readNetRig(t, tc.status5, tc.stored...)
			err := p.ReadNet(t.Context(), id)
			d, gerr := st.GetDevice(t.Context(), id)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if stored := d.Net.IP != ""; (err == nil) != tc.stores || stored != tc.stores {
				t.Fatalf("err=%v Net=%+v, want stored=%v", err, d.Net, tc.stores)
			}
		})
	}
}

// readNetRig stores one device with the given MACs (none: a device stored without a MAC) and returns a
// prober whose background reads answer Status 5 with status5 and Status 11 with status11Net.
func readNetRig(t *testing.T, status5 string, macs ...string) (*Prober, *store.SQLite, model.DeviceID) {
	t.Helper()
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
	d := model.Device{Name: "plug", Address: "192.0.2.10"}
	if len(macs) > 0 {
		d.MAC, d.MACs = macs[0], macs
	}
	id, err := st.CreateDevice(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	reads := override{reply: map[string]string{"Status 5": status5, "Status 11": status11Net}}
	return &Prober{Reads: reads, Devices: st, Gap: time.Millisecond}, st, id
}

// Why: the Prober's background reads must go through the router; with the raw client they would stay
// HTTP only and the network facts of a device HTTP cannot reach would never arrive.
func TestServicesWireTheBackgroundReadsToTheRouter(t *testing.T) {
	svc := importServices(t)
	if svc.Prober.Reads != svc.Client {
		t.Error("Prober.Reads is not the router")
	}
	if svc.Prober.Client == svc.Client {
		t.Error("identity probes must keep the raw HTTP client")
	}
}
