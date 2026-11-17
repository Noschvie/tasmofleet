package transport

import (
	"strings"
	"testing"
	"time"
)

// Why (#605): a device usable on our broker under the strict rule (its own GroupTopic read, no other device's
// unread, topic exclusive, FullTopic ours, last will Online) takes changes over MQTT first, so a device HTTP
// struggles with is not loaded with an HTTP request per change. A device whose topic might be shared, or whose
// MQTT reads just went unanswered, keeps HTTP while it works; a message to a shared topic would change other
// devices too, and a write that is never answered is an unknown outcome.
func TestWritesGoOverMQTTFirstUnderTheStrictRule(t *testing.T) {
	cases := []struct {
		name  string
		mod   func(*Facts)
		write Path
		note  string // substring of the writes reason
	}{
		{"strict usable, HTTP ok: MQTT", func(f *Facts) {}, MQTT, "over MQTT first"},
		{"strict usable, HTTP unknown: MQTT", func(f *Facts) { f.HTTP = HTTPState{} }, MQTT, "over MQTT first"},
		{"strict usable, HTTP failing: MQTT, and says why", func(f *Facts) { *f = httpDown(*f) }, MQTT, "cannot reach the device over HTTP"},
		{"own GroupTopic unread, HTTP ok: HTTP, and says why", func(f *Facts) { f.MQTT.GroupTopic = "" }, HTTP, "GroupTopic has not been read recently"},
		{"own GroupTopic older than the last probe: HTTP", func(f *Facts) { f.MQTT.LastProbeAt = f.MQTT.GroupTopicReadAt.Add(time.Minute) }, HTTP, "GroupTopic"},
		{"another device's GroupTopic unread: HTTP", func(f *Facts) { f.MQTT.OthersGroupUnread = []string{"garage"} }, HTTP, "the group topic of garage has not been read yet"},
		{"MQTT reads unanswered, HTTP ok: HTTP", func(f *Facts) { f.MQTT.ReadsUnansweredSince = t0.Add(-time.Minute) }, HTTP, "got no answer"},
		{"MQTT reads unanswered, HTTP failing: MQTT", func(f *Facts) { *f = httpDown(*f); f.MQTT.ReadsUnansweredSince = t0.Add(-time.Minute) }, MQTT, "cannot reach the device over HTTP"},
		// Why (#605 review, round 4; "HTTP stays the fallback for the next commands"): after TasmoFleet changed the
		// device's MQTT connection over MQTT, the device may not rejoin the broker while its last will stays Online, so
		// its next changes, the person's corrective one included, go over HTTP while HTTP works.
		{"MQTT connection just changed, HTTP ok: HTTP, and says why", func(f *Facts) { f.MQTT.ConnChangedSince = t0.Add(-time.Minute) }, HTTP, "changed the device's MQTT connection"},
		{"MQTT connection just changed, HTTP unknown: HTTP", func(f *Facts) { f.HTTP = HTTPState{}; f.MQTT.ConnChangedSince = t0.Add(-time.Minute) }, HTTP, "may not be back on the broker"},
		{"MQTT connection just changed, HTTP failing: MQTT", func(f *Facts) { *f = httpDown(*f); f.MQTT.ConnChangedSince = t0.Add(-time.Minute) }, MQTT, "cannot reach the device over HTTP"},
		{"LWT not online: HTTP", func(f *Facts) { f.MQTT.LWTOnline = false }, HTTP, "not online on MQTT"},
		{"other broker: HTTP", func(f *Facts) { f.MQTT.Ours = false }, HTTP, "does not use TasmoFleet's MQTT broker"},
		{"shared topic: HTTP", func(f *Facts) { f.MQTT.TopicExclusive = false }, HTTP, "another device listens"},
		{"bus down: HTTP", func(f *Facts) { f.MQTT.BusConnected = false }, HTTP, "not connected to its MQTT broker"},
		// Why: both ways fail: nothing is sent, and the sentence names both causes.
		{"GroupTopic unread, HTTP failing: none", func(f *Facts) { *f = httpDown(*f); f.MQTT.GroupTopic = "" }, None, "no path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := base()
			c.mod(&f)
			p := Choose(f)
			if p.Writes != c.write {
				t.Fatalf("writes=%s, want %s\nreasons: %v", p.Writes, c.write, p.Reasons)
			}
			if got := p.Reason(ActWrites); !strings.Contains(got, c.note) {
				t.Errorf("writes reason %q lacks %q", got, c.note)
			}
		})
	}
}

// Why (#605, the product owner's transport rule of 2026-11-14): the connection card shows this sentence under
// "Console and Settings: changing". It must say what the router does: MQTT first, HTTP only when nothing could be
// published, which is the case for the commands the channel does not carry (those that run other commands). It must
// not name passwords, Wi-Fi names, the MQTT connection or GPIO templates as changes that go over HTTP: they go over
// MQTT first like every other change, and a card saying otherwise would contradict the audit's via=mqtt. When HTTP
// is used, it says that the switches use it too and why.
func TestWritesReasonSaysWhatTheRouterDoes(t *testing.T) {
	got := Choose(base()).Reason(ActWrites)
	for _, want := range []string{"over MQTT first", "over HTTP if nothing could be published", "runs other commands"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason %q lacks %q", got, want)
		}
	}
	for _, gone := range []string{"password", "Wi-Fi", "MQTT connection", "GPIO template", "audit log hides"} {
		if strings.Contains(got, gone) {
			t.Errorf("reason %q still names %q as going over HTTP", got, gone)
		}
	}
	f := base()
	f.MQTT.GroupTopic = ""
	got = Choose(f).Reason(ActWrites)
	for _, want := range []string{"over the device's HTTP API", "switches", "refresh the device's identity"} {
		if !strings.Contains(got, want) {
			t.Errorf("HTTP reason %q lacks %q", got, want)
		}
	}
}
