package transport

import (
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#608, the product owner's transport rule of 2026-11-14): OtaUrl and Upgrade 1 are writes, so they go over MQTT
// first whenever the device is usable there under the strict rule of writes, also while HTTP works; the device still
// fetches the image from TasmoFleet's firmware server over HTTP (DownloadMQTT). The update takes the same rule and the
// same two exceptions as every other write (MQTT commands unanswered, the MQTT connection just changed): a topic that
// might be shared would flash the wrong devices. Commands need no MqttPassword. Push uploads stay HTTP while HTTP
// works (coordinator decision of 2026-11-17).
func TestTheUpdateCommandsGoOverMQTTFirstWhileHTTPWorks(t *testing.T) {
	for _, c := range []struct {
		name string
		f    func(Facts) Facts
		want Path
	}{
		{"usable on our broker, HTTP answers", func(f Facts) Facts { return f }, DownloadMQTT},
		{"usable on our broker, HTTP unknown", func(f Facts) Facts { f.HTTP = HTTPState{}; return f }, DownloadMQTT},
		{"no MQTT password: commands need none", func(f Facts) Facts { f.Password = Password{}; return f }, DownloadMQTT},
		{"MQTT password refused: commands need none", func(f Facts) Facts { f.Password.BadAt = t0.Add(-time.Minute); return f }, DownloadMQTT},
		{"MQTT connection just changed", func(f Facts) Facts { f.MQTT.ConnChangedSince = t0.Add(-time.Minute); return f }, DownloadHTTP},
		{"MQTT commands unanswered", func(f Facts) Facts { f.MQTT.ReadsUnansweredSince = t0.Add(-time.Minute); return f }, DownloadHTTP},
		{"another device answers on the topic", func(f Facts) Facts { f.MQTT.TopicTakenSince = t0.Add(-time.Minute); return f }, DownloadHTTP},
		{"own group topic unread", func(f Facts) Facts { f.MQTT.GroupTopicReadAt = time.Time{}; return f }, DownloadHTTP},
		{"another device's group topic unread", func(f Facts) Facts { f.MQTT.OthersGroupUnread = []string{"porch"}; return f }, DownloadHTTP},
		{"topic not exclusive", func(f Facts) Facts { f.MQTT.TopicExclusive = false; return f }, DownloadHTTP},
		{"last will offline", func(f Facts) Facts { f.MQTT.LWTOnline = false; return f }, DownloadHTTP},
		{"another broker", func(f Facts) Facts { f.MQTT.Ours = false; return f }, DownloadHTTP},
		{"bus down", func(f Facts) Facts { f.MQTT.BusConnected = false; return f }, DownloadHTTP},
		{"firmware server unreachable", func(f Facts) Facts {
			f.FWReach = model.FWReach{State: model.FWReachNo, At: t0.Add(-time.Hour)}
			return f
		}, None},
		{"no FIRMWARE_BASE_URL", func(f Facts) Facts { f.FirmwareBaseURL = false; return f }, None},
		{"push upload over HTTP stays", func(f Facts) Facts { return push(f, PushHTTP, false) }, PushHTTP},
		{"push into safeboot stays", func(f Facts) Facts { return push(f, PushSafeboot, false) }, PushSafeboot},
		{"a push that fell back to the download", func(f Facts) Facts { return push(f, DownloadHTTP, false) }, DownloadMQTT},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := Choose(c.f(base()))
			if p.Update != c.want {
				t.Fatalf("update = %s, want %s: %s", p.Update, c.want, p.Reason(ActUpdate))
			}
		})
	}
}

// Why (#608): the update's path is the one the job pins for its whole life, so the sentence a person reads in the
// preview and on the device page must say which leg goes where: the commands over MQTT, the image over HTTP from
// TasmoFleet. When the commands stay on HTTP although the device is on our broker, it says why, as writes do. A push
// that fell back to the download keeps its note.
func TestTheUpdateReasonNamesBothLegs(t *testing.T) {
	p := Choose(base())
	r := p.Reason(ActUpdate)
	if !strings.Contains(r, "OtaUrl and Upgrade go to the device's own topic on TasmoFleet's broker") || !strings.Contains(r, "downloads the firmware from TasmoFleet over HTTP") {
		t.Errorf("MQTT reason %q", r)
	}
	f := push(base(), DownloadHTTP, false)
	f.Request.HTTPPushNote = "not enough free flash for an upload"
	if r := Choose(f).Reason(ActUpdate); !strings.Contains(r, "OtaUrl and Upgrade go to the device's own topic on TasmoFleet's broker") || !strings.Contains(r, "not enough free flash for an upload") {
		t.Errorf("fallback reason %q", r)
	}
	for name, mod := range map[string]func(*Facts) string{
		"blocked": func(f *Facts) string {
			f.MQTT.OthersGroupUnread = []string{"porch"}
			return "the group topic of porch has not been read yet"
		},
		"unanswered": func(f *Facts) string {
			f.MQTT.ReadsUnansweredSince = t0.Add(-time.Minute)
			return "got no answer from the device"
		},
		"conn changed": func(f *Facts) string {
			f.MQTT.ConnChangedSince = t0.Add(-time.Minute)
			return "changed the device's MQTT connection"
		},
	} {
		f := base()
		want := mod(&f)
		r := Choose(f).Reason(ActUpdate)
		if !strings.Contains(r, "OtaUrl and Upgrade over the device's HTTP API") || !strings.Contains(r, want) ||
			!strings.Contains(r, "downloads the firmware from TasmoFleet over HTTP") {
			t.Errorf("%s: HTTP reason %q", name, r)
		}
	}
}
