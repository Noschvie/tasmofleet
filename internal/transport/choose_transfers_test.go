package transport

import (
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#607, the product owner's transport rule of 2026-11-14): backups and restores go over MQTT first whenever the
// device is usable there for a transfer (the strict rule of writes plus a usable MQTT password), like every other
// write; HTTP carries them only when MQTT cannot. A config dump is not better protected over HTTP: Tasmota serves
// plain HTTP and the request carries the web password in clear. The same two exceptions as for writes keep HTTP while
// it works: MQTT commands of the device just went unanswered (a transfer would only time out), or its MQTT connection
// was just changed (it may not be back on the broker). The sentence is what the device page shows for "Back up" and
// "Restore", so it must say what the router does and, when it is HTTP, why MQTT could not take the transfer.
func TestTransfersGoOverMQTTFirstWhileHTTPWorks(t *testing.T) {
	cases := []struct {
		name            string
		mod             func(*Facts)
		backup, restore Path
		note            string // substring of both reasons
	}{
		{"usable, HTTP ok: MQTT", func(f *Facts) {}, MQTT, MQTT, "over MQTT first"},
		{"usable, HTTP unknown: MQTT", func(f *Facts) { f.HTTP = HTTPState{} }, MQTT, MQTT, "over MQTT first"},
		{"usable, HTTP failing: MQTT, and says why", func(f *Facts) { *f = httpDown(*f) }, MQTT, MQTT, "cannot reach the device over HTTP"},
		{"no MQTT password, HTTP ok: HTTP, and names the fix", func(f *Facts) { f.Password = Password{} }, HTTP, HTTP, "enter the device's MQTT password"},
		{"password refused, HTTP ok: HTTP", func(f *Facts) { f.Password.BadAt = t0.Add(-time.Hour) }, HTTP, HTTP, "refused its MQTT password"},
		{"broker mode without a broker password: HTTP", func(f *Facts) {
			f.Password = Password{Mode: model.MQTTPasswordBroker, Unusable: "no broker password is set"}
		}, HTTP, HTTP, "no broker password"},
		{"own GroupTopic unread, HTTP ok: HTTP", func(f *Facts) { f.MQTT.GroupTopic = "" }, HTTP, HTTP, "GroupTopic has not been read recently"},
		{"shared topic, HTTP ok: HTTP", func(f *Facts) { f.MQTT.TopicExclusive = false }, HTTP, HTTP, "another device listens"},
		{"MQTT commands unanswered, HTTP ok: HTTP", func(f *Facts) { f.MQTT.ReadsUnansweredSince = t0.Add(-time.Minute) }, HTTP, HTTP, "got no answer"},
		{"MQTT connection just changed, HTTP ok: HTTP", func(f *Facts) { f.MQTT.ConnChangedSince = t0.Add(-time.Minute) }, HTTP, HTTP, "may not be back on the broker"},
		{"MQTT connection just changed, HTTP failing: MQTT", func(f *Facts) { *f = httpDown(*f); f.MQTT.ConnChangedSince = t0.Add(-time.Minute) }, MQTT, MQTT, "cannot reach the device over HTTP"},
		{"no password, HTTP failing: none", func(f *Facts) { *f = httpDown(*f); f.Password = Password{} }, None, None, "no path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := base()
			c.mod(&f)
			p := Choose(f)
			if p.Backup != c.backup || p.Restore != c.restore {
				t.Fatalf("backup=%s restore=%s, want %s %s\nreasons: %v", p.Backup, p.Restore, c.backup, c.restore, p.Reasons)
			}
			for _, a := range []Action{ActBackup, ActRestore} {
				if got := p.Reason(a); !strings.Contains(got, c.note) {
					t.Errorf("%s reason %q lacks %q", a, got, c.note)
				}
			}
		})
	}
}

// Why (#607): the device page shows these sentences under "Back up" and "Restore". Over MQTT first they say that
// the files archive still needs HTTP (Tasmota has no file transfer over MQTT) and that HTTP takes a transfer only
// when MQTT published nothing; over HTTP they say why MQTT could not take it, so a person sees what to fix.
func TestTransferReasonsSayWhatTheRouterDoes(t *testing.T) {
	p := Choose(base())
	for a, want := range map[Action][]string{
		ActBackup:  {"over MQTT first (FileDownload", "files archive over HTTP", "over HTTP if nothing could be published"},
		ActRestore: {"over MQTT first (FileUpload", "files over HTTP", "MQTT connection"},
	} {
		for _, w := range want {
			if got := p.Reason(a); !strings.Contains(got, w) {
				t.Errorf("%s reason %q lacks %q", a, got, w)
			}
		}
	}
	f := base()
	f.Password = Password{}
	for a, want := range map[Action]string{
		ActBackup:  "over the device's HTTP API (/dl), because a backup over MQTT is not possible: enter the device's MQTT password to back up over MQTT",
		ActRestore: "over the device's HTTP API (restore upload), because a restore over MQTT is not possible: enter the device's MQTT password to restore over MQTT",
	} {
		if got := Choose(f).Reason(a); got != want {
			t.Errorf("%s reason %q, want %q", a, got, want)
		}
	}
}
