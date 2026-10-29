package web

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// Tests of the reworked device Overview (#145), its live refresh (#126, #160) and the shapes of
// the mockups in docs/design/device-overview.

func meter(w, today, yest, total float64) health.EnergyChannel {
	return health.EnergyChannel{Power: f64(w), Today: f64(today), Yesterday: f64(yest), Total: f64(total)}
}

func tileKeys(tiles []tile) string {
	var keys []string
	for _, t := range tiles {
		keys = append(keys, t.Key)
	}
	return strings.Join(keys, ",")
}

var now0 = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// onlineHealth is a device that answers on both channels.
func onlineHealth(power map[string]string) model.Health {
	return model.Health{State: model.HealthOnline, LastSeen: now0, SeenHTTP: now0, SeenMQTT: now0, LWT: "Online", UptimeSec: 3*86400 + 2*3600, RSSI: 58, Power: power}
}

// Why: the tile row is one row of equal tiles and never has holes: a tile that does not apply is
// not built. Power and Energy exist only with a meter, Wi-Fi turns into Link on Ethernet, and
// Policy only when there are rules. The order is fixed (the eight mockup shapes of #145).
func TestTilesCoverTheDeviceShapes(t *testing.T) {
	mqtt := model.Device{MQTT: model.MQTTOurs}
	okPolicy := &policyCard{Rules: 7}
	tests := []struct {
		name string
		in   overviewIn
		want string
	}{
		{"1 relay, meter, Wi-Fi", overviewIn{Dev: mqtt, Health: onlineHealth(map[string]string{"POWER": "ON"}),
			Tele: health.Telemetry{Energy: map[string]health.EnergyChannel{"": meter(133, 0.45, 0, 8.5)}}, Policy: okPolicy}, "power,energy,wifi,uptime,reach,policy"},
		{"1 relay, no meter, Wi-Fi", overviewIn{Dev: mqtt, Health: onlineHealth(map[string]string{"POWER": "ON"}), Policy: okPolicy}, "wifi,uptime,reach,policy"},
		{"4 relays over Ethernet, no meter", overviewIn{Dev: mqtt, Health: onlineHealth(nil), Facts: snapFacts{Wired: true}, Policy: okPolicy}, "link,uptime,reach,policy"},
		{"no policy configured", overviewIn{Dev: mqtt, Health: onlineHealth(nil)}, "wifi,uptime,reach"},
		{"policy without rules", overviewIn{Dev: mqtt, Health: onlineHealth(nil), Policy: &policyCard{}}, "wifi,uptime,reach"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tileKeys(tilesOf(tc.in)); got != tc.want {
				t.Fatalf("tiles %s, want %s", got, tc.want)
			}
		})
	}
}

// Why: several channels report as arrays; the Power tile adds them up and says so, and the Energy
// tile adds the counters, with yesterday and the lifetime total as its subline.
func TestPowerTilesSumTheChannels(t *testing.T) {
	tl := powerTiles(health.Telemetry{Energy: map[string]health.EnergyChannel{
		"1": meter(42, 0.5, 0.4, 100), "2": meter(96, 0.7, 0.8, 200), "3": meter(20, 0.2, 0.2, 100), "4": meter(0, 0, 0.64, 12)}})
	if len(tl) != 2 || tl[0].Value != "158 W" || tl[0].Sub != "all channels" {
		t.Fatalf("power: %+v", tl)
	}
	if tl[1].Value != "1.40 kWh" || tl[1].Sub != "Yesterday 2.04 · Total 412" {
		t.Fatalf("energy: %+v", tl[1])
	}
	one := powerTiles(health.Telemetry{Energy: map[string]health.EnergyChannel{"": meter(133, 0.45, 0, 8.5)}})
	if one[0].Value != "133 W" || one[0].Sub != "now" || one[1].Sub != "Yesterday 0.00 · Total 8.5" {
		t.Fatalf("single meter: %+v", one)
	}
	if powerTiles(health.Telemetry{}) != nil {
		t.Fatal("a device without a meter has no Power tile")
	}
}

// Why: the signal bar answers "can I trust this link": green above -60 dBm, amber down to
// -75, red below (the weak Wi-Fi of mockup 4 and 8).
func TestWifiTileBarToneFollowsTheSignal(t *testing.T) {
	for _, tc := range []struct {
		dbm  int
		tone ui.Tone
	}{{-48, ui.ToneOK}, {-59, ui.ToneOK}, {-60, ui.ToneWarn}, {-71, ui.ToneWarn}, {-75, ui.ToneWarn}, {-76, ui.ToneDanger}, {-90, ui.ToneDanger}} {
		in := overviewIn{Health: model.Health{RSSI: 52}, Tele: health.Telemetry{SignalDBm: tc.dbm, HasSignal: true}, Facts: snapFacts{SSID: "IoT", Channel: 11}}
		tl := linkTile(in)
		if tl.BarTone != tc.tone || tl.Bar == 0 {
			t.Errorf("%d dBm: tone %q bar %d, want %q", tc.dbm, tl.BarTone, tl.Bar, tc.tone)
		}
		if tl.Value != strconvDBm(tc.dbm) || tl.Sub != "52 % · IoT · ch 11" {
			t.Errorf("%d dBm: value %q sub %q", tc.dbm, tl.Value, tl.Sub)
		}
	}
	wired := linkTile(overviewIn{Facts: snapFacts{Wired: true}})
	if wired.Key != "link" || wired.Value != "Ethernet" || wired.Sub != "Wi-Fi off" || wired.Bar != 0 {
		t.Errorf("wired: %+v", wired)
	}
}

func strconvDBm(n int) string { return strings.TrimSpace(strings.Join([]string{itoa(n), "dBm"}, " ")) }

// Why: "Reachability" tells HTTP-only devices from devices on our broker, and an offline device
// says Offline instead of looking half-alive ("MQTT not configured" must not read as a fault).
func TestReachTileSaysWhichChannelsAnswer(t *testing.T) {
	ours := model.Device{MQTT: model.MQTTOurs}
	tests := []struct {
		name       string
		d          model.Device
		hl         model.Health
		value, sub string
		tone       ui.Tone
	}{
		{"both", ours, onlineHealth(nil), "MQTT · HTTP", "Connected · Reachable", ui.ToneOK},
		{"HTTP only", model.Device{}, model.Health{State: model.HealthOnline, LastSeen: now0, SeenHTTP: now0}, "HTTP", "MQTT not configured", ui.ToneOK},
		{"on our broker but silent", ours, model.Health{State: model.HealthOnline, LastSeen: now0, SeenHTTP: now0}, "HTTP", "MQTT not connected", ui.ToneWarn},
		{"offline", ours, model.Health{State: model.HealthOffline, LastSeen: now0, LWT: "Offline"}, "Offline", "MQTT LWT offline · HTTP no answer", ui.ToneNeutral},
		{"never answered", model.Device{}, model.Health{State: model.HealthUnknown}, "Unknown", "The device has not answered yet", ui.ToneNeutral},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := reachTile(tc.d, tc.hl)
			if got.Value != tc.value || got.Sub != tc.sub || got.Tone != tc.tone {
				t.Fatalf("got %q / %q / %q", got.Value, got.Sub, got.Tone)
			}
		})
	}
}

// Why: Policy is "n / m": green when everything matches, amber naming the first failing
// setting (not just a count), and only an admin gets "Make compliant…".
func TestPolicyTileNamesTheFirstFailingSetting(t *testing.T) {
	if tl := policyTile(&policyCard{Rules: 7}, false); tl.Value != "7 / 7" || tl.Tone != ui.ToneOK || tl.Sub != "Matches all rules" || tl.Fix {
		t.Fatalf("compliant: %+v", tl)
	}
	bad := &policyCard{Rules: 7, CanFix: true, Deviations: []deviation{{Rule: "Telemetry", Have: "TelePeriod 60", Want: "300"}, {Rule: "x", Have: "SetOption0 1", Want: "0"}}}
	if tl := policyTile(bad, false); tl.Value != "5 / 7" || tl.Tone != ui.ToneWarn || tl.Sub != "TelePeriod 60 → 300 (+1 more)" || !tl.Fix {
		t.Fatalf("deviating: %+v", tl)
	}
	bad.CanFix = false
	if tl := policyTile(bad, false); tl.Fix {
		t.Fatal("an operator gets no fix")
	}
	if tl := policyTile(&policyCard{Rules: 7}, true); tl.Sub != "Last check before going offline" {
		t.Fatalf("offline: %+v", tl)
	}
	if policyTile(nil, false) != nil {
		t.Fatal("no policy, no tile")
	}
}

// Why: the relay box is the same box for one relay and four. Watts per relay only when the
// device meters each channel (as many channels as relays); otherwise the state, so that one
// shared meter is never shown as a reading of one relay.
func TestRelayBoxMeteringLabelsAndPerChannelWatts(t *testing.T) {
	four := map[string]string{"POWER1": "ON", "POWER2": "ON", "POWER3": "ON", "POWER4": "OFF"}
	perChannel := health.Telemetry{Energy: map[string]health.EnergyChannel{"1": meter(42, 0, 0, 0), "2": meter(96, 0, 0, 0), "3": meter(20, 0, 0, 0), "4": meter(0, 0, 0, 0)}}
	box := relayBoxOf(model.Health{Power: four}, perChannel, true)
	if box.Right != "3 of 4 on · metered per channel" || box.Rows[0].Line != "42 W" || box.Rows[3].Line != "0 W" || box.Rows[1].Cmd != "POWER2" {
		t.Fatalf("per channel: %+v", box)
	}
	two := map[string]string{"POWER1": "ON", "POWER2": "OFF"}
	shared := relayBoxOf(model.Health{Power: two}, health.Telemetry{Energy: map[string]health.EnergyChannel{"": meter(60, 0, 0, 0)}}, true)
	if shared.Right != "1 of 2 on · one meter for all" || shared.Rows[0].Line != "On" || shared.Rows[1].Line != "Off" {
		t.Fatalf("one meter for two relays: %+v", shared)
	}
	// a meter with 3 channels on 2 relays is not "per channel": counts differ
	if odd := relayBoxOf(model.Health{Power: two}, health.Telemetry{Energy: map[string]health.EnergyChannel{"1": meter(1, 0, 0, 0), "2": meter(2, 0, 0, 0), "3": meter(3, 0, 0, 0)}}, true); odd.Rows[0].Line != "On" {
		t.Fatalf("channel count differs from relay count: %+v", odd)
	}
	none := relayBoxOf(model.Health{Power: four}, health.Telemetry{}, true)
	if none.Right != "3 of 4 on · no energy meter" || none.Rows[0].Line != "On" {
		t.Fatalf("no meter: %+v", none)
	}
	one := relayBoxOf(model.Health{Power: map[string]string{"POWER": "ON"}}, health.Telemetry{Energy: map[string]health.EnergyChannel{"": meter(133, 0, 0, 0)}}, true)
	if one.Right != "metered" || one.Rows[0].Cmd != "POWER" || one.Rows[0].Line != "On" {
		t.Fatalf("one relay: %+v", one)
	}
	if relayBoxOf(model.Health{}, health.Telemetry{}, true) != nil {
		t.Fatal("a device without relays has no relay box")
	}
}

// Why: Connection shows how the device is connected: SSID/BSSID/channel on Wi-Fi, the Ethernet MAC
// on a cable, and says so when no broker is configured (HTTP-only devices, mockup 7).
func TestConnectionCardFollowsTheLink(t *testing.T) {
	wifi := connOf(overviewIn{Dev: model.Device{Address: "172.27.13.26", MQTTHost: "mqtt.home.internal"}, Facts: snapFacts{Gateway: "172.27.13.1", SSID: "IoT", BSSID: "8C:30", Channel: 11}})
	if wifi.Title != "Connection · Wi-Fi" || kvKeys(wifi.Rows) != "Address,Gateway,SSID,BSSID,Channel,MQTT broker" || wifi.Rows[5].Value != "mqtt.home.internal:1883" || wifi.Rows[4].Value != "11 (2.4 GHz)" {
		t.Fatalf("wifi: %+v", wifi)
	}
	eth := connOf(overviewIn{Dev: model.Device{Address: "172.27.13.44", MAC: "aa", MACs: []string{"aa", "bb"}}, Facts: snapFacts{Wired: true, Gateway: "172.27.13.1"}})
	if eth.Title != "Connection · Ethernet" || kvKeys(eth.Rows) != "Address,Gateway,Ethernet MAC,MQTT broker" || eth.Rows[2].Value != "bb" || eth.Rows[3].Value != "not configured" {
		t.Fatalf("ethernet: %+v", eth)
	}
}

func kvKeys(rows []ui.KVRow) string {
	var k []string
	for _, r := range rows {
		k = append(k, r.Key)
	}
	return strings.Join(k, ",")
}

// Why: Identity is the static facts in a fixed order; "Chip" is gone (the family is in the header's
// meta line) and the hardware row carries the flash size.
func TestIdentityRowsFollowTheMockup(t *testing.T) {
	e := setup(t)
	d := e.inv.devs[1]
	d.Identity.Hardware, d.Identity.FlashSizeKB = "ESP8266EX", 2048
	e.inv.devs[1] = d
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	i := strings.Index(body, `id="dc-ident"`)
	j := strings.Index(body, `id="dc-conn"`)
	ident := body[i:j]
	if strings.Contains(ident, "<dt>Chip</dt>") || !strings.Contains(ident, "ESP8266EX · 2 MB flash") {
		t.Errorf("identity:\n%s", ident)
	}
	last, order := -1, []string{"<dt>Device name", "<dt>Hostname", "<dt>MQTT topic", "<dt>Hardware", "<dt>Build", "<dt>Firmware", "<dt>MAC"}
	for _, k := range order {
		at := strings.Index(ident, k)
		if at < last {
			t.Fatalf("%s is out of order", k)
		}
		last = at
	}
}

// Why (#126): the overview must follow the device without a reload. The live region asks for
// itself on the events of this device only (the stream is shared by every page, so the event
// names carry the id) and on resync (#160).
func TestOverviewLiveRegionListensToThisDeviceOnly(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/1", "operator", nil)
	for _, want := range []string{`hx-get="/tf/devices/1/live"`, `sse:devstate-1 throttle:2s`, `sse:devjob-1 throttle:2s`, `sse:power-1 throttle:10s`, `sse:uptime-1 throttle:10s`, `sse:resync`} {
		if !strings.Contains(body, want) {
			t.Errorf("live region lacks %q", want)
		}
	}
	if strings.Contains(body, "devstate-2") || strings.Contains(body, "sse:state") {
		t.Error("the region must not listen to other devices' events")
	}
}

// Why (#126): the live answer carries the region, the Activity box, the header and the banner slot,
// and never the Identity card (it holds answers and notices a swap would erase).
func TestLiveAnswerCarriesTheRegionsButNotTheIdentity(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.Health.(fakeHealth)[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: now0, Power: map[string]string{"POWER": "ON"}}
	})
	resp, body := e.do(t, "GET", "/devices/1/live", "operator", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{`id="devlive"`, `<section id="devact"`, `hx-swap-oob="true"`, `id="devhd"`, `id="devhd-slot"`, `id="devhd-off"`} {
		if !strings.Contains(body, want) {
			t.Errorf("live answer lacks %q", want)
		}
	}
	for _, bad := range []string{`id="identity"`, `id="devconn"`, "<html", "csrf-leak"} {
		if strings.Contains(body, bad) {
			t.Errorf("live answer carries %q", bad)
		}
	}
	if r, _ := e.do(t, "GET", "/devices/99/live", "operator", nil); r.StatusCode != 404 {
		t.Errorf("unknown device: %d", r.StatusCode)
	}
}

// Why (#126): a relay switched on the overview updates the box without a reload: the answer to the
// htmx post is the live region with the new state ("n of m on" included), also before the health
// tracker has heard of it. A failed command is said in place.
func TestRelaySwitchAnswersWithTheLiveRegion(t *testing.T) {
	f := &fakeCmd{state: "OFF"}
	e := setupWith(t, func(d *Deps) {
		d.Health.(fakeHealth)[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), SeenHTTP: time.Now(), Power: map[string]string{"POWER1": "ON", "POWER2": "ON"}}
		d.DeviceCmd = f
	})
	resp, body := e.do(t, "POST", "/devices/1/power/2", "operator", url.Values{"csrf": {"tok"}, "action": {"OFF"}, "back": {"device"}}, "HX-Request", "true")
	if resp.StatusCode != 200 || !strings.Contains(body, `id="devlive"`) || !strings.Contains(body, "1 of 2 on") || !strings.Contains(body, `aria-label="Relay 2 is Off: switch it On"`) {
		t.Fatalf("status %d:\n%s", resp.StatusCode, body)
	}
	if strings.Contains(body, `id="identity"`) {
		t.Error("the switch answer carries the Identity card")
	}
	f.powerErr = context.DeadlineExceeded
	_, failed := e.do(t, "POST", "/devices/1/power/2", "operator", url.Values{"csrf": {"tok"}, "action": {"OFF"}, "back": {"device"}}, "HX-Request", "true")
	if !strings.Contains(failed, `role="alert"`) || !strings.Contains(failed, "did not answer in time") {
		t.Errorf("a failed switch must say why in place:\n%s", failed)
	}
}

// Why: telemetry may arrive every few seconds; one refresh reads the policy report exactly once
// (it is the dearest part of the page: all rules over all devices, from stored values) and the
// htmx throttle bounds refreshes to one per 2 s.
func TestLiveRefreshReadsThePolicyReportOnce(t *testing.T) {
	fp := &fakePolicy{rep: PolicyReport{Rules: []policy.Rule{{Command: "TelePeriod", Want: "300"}}}}
	e := setupWith(t, func(d *Deps) { d.Policy = fp })
	e.do(t, "GET", "/devices/1/live", "operator", nil)
	if len(fp.live) != 1 || fp.live[0] {
		t.Fatalf("report calls %v: one, stored values only (live=false)", fp.live)
	}
}

// readStream opens the event stream, publishes ev until the stream has sent until (when ev has a
// topic), and returns what it read. since is the page's sequence number.
func readStream(t *testing.T, e *env, since string, ev *events.Event, until string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/tf/events?since="+since, nil)
	req.Header.Set("X-Test-Role", "operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ev != nil {
		go func() {
			for ctx.Err() == nil {
				e.hub.Publish(*ev)
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	rd := bufio.NewReader(resp.Body)
	var got string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !strings.Contains(got, until) {
			line, err := rd.ReadString('\n')
			got += line
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
	}
	return got
}

// Why (#126): a state change of device 1 reaches only listeners of device 1; a job of device 1 too.
func TestStreamNamesDeviceEventsWithTheDeviceID(t *testing.T) {
	e := setup(t)
	hl := model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now(), Power: map[string]string{"POWER": "ON"}}
	got := readStream(t, e, "999999999", &events.Event{Topic: events.TopicHealth, Payload: hl}, "devstate-1")
	if !strings.Contains(got, "event: devstate-1\n") || strings.Contains(got, "devstate-2") {
		t.Errorf("health change:\n%s", got)
	}
	got = readStream(t, e, "999999999", &events.Event{Topic: events.TopicJob, Payload: model.Job{ID: 5, DeviceID: 1}}, "devjob-1")
	if !strings.Contains(got, "event: devjob-1\n") || strings.Contains(got, "devjob-2") {
		t.Errorf("job:\n%s", got)
	}
}

// Why (#160): the page is built from data older than the stream. A stream opened for a page
// built before events were published must say "resync" at once, or the page keeps showing the
// state of render time (a device Online that went Offline in between) until another event for
// that device arrives. A page that is up to date gets no needless refresh.
func TestStreamResyncsWhenEventsCameBetweenRenderAndConnect(t *testing.T) {
	e := setup(t)
	stale := "0"
	e.hub.Publish(events.Event{Topic: events.TopicBackup}) // seq is now > 0: a change after a page built at 0
	if got := readStream(t, e, stale, nil, "event: resync"); !strings.Contains(got, "event: resync\n") {
		t.Fatalf("a stale page gets no resync:\n%q", got)
	}
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(body, `sse-connect="/tf/events?since=`) {
		t.Fatal("the page does not hand its sequence number to the stream")
	}
	current := strconvSeq(e.hub.Seq())
	if got := readStream(t, e, current, nil, "event: resync"); strings.Contains(got, "event: resync") {
		t.Fatalf("an up-to-date page must not be told to refresh:\n%q", got)
	}
}

func strconvSeq(n uint64) string { return itoa(int(n)) }
