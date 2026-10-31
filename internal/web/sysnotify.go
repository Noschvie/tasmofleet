package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/notify"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// NotifyService is the part of notify.Dispatcher the Notifications tab uses. Optional: without
// it the tab shows the settings but no status and no test buttons.
type NotifyService interface {
	Config() config.Config
	Status(ch notify.Channel) notify.Status
	Test(ctx context.Context, ch notify.Channel, c config.Config) (notify.Attempt, error)
}

// sysChannel is the status line of a channel: a dot, a title and one sentence around a time.
type sysChannel struct {
	Tone, Title string
	Lead, Tail  string // text before and after the time
	At          time.Time
}

// sysNotify is what the Notifications tab shows besides the settings.
type sysNotify struct {
	On             bool // a NotifyService exists: status lines and test buttons
	Webhook, Email sysChannel
	Preview        string
	WebhookResult  string // result of a test button pressed without scripts (the page was posted)
	EmailResult    string
}

// notifyChannelView words the state of a channel: not set up, or its last delivery, or its last
// test. A test is only the headline when nothing was delivered yet.
func notifyChannelView(configured bool, notConfigured string, st notify.Status) sysChannel {
	if !configured {
		return sysChannel{Tone: "neutral", Title: "Not configured", Lead: notConfigured}
	}
	a, what := st.Delivery, "delivery"
	if a.At.IsZero() {
		a, what = st.Test, "test"
	}
	switch {
	case a.At.IsZero():
		return sysChannel{Tone: "neutral", Title: "Nothing sent yet", Lead: "Send a test to check the settings."}
	case a.OK && what == "delivery":
		return sysChannel{Tone: "ok", Title: "Last delivery OK", At: a.At, Tail: " · " + a.Context + " · " + a.Detail}
	case a.OK:
		return sysChannel{Tone: "ok", Title: "Last test OK", At: a.At, Tail: " · " + a.Detail}
	case what == "delivery":
		return sysChannel{Tone: "danger", Title: "Last delivery failed", Lead: a.Detail + " · ", At: a.At, Tail: " · " + a.Context + ". The next event tries again."}
	}
	return sysChannel{Tone: "danger", Title: "Last test failed", Lead: a.Detail + " · ", At: a.At}
}

// sysNotifyOf builds the tab's extras from the saved values (the status describes what is
// stored, not what is typed).
func (h *handler) sysNotifyOf(values map[string]string) sysNotify {
	n := h.d.Notify
	if n == nil {
		return sysNotify{}
	}
	out := sysNotify{On: true, Preview: notify.WebhookJSON(notify.Sample(n.Config()), true)}
	out.Webhook = notifyChannelView(values["NOTIFY_WEBHOOK"] == "true" && values["NOTIFY_WEBHOOK_URL"] != "",
		"Turn it on and enter a URL.", n.Status(notify.Webhook))
	out.Email = notifyChannelView(values["NOTIFY_SMTP"] == "true" && values["NOTIFY_SMTP_HOST"] != "",
		"Turn it on and enter a host.", n.Status(notify.Email))
	return out
}

// notifyKeyPrefix names the settings a channel's test uses; the rest of the posted form is ignored.
var notifyKeyPrefix = map[notify.Channel]string{notify.Webhook: "NOTIFY_WEBHOOK", notify.Email: "NOTIFY_SMTP"}

// notifySecretKey is the write-only secret each channel sends.
var notifySecretKey = map[notify.Channel]string{notify.Webhook: "NOTIFY_WEBHOOK_SECRET", notify.Email: "NOTIFY_SMTP_PASSWORD"}

const (
	webhookTestHelp = "Sends a test message with the values above. Saves nothing."
	emailTestHelp   = "Sends one test e-mail. Saves nothing."
)

// sysNotifyTest sends the test event to one channel with the values of the form, saved or not.
// A secret left empty is the stored one, used here and never shown. The form posts every setting
// of every tab, so only the channel's own keys count.
func (h *handler) sysNotifyTest(w http.ResponseWriter, r *http.Request) {
	ch := notify.Channel(r.PathValue("channel"))
	prefix, known := notifyKeyPrefix[ch]
	if !known || h.d.Notify == nil {
		http.NotFound(w, r)
		return
	}
	all, clear, _, ok := h.sysForm(w, r, true, sysFormExtras...)
	if !ok {
		return
	}
	values := map[string]string{}
	for k, v := range all {
		if strings.HasPrefix(k, prefix) {
			values[k] = v
		}
	}
	for _, k := range clear { // "Reset to default" ticked: the test sees what the save would store, a removed secret included
		if strings.HasPrefix(k, prefix) {
			values[k] = config.DefaultValue(k)
		}
	}
	values[prefix] = "true" // the person pressed the button of this channel: check it as if it were on
	msg := ""
	cfg, dropped, err := h.d.SysConfig.Preview(values)
	var ve *sysconfig.ValidationError
	switch {
	case errors.As(err, &ve):
		var why []string
		for _, k := range sortedKeys(ve.Fields) {
			why = append(why, ve.Fields[k])
		}
		msg = "failed: " + strings.Join(why, "; ")
	case err != nil:
		h.fail(w, r, err)
		return
	default:
		a, terr := h.d.Notify.Test(r.Context(), ch, cfg)
		switch {
		case errors.Is(terr, notify.ErrTooSoon):
			msg = "failed: wait a few seconds between tests"
		case terr != nil:
			h.fail(w, r, terr)
			return
		case a.OK:
			msg = "sent, " + a.Detail
		default:
			msg = "failed: " + a.Detail
		}
	}
	// A stored secret goes only to the target it was entered for: say that it was left out.
	if slices.Contains(dropped, notifySecretKey[ch]) {
		msg += " · secret not sent: target changed, enter it to test"
	}
	what, help := "Webhook test", webhookTestHelp
	if ch == notify.Email {
		what, help = "E-mail test", emailTestHelp
	}
	if r.Header.Get("HX-Request") != "" {
		h.component(w, r, http.StatusOK, sysTestRow("Send test", httpBase(r)+sysPath+"/notify-test/"+string(ch)+"?tab=notifications", what, msg, help))
		return
	}
	p := h.sysData(r, values, nil)
	p.Active = "notifications"
	if ch == notify.Email {
		p.Notify.EmailResult = msg
	} else {
		p.Notify.WebhookResult = msg
	}
	h.sysRender(w, r, http.StatusOK, p)
}

// sysEnumLabel is a choice of a notification setting as the page words it.
func sysEnumLabel(key, value string) string {
	labels := map[string]map[string]string{
		"NOTIFY_UPDATE_WHEN":    {config.NotifyWhenAny: "Any result", config.NotifyWhenProblems: "Failed or partial only"},
		"NOTIFY_WEBHOOK_FORMAT": {config.NotifyFormatJSON: "Generic JSON", config.NotifyFormatHomeAssistant: "Home Assistant"},
		"NOTIFY_WEBHOOK_AUTH":   {config.NotifyAuthNone: "None", config.NotifyAuthHeader: "Secret header"},
		"NOTIFY_SMTP_SECURITY":  {config.NotifySecStartTLS: "STARTTLS", config.NotifySecTLS: "TLS", config.NotifySecNone: "None"},
	}
	if l, ok := labels[key][value]; ok {
		return l
	}
	return value
}

// sysSegmentedEnum says whether a choice is drawn as segments (two or three short choices).
func sysSegmentedEnum(key string) bool {
	return key == "NOTIFY_UPDATE_WHEN" || key == "NOTIFY_WEBHOOK_FORMAT" || key == "NOTIFY_WEBHOOK_AUTH"
}

// sysEnumOptions are the segments of a choice setting.
func sysEnumOptions(row sysRow) []ui.Option {
	out := make([]ui.Option, 0, len(row.Enum))
	for _, v := range row.Enum {
		out = append(out, ui.Option{Value: v, Label: sysEnumLabel(row.Key, v), Checked: v == row.Posted})
	}
	return out
}

// sysSecretMax is the longest secret the field takes: a bearer token is longer than a password.
func sysSecretMax(row sysRow) string {
	if strings.HasPrefix(row.Key, "NOTIFY_") {
		return "512"
	}
	return "128"
}

// sysRowOf finds the row of a setting among a card's rows and the rows hung under them.
func sysRowOf(rows []sysRow, key string) sysRow {
	if r := sysFindRow(rows, key); r != nil {
		return *r
	}
	return sysRow{}
}

// sysNoReset is a row without the "Reset to default" control: the port and security columns of
// the mail server row are too narrow for it (the page has the switch and the host for that).
func sysNoReset(r sysRow) sysRow {
	if r.Key != "NOTIFY_SMTP_HOST" {
		r.Source = sysconfig.SourceDefault
	}
	return r
}
