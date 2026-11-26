package web

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/health"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/stability"
)

// #1112: the overview said every fact in every card that could reach it (Wi-Fi signal in the tile, in Stability and in
// Connection; Last restart in the Uptime tile, in Stability and in Recent restarts), put Tasmota's field names into
// labels and showed "Unknown" as if it were a value. The tests below read the page the way a person does (tags and
// attributes gone) and pin where each fact lives now.

// healthyWifiDevice is device 1 as a person sees a fine one: online, a good signal sampled for hours, one restart
// a day ago (the first row stored: no run before it is proven, so it counts, #1308), its network facts read except
// the Wi-Fi channel (Tasmota does not report it on every build), and a Status 2 read. Its address differs from its IP, so the page has an "IP address" row.
func healthyWifiDevice(t *testing.T) *env {
	t.Helper()
	now := time.Now()
	var samples []model.WifiSample
	for i := 1; i <= 6; i++ {
		samples = append(samples, model.WifiSample{DeviceID: 1, At: now.Add(-time.Duration(i) * time.Hour), DBm: -50 - i})
	}
	st := fakeStab{
		boots:   map[model.DeviceID][]model.DeviceBoot{1: {{DeviceID: 1, At: now.Add(-26 * time.Hour), BootCount: 4, Reason: "Software/System restart"}}},
		samples: map[model.DeviceID][]model.WifiSample{1: samples},
	}
	e := setupWith(t, func(d *Deps) { d.Stability = st })
	d := e.inv.devs[1]
	d.Net = model.NetFacts{Hostname: "kitchen-1234", IP: "10.0.0.77", Gateway: "10.0.0.254", SSID: "IoT", BSSID: "8C:30:66:01:02:03", ReadAt: now}
	d.MAC, d.Topic = "AA:BB:CC:DD:EE:01", "tasmota_kitchen"
	d.Identity.FWR = model.FWRFacts{Version: "16.0.0(tasmota)", BuildDateTime: "2026-10-16T18:34:59", Core: "2_7_8", SDK: "2.2.2-dev", CPUFrequency: 160, CR: "445/699"}
	d.Identity.Hardware, d.Identity.FlashSizeKB = "ESP8266EX", 4096
	e.inv.devs[1] = d
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: now, SignalDBm: -56, RSSI: 88, LinkCount: 1, UptimeSec: 3700}
	return e
}

func sectionOf(body, open string) string {
	return between(body, open, "</section>")
}

// Why (A2-007): one fact has one owner. "Last restart" is the Uptime tile's; Stability keeps only restart numbers. The
// current signal is the Wi-Fi tile's and the 24 h history the Wi-Fi card's, so Stability repeats neither, and the
// address TasmoFleet uses sits in the Connection card, not in the card about the medium. A page where a fact is said
// in a third place is a page where the three drift apart.
func TestOverviewNamesEachFactOnce(t *testing.T) {
	e := healthyWifiDevice(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	text := visibleText(body)
	if n := strings.Count(text, "Last restart"); n != 1 {
		t.Errorf("\"Last restart\" appears %d times, want once (the Uptime tile)", n)
	}
	if n := strings.Count(text, "Wi-Fi signal"); n > 2 {
		t.Errorf("\"Wi-Fi signal\" appears %d times, want at most twice", n)
	}
	stab := visibleText(sectionOf(body, `<section id="stability"`))
	for _, bad := range []string{"Wi-Fi signal", "Last restart", "Wi-Fi reconnects", "dBm"} {
		if strings.Contains(stab, bad) {
			t.Errorf("Stability repeats %q, which the tile or the Wi-Fi card says:\n%s", bad, stab)
		}
	}
	wifi := visibleText(sectionOf(body, `<section id="devconn"`))
	if !strings.Contains(wifi, "-51") || !strings.Contains(wifi, "-56 dBm") && !strings.Contains(wifi, "-56") {
		t.Errorf("the 24 hour signal range moved out of Stability but is not in the Wi-Fi card:\n%s", wifi)
	}
	if strings.Contains(wifi, "Address") {
		t.Errorf("the Wi-Fi card still holds the address TasmoFleet uses:\n%s", wifi)
	}
	paths := visibleText(sectionOf(body, `<section id="devpaths"`))
	for _, want := range []string{"Address 10.0.0.1", "IP address 10.0.0.77"} {
		if !strings.Contains(paths, want) {
			t.Errorf("the Connection card lacks %q:\n%s", want, paths)
		}
	}
}

// Why (A2-009): "1 in 24 h · 1 in 7 days" reads like a ratio and "Wi-Fi reconnects 0 since boot, no Wi-Fi downtime"
// names an event boundary the person does not know. The numbers say their window in words, and a quiet device says
// "none".
func TestStabilityRowsSayTheirWindowInWords(t *testing.T) {
	e := healthyWifiDevice(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	stab := visibleText(sectionOf(body, `<section id="stability"`))
	for _, want := range []string{"Restarts None in the last 24 hours, once in the last 7 days", "Recent restarts"} {
		if !strings.Contains(stab, want) {
			t.Errorf("Stability lacks %q:\n%s", want, stab)
		}
	}
	wifi := visibleText(sectionOf(body, `<section id="devconn"`))
	if !strings.Contains(wifi, "Wi-Fi drops None since the last restart") {
		t.Errorf("the Wi-Fi card lacks the drops in words:\n%s", wifi)
	}
	for _, bad := range []string{"LinkCount", "Downtime", "BootCount", "since boot"} {
		if strings.Contains(body, bad) {
			t.Errorf("Tasmota's own word %q is on the page", bad)
		}
	}
}

// Why (A2-008): the Status 2 block listed Tasmota's field names ("BuildDateTime", "CpuFrequency", "CR 445/699") under
// a heading that says "Status 2 · StatusFWR". People who look for those names find them, secondary, behind the plain
// label; everyone else sees a closed fold and no jargon, and the page is 275 px shorter.
func TestFirmwareDetailsFoldIsClosedAndPlain(t *testing.T) {
	e := healthyWifiDevice(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	card := sectionOf(body, `<section id="identity"`)
	m := regexp.MustCompile(`<details class="fold"([^>]*)>\s*<summary>Firmware details</summary>`).FindStringSubmatch(card)
	if m == nil {
		t.Fatalf("the Identity card has no fold titled Firmware details:\n%s", shortDump(card))
	}
	if strings.Contains(m[1], "open") {
		t.Error("the fold starts open: the page is as tall as before")
	}
	fold := card[strings.Index(card, "Firmware details"):]
	text := visibleText(fold)
	for _, want := range []string{"Build date", "CPU speed", "160 MHz", "Settings memory used", "445 of 699", "Arduino core", "Espressif SDK", "BuildDateTime", "Variant", "Flash size"} {
		if !strings.Contains(text, want) {
			t.Errorf("the fold lacks %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"Status 2", "StatusFWR", "worked out by TasmoFleet", "Reported by the device"} {
		if strings.Contains(visibleText(card), bad) {
			t.Errorf("the Identity card still says %q", bad)
		}
	}
	if !strings.Contains(card, `id="fwr-info-BuildDateTime" popover`) {
		t.Error("the explanation of a field must stay reachable by its Tasmota name (the popover id)")
	}
}

// Why (A2-018): "Unknown" is the status word of a device that never answered. As a value of a row it reads like a
// fault, so an empty fact is a dash, and a Wi-Fi channel the device does not report is not a row at all.
func TestEmptyValuesShowADashAndNoUnknownChannel(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, id := range []string{`<section id="devconn"`, `<section id="identity"`} {
		sec := visibleText(sectionOf(body, id))
		if strings.Contains(sec, "Unknown") {
			t.Errorf("%s shows Unknown as a value:\n%s", id, sec)
		}
	}
	conn := visibleText(sectionOf(body, `<section id="devconn"`))
	if !strings.Contains(conn, "Gateway —") || !strings.Contains(conn, "SSID —") {
		t.Errorf("an unread fact is not a dash:\n%s", conn)
	}
	if strings.Contains(conn, "Channel") {
		t.Errorf("a channel nobody reported is a row:\n%s", conn)
	}
	_, body = e.do(t, "GET", "/devices/2", "admin", nil)
	if !strings.Contains(visibleText(sectionOf(body, `<div class="itiles"`)), "Unknown") {
		t.Error("the never-answered device must keep Unknown as its status word")
	}

	h := healthyWifiDevice(t)
	_, body = h.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(visibleText(sectionOf(body, `<section id="devconn"`)), "Channel") {
		t.Error("a healthy device that reports no channel shows a Channel row")
	}
}

// Why (decision 14): the medium card is named by its medium, the card with the paths is "Connection", and neither
// repeats the other's rows.
func TestConnectionCardsAreNamedByWhatTheyHold(t *testing.T) {
	e := healthyWifiDevice(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, want := range []string{`<h2 id="dc-conn">Wi-Fi</h2>`, `<h2 id="dc-paths">Connection</h2>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{"How TasmoFleet reaches this device", "Connection · Wi-Fi", "Connection: HTTP"} {
		if strings.Contains(body, bad) {
			t.Errorf("the page still says %q", bad)
		}
	}
	d := e.inv.devs[1]
	d.Net.Wired = true
	d.Net.Subnet, d.Net.DNS = "255.255.255.0", "10.0.0.254"
	e.inv.devs[1] = d
	_, body = e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, `<h2 id="dc-conn">Ethernet</h2>`) {
		t.Error("a wired device's medium card is Ethernet")
	}
}

// Why (A2-014): the relay is named as its person named it; "POWER" is Tasmota's command, a tooltip at most, and the
// header does not announce what the device lacks ("no energy meter").
func TestRelayRowNamesTheRelayOnly(t *testing.T) {
	e := healthyWifiDevice(t)
	h := e.health[1]
	h.Power = map[string]string{"POWER": "ON"}
	e.health[1] = h
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	box := sectionOf(body, `<section class="devcard dev-box"`)
	if strings.Contains(visibleText(box), "POWER") || strings.Contains(visibleText(box), "no energy meter") {
		t.Errorf("the relay box shows the command or a missing meter:\n%s", visibleText(box))
	}
	if !strings.Contains(box, `title="POWER"`) {
		t.Error("the command is not even in the tooltip")
	}
	if !strings.Contains(visibleText(box), "Relay 1") {
		t.Errorf("the relay has no name:\n%s", visibleText(box))
	}
}

// Why (A2-014): a lamp without sensors got a card titled Sensors that said it had nothing to show. A card that has
// nothing to say is not drawn; with readings it is, as before.
func TestNoSensorsCardWithoutReadings(t *testing.T) {
	e := healthyWifiDevice(t) // online, no relay, no telemetry readings
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(body, `id="dc-sensors"`) || strings.Contains(visibleText(body), "Sensor readings come from") {
		t.Error("an empty Sensors card is drawn")
	}
	if box := sensorBoxOf(overviewIn{Health: model.Health{State: model.HealthOnline}, Tele: health.Telemetry{}}); box != nil {
		t.Errorf("sensorBoxOf without readings: %+v", box)
	}
}

// Why (A2-042): the sentences of the overview live in deviceoverview_copy.go, so the words of a note are reviewed as a
// set and cannot drift between the template and the test. The two notes keep their wording; only its place moved.
func TestDisabledReasonLeadsComeFromTheCopyFile(t *testing.T) {
	var b bytes.Buffer
	card := pathCard{Facts: []pathRow{{Key: "MQTT password", Value: "Set", PWActs: &pwActs{Test: true, TestLabel: "Test", TestWhy: "the device is offline"}}}}
	if err := pathList("devpaths-facts", card.Facts, card, nil).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got := visibleText(b.String()); !strings.Contains(got, connTestWhyLead+"The device is offline.") || connTestWhyLead != "Test not possible: " {
		t.Errorf("password test reason: %s", got)
	}
	b.Reset()
	l := liveView{ID: 1, Box: &relayBox{Rows: []relayRow{{N: 1, Name: "Lamp", Cmd: "POWER", Line: "On", Action: "OFF"}}, Why: "the device is offline"}}
	if err := relayBoxView(l).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got := visibleText(b.String()); !strings.Contains(got, relaysWhyLead+"the device is offline") || relaysWhyLead != "Switching is disabled: " {
		t.Errorf("relay reason: %s", got)
	}
}

// Why (A2-009): a boot loop's numbers need no lecture ("a healthy device stays up for days", "counted from BootCount:
// includes restarts TasmoFleet did not see"); the notice above them already says what to do.
func TestBootLoopRowsHaveNoLectureLines(t *testing.T) {
	e := stabFleet(t)
	_, body := e.do(t, "GET", "/devices/7", "admin", nil)
	card := visibleText(sectionOf(body, `<section id="stability"`))
	for _, bad := range []string{"healthy device stays up", "BootCount", "TasmoFleet did not see"} {
		if strings.Contains(card, bad) {
			t.Errorf("the boot loop card lectures: %q", bad)
		}
	}
	for _, want := range []string{"Restarts 5 times in the last hour", "Longest uptime 9 min in the last 24 hours"} {
		if !strings.Contains(card, want) {
			t.Errorf("the boot loop card lacks %q:\n%s", want, card)
		}
	}
}

// The restart words as a pure function, edge by edge: the windows are rolling, so "today" would be false for a restart
// twenty hours ago.
func TestRestartCountWordsFollowTheRollingWindows(t *testing.T) {
	for _, c := range []struct {
		r    stability.Result
		want string
	}{
		{stability.Result{}, "None in the last 7 days"},
		{stability.Result{Restarts24h: 1, Restarts7d: 1}, "Once in the last 24 hours, once in the last 7 days"},
		{stability.Result{Restarts24h: 0, Restarts7d: 3}, "None in the last 24 hours, 3 times in the last 7 days"},
		{stability.Result{Restarts1h: 5, Restarts24h: 6, Restarts7d: 6}, "5 times in the last hour, 6 times in the last 24 hours, 6 times in the last 7 days"},
	} {
		if got := restartCountWords(c.r); got != c.want {
			t.Errorf("%+v: %q, want %q", c.r, got, c.want)
		}
	}
}
