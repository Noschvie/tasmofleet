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
	if !strings.Contains(r, "OtaUrl and Upgrade go to the device's own topic on TasmoFleet's broker") || !strings.Contains(r, "commands over MQTT, the image over HTTP from TasmoFleet's firmware server") {
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
		if !strings.Contains(r, "the commands go over HTTP because") || !strings.Contains(r, want) ||
			!strings.Contains(r, "commands over HTTP, the image over HTTP from TasmoFleet's firmware server") {
			t.Errorf("%s: HTTP reason %q", name, r)
		}
	}
}

// Why (#987, found in the 0.41 live test): the device page said "Download over MQTT", so a person could believe the
// firmware image crosses the broker. It never does: Tasmota fetches an OTA image over HTTP. Only the commands (OtaUrl,
// Upgrade) have a transport of their own. One sentence per update path therefore names both legs, the commands'
// transport and how the image reaches the device, and is the one wording every screen and log line takes.
func TestEveryUpdatePathSentenceNamesCommandsAndImage(t *testing.T) {
	for _, c := range []struct {
		path     Path
		commands string
		image    string
	}{
		{DownloadHTTP, "commands over HTTP", "the image over HTTP from TasmoFleet's firmware server"},
		{DownloadMQTT, "commands over MQTT", "the image over HTTP from TasmoFleet's firmware server"},
		{PushHTTP, "commands over HTTP", "the image pushed by TasmoFleet over HTTP"},
		{PushSafeboot, "commands over HTTP", "the image pushed by TasmoFleet over HTTP through the safeboot partition"},
		{PushMQTT, "commands over MQTT", "the image pushed by TasmoFleet over MQTT (experimental)"},
	} {
		t.Run(string(c.path), func(t *testing.T) {
			d := c.path.Describe()
			if !strings.Contains(d, c.commands) || !strings.Contains(d, c.image) {
				t.Errorf("%q must name %q and %q", d, c.commands, c.image)
			}
			if strings.Contains(strings.ToLower(d), "download over mqtt") || strings.Contains(d, "starts the download over MQTT") {
				t.Errorf("%q says the download goes over MQTT", d)
			}
		})
	}
}

// Why (#987): the device page shows Paths.Reason and the update preview shows the same text, so one path must read the
// same everywhere, and a person must meet the route (commands, image) before the why. Every update branch of the
// chooser therefore starts with its path's sentence and only adds the reason after it; a branch that words the path
// itself would drift from the other screens again.
func TestEveryUpdateReasonStartsWithItsPathSentence(t *testing.T) {
	withNote := push(base(), DownloadHTTP, false)
	withNote.Request.HTTPPushNote = "not enough free flash for an upload"
	connChanged := base()
	connChanged.MQTT.ConnChangedSince = t0.Add(-time.Minute)
	unanswered := base()
	unanswered.MQTT.ReadsUnansweredSince = t0.Add(-time.Minute)
	groupUnread := base()
	groupUnread.MQTT.OthersGroupUnread = []string{"porch"}
	noUpload := push(httpDown(base()), PushHTTP, false)
	for _, c := range []struct {
		name string
		f    Facts
		want Path
	}{
		{"commands over MQTT while HTTP works", base(), DownloadMQTT},
		{"the same with a note why no upload", withNote, DownloadMQTT},
		{"MQTT not usable", groupUnread, DownloadHTTP},
		{"MQTT commands unanswered", unanswered, DownloadHTTP},
		{"MQTT connection changed", connChanged, DownloadHTTP},
		{"HTTP failing, MQTT carries the update", httpDown(base()), DownloadMQTT},
		{"HTTP failing, upload over MQTT not possible", noUpload, DownloadMQTT},
		{"upload over HTTP", push(base(), PushHTTP, false), PushHTTP},
		{"upload through safeboot", push(base(), PushSafeboot, false), PushSafeboot},
		{"upload over MQTT", push(httpDown(base()), PushHTTP, true), PushMQTT},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := Choose(c.f)
			if p.Update != c.want {
				t.Fatalf("update = %s, want %s: %s", p.Update, c.want, p.Reason(ActUpdate))
			}
			if r := p.Reason(ActUpdate); !strings.HasPrefix(r, c.want.Describe()) {
				t.Errorf("reason %q does not start with the path's sentence %q", r, c.want.Describe())
			}
		})
	}
}
