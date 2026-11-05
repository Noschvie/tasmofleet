package transport

import (
	"strings"
	"testing"
	"time"
)

// Why (#456 A): a device on TasmoFleet's broker that is online answers reads over MQTT, so a device
// HTTP cannot reach (or a tile that has not been polled lately) never decides the read path. Writes
// keep their order (HTTP unless failing) until stage 2.
func TestReadsGoOverMQTTFirst(t *testing.T) {
	cases := []struct {
		name  string
		mod   func(*Facts)
		reads Path
		write Path
		note  string // substring of the reads reason
	}{
		{"on our broker and live, HTTP ok: MQTT", func(f *Facts) {}, MQTT, HTTP, "MQTT first"},
		{"on our broker and live, HTTP unknown: MQTT", func(f *Facts) { f.HTTP = HTTPState{} }, MQTT, HTTP, "MQTT first"},
		{"on our broker and live, HTTP failing: MQTT, and says why", func(f *Facts) { *f = httpDown(*f) }, MQTT, MQTT, "cannot reach the device over HTTP"},
		// Why (review of PR 626): a device whose MQTT reads went unanswered is read over HTTP while HTTP
		// works; when HTTP fails, MQTT is all there is.
		{"MQTT reads unanswered, HTTP ok: HTTP", func(f *Facts) { f.MQTT.ReadsUnansweredSince = t0.Add(-time.Minute) }, HTTP, HTTP, "got no answer"},
		{"MQTT reads unanswered, HTTP unknown: HTTP", func(f *Facts) { f.HTTP = HTTPState{}; f.MQTT.ReadsUnansweredSince = t0.Add(-time.Minute) }, HTTP, HTTP, "got no answer"},
		{"MQTT reads unanswered, HTTP failing: MQTT", func(f *Facts) { *f = httpDown(*f); f.MQTT.ReadsUnansweredSince = t0.Add(-time.Minute) }, MQTT, MQTT, "cannot reach the device over HTTP"},
		{"LWT not online: HTTP (a publish would vanish)", func(f *Facts) { f.MQTT.LWTOnline = false }, HTTP, HTTP, "HTTP"},
		{"other broker: HTTP", func(f *Facts) { f.MQTT.Ours = false }, HTTP, HTTP, "HTTP"},
		{"MQTT off for the device (no topic): HTTP", func(f *Facts) { f.MQTT.Topic = "" }, HTTP, HTTP, "HTTP"},
		{"bus down: HTTP", func(f *Facts) { f.MQTT.BusConnected = false }, HTTP, HTTP, "HTTP"},
		{"shared topic: HTTP", func(f *Facts) { f.MQTT.TopicExclusive = false }, HTTP, HTTP, "HTTP"},
		{"foreign FullTopic: HTTP", func(f *Facts) { f.MQTT.FullTopic = "x/%topic%/%prefix%/" }, HTTP, HTTP, "HTTP"},
		// Why: a read needs no known GroupTopic (the channel refuses known group topics itself), and a
		// device whose Status 1 was never read must still be readable so that Status 1 can be read at all.
		{"GroupTopic unread: reads still MQTT, writes HTTP", func(f *Facts) { f.MQTT.GroupTopic = "" }, MQTT, HTTP, "MQTT first"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := base()
			c.mod(&f)
			p := Choose(f)
			if p.Commands != c.reads || p.Writes != c.write {
				t.Fatalf("reads=%s writes=%s, want %s/%s\nreasons: %v", p.Commands, p.Writes, c.reads, c.write, p.Reasons)
			}
			if got := p.Reason(ActCommands); !strings.Contains(got, c.note) {
				t.Errorf("reads reason %q lacks %q", got, c.note)
			}
		})
	}
}

// Why: MQTTNone is a device with MQTT switched off; it can only be read over HTTP, and when HTTP
// fails too there is nothing to try (the reason names both).
func TestReadsOfADeviceWithoutMQTT(t *testing.T) {
	f := base()
	f.MQTT = MQTTFacts{BusConnected: true}
	if p := Choose(f); p.Commands != HTTP {
		t.Fatalf("reads = %s", p.Commands)
	}
	f = httpDown(f)
	p := Choose(f)
	if p.Commands != None || !strings.Contains(p.Reason(ActCommands), "no path") {
		t.Fatalf("reads = %s (%q)", p.Commands, p.Reason(ActCommands))
	}
}

// Why: the connection card shows this sentence under "Console and Settings: reading"; it must say
// what the router does (MQTT first, HTTP when MQTT is not usable or nothing could be published),
// not the old "HTTP first" order.
func TestReadsReasonSaysWhatTheRouterDoes(t *testing.T) {
	got := Choose(base()).Reason(ActCommands)
	for _, want := range []string{"over MQTT first", "online on TasmoFleet's broker", "over HTTP once if the device does not answer"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason %q lacks %q", got, want)
		}
	}
	other := base()
	other.MQTT.Ours = false
	got = Choose(other).Reason(ActCommands)
	if !strings.Contains(got, "over the device's HTTP API") || !strings.Contains(got, "does not use TasmoFleet's MQTT broker") {
		t.Errorf("HTTP reason %q does not say why MQTT was not used", got)
	}
}
