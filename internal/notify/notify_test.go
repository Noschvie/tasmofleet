package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/config"
)

var t0 = time.Date(2026, 10, 31, 3, 12, 7, 0, time.UTC)

func baseConfig() config.Config {
	return config.Config{NotifyUpdate: true, NotifyUpdateWhen: config.NotifyWhenAny, NotifyBackup: true, NotifySnapshot: true,
		NotifyWebhookFormat: config.NotifyFormatJSON, NotifyWebhookAuth: config.NotifyAuthNone, NotifyWebhookHeader: "Authorization",
		NotifySMTPPort: 587, NotifySMTPSecurity: config.NotifySecStartTLS, PublicURL: "http://tasmofleet.local"}
}

func newDispatcher(c *config.Config, log *bytes.Buffer) *Dispatcher {
	o := Options{Config: func() config.Config { return *c }, Now: func() time.Time { return t0 }, RetryDelay: time.Millisecond}
	o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if log != nil {
		o.Log = slog.New(slog.NewTextHandler(log, nil))
	}
	return New(o)
}

// Why: the person chose which events they want. A message for an event they switched off (or for
// a successful update when they only want problems) is noise, and the reason they would switch
// the whole thing off.
func TestOnlySelectedEventsAreSent(t *testing.T) {
	ok, partial, failed := UpdateFinished(baseConfig(), 1, 3, 0, 0, "1", t0), UpdateFinished(baseConfig(), 2, 2, 1, 0, "1", t0), UpdateFinished(baseConfig(), 3, 0, 2, 0, "1", t0)
	cancelled := UpdateFinished(baseConfig(), 4, 0, 0, 2, "1", t0)
	bk, sn := BackupFailed(baseConfig(), 5, []string{"a"}, t0), SnapshotFailed(baseConfig(), "disk full", t0)
	cases := []struct {
		name string
		mod  func(*config.Config)
		ev   Event
		want bool
	}{
		{"update any: ok", func(*config.Config) {}, ok, true},
		{"update problems: ok is not sent", func(c *config.Config) { c.NotifyUpdateWhen = config.NotifyWhenProblems }, ok, false},
		{"update problems: partial", func(c *config.Config) { c.NotifyUpdateWhen = config.NotifyWhenProblems }, partial, true},
		{"update problems: failed", func(c *config.Config) { c.NotifyUpdateWhen = config.NotifyWhenProblems }, failed, true},
		{"update problems: a cancel by the person is no problem", func(c *config.Config) { c.NotifyUpdateWhen = config.NotifyWhenProblems }, cancelled, false},
		{"update off", func(c *config.Config) { c.NotifyUpdate = false }, failed, false},
		{"backup off", func(c *config.Config) { c.NotifyBackup = false }, bk, false},
		{"backup on", func(*config.Config) {}, bk, true},
		{"snapshot off", func(c *config.Config) { c.NotifySnapshot = false }, sn, false},
		{"unknown event", func(*config.Config) {}, Event{Name: "x"}, false},
	}
	for _, tc := range cases {
		c := baseConfig()
		tc.mod(&c)
		if got := Selected(c, tc.ev); got != tc.want {
			t.Errorf("%s: Selected = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Why: Emit is the one entry point of the app. It must send to the channel that is on and
// nothing when the event is not selected, so a request count is the real proof.
func TestEmitSendsOnlySelectedEventsToEnabledChannels(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := baseConfig()
	c.NotifyWebhook, c.NotifyWebhookURL, c.NotifyUpdateWhen = true, srv.URL, config.NotifyWhenProblems
	d := newDispatcher(&c, nil)
	d.Emit(UpdateFinished(c, 1, 5, 0, 0, "1", t0))
	d.Emit(UpdateFinished(c, 2, 4, 1, 0, "1", t0))
	d.Wait()
	if hits.Load() != 1 {
		t.Fatalf("webhook got %d requests, want 1 (only the partial transaction)", hits.Load())
	}
	if st := d.Status(Webhook); !st.Delivery.OK || st.Delivery.Context != "update job #2 (partial)" {
		t.Errorf("status = %+v, want the partial update recorded as delivered", st.Delivery)
	}
	if st := d.Status(Email); !st.Delivery.At.IsZero() {
		t.Errorf("e-mail is off but has a delivery: %+v", st.Delivery)
	}
}

// Why: the Home Assistant preset promises "the same JSON, POST to the webhook URL": an automation
// reads trigger.json.event. The generic format and the preset must not drift apart.
func TestWebhookBodyIsTheSameJSONForGenericAndHomeAssistant(t *testing.T) {
	for _, format := range []string{config.NotifyFormatJSON, config.NotifyFormatHomeAssistant} {
		var got map[string]string
		var ctype, method string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctype, method = r.Header.Get("Content-Type"), r.Method
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, &got); err != nil {
				t.Errorf("%s: body is not JSON: %v", format, err)
			}
		}))
		c := baseConfig()
		c.NotifyWebhook, c.NotifyWebhookURL, c.NotifyWebhookFormat = true, srv.URL+"/api/webhook/tf-notify-9c1f", format
		d := newDispatcher(&c, nil)
		d.Emit(UpdateFinished(c, 41, 11, 1, 0, "16.1.0", t0))
		d.Wait()
		srv.Close()
		want := map[string]string{"event": "update.finished", "result": "partial", "title": "Update job #41: 11 ok, 1 failed",
			"text": "12 devices to 16.1.0; 1 failed", "url": "http://tasmofleet.local/transactions/41", "time": "2026-10-31T03:12:07Z"}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", format, k, got[k], v)
			}
		}
		if method != "POST" || ctype != "application/json" {
			t.Errorf("%s: %s with %q, want POST application/json", format, method, ctype)
		}
	}
}

// Why: the secret header is the only protection of an open receiver; it must be sent when asked
// for and never when authentication is "none" (a stale stored secret must not leak).
func TestWebhookSecretHeaderOnlyWhenSelected(t *testing.T) {
	for _, auth := range []string{config.NotifyAuthHeader, config.NotifyAuthNone} {
		var hdr string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hdr = r.Header.Get("X-Token") }))
		c := baseConfig()
		c.NotifyWebhook, c.NotifyWebhookURL, c.NotifyWebhookAuth, c.NotifyWebhookHeader, c.NotifyWebhookSecret = true, srv.URL, auth, "X-Token", "s3cret-value"
		d := newDispatcher(&c, nil)
		if _, err := d.Test(t.Context(), Webhook, c); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if want := map[string]string{config.NotifyAuthHeader: "s3cret-value", config.NotifyAuthNone: ""}[auth]; hdr != want {
			t.Errorf("auth %s: header = %q, want %q", auth, hdr, want)
		}
	}
}

// Why: a failed delivery is recorded so the page can say why, and the record, the log and the
// error must never carry the secret or the address (a Home Assistant webhook id is a secret too).
func TestFailureIsRecordedWithoutSecrets(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close() // nothing listens there any more: connection refused
	for _, target := range []string{srv.URL, closedURL} {
		var log bytes.Buffer
		c := baseConfig()
		c.NotifyWebhook, c.NotifyWebhookURL, c.NotifyWebhookAuth, c.NotifyWebhookSecret = true, target+"/api/webhook/hook-id-123", config.NotifyAuthHeader, "s3cret-value"
		d := newDispatcher(&c, &log)
		a, err := d.Test(t.Context(), Webhook, c)
		if err != nil {
			t.Fatal(err)
		}
		if a.OK || a.Detail == "" {
			t.Fatalf("attempt = %+v, want a recorded failure with a reason", a)
		}
		st := d.Status(Webhook)
		if st.Test != a {
			t.Errorf("status test = %+v, want the attempt %+v", st.Test, a)
		}
		for _, text := range []string{a.Detail, log.String()} {
			for _, leak := range []string{"s3cret-value", "hook-id-123"} {
				if strings.Contains(text, leak) {
					t.Errorf("%q leaks %q", text, leak)
				}
			}
		}
	}
}

// Why: "one retry max" (the receiver may be restarting) but a refusal such as 404 will not get
// better, and a receiver that is down must not be hammered.
func TestWebhookRetriesOnceOnServerErrorNeverOnRefusal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		codes    []int
		wantReqs int32
		wantOK   bool
	}{
		{"503 then 200 succeeds on the retry", []int{503, 200}, 2, true},
		{"503 twice stops after the retry", []int{503, 503, 200}, 2, false},
		{"404 is not retried", []int{404, 200}, 1, false},
	} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.codes[min(int(n.Add(1))-1, len(tc.codes)-1)])
		}))
		c := baseConfig()
		c.NotifyWebhook, c.NotifyWebhookURL = true, srv.URL
		a, _ := newDispatcher(&c, nil).Test(t.Context(), Webhook, c)
		srv.Close()
		if n.Load() != tc.wantReqs || a.OK != tc.wantOK {
			t.Errorf("%s: %d requests, ok=%v (%s); want %d, %v", tc.name, n.Load(), a.OK, a.Detail, tc.wantReqs, tc.wantOK)
		}
	}
}

// Why: the secret header must not follow a redirect to another host.
func TestWebhookDoesNotFollowARedirectToAnotherHost(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer other.Close()
	// 127.0.0.1 and "localhost" are different hosts for the redirect rule.
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1), http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	c := baseConfig()
	c.NotifyWebhook, c.NotifyWebhookURL, c.NotifyWebhookAuth, c.NotifyWebhookSecret = true, redir.URL, config.NotifyAuthHeader, "s3cret-value"
	a, _ := newDispatcher(&c, nil).Test(t.Context(), Webhook, c)
	if a.OK || leaked.Load() != 0 {
		t.Fatalf("ok=%v, other host got %d requests (%s); want a refused redirect", a.OK, leaked.Load(), a.Detail)
	}
}

// Why: a double click on "Send test" must not send twice.
func TestTestIsRateLimitedPerChannel(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	defer srv.Close()
	c := baseConfig()
	c.NotifyWebhook, c.NotifyWebhookURL = true, srv.URL
	d := newDispatcher(&c, nil)
	if _, err := d.Test(t.Context(), Webhook, c); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Test(t.Context(), Webhook, c); !errors.Is(err, ErrTooSoon) {
		t.Fatalf("second test err = %v, want ErrTooSoon", err)
	}
	if n.Load() != 1 {
		t.Fatalf("%d requests, want 1", n.Load())
	}
}

// Why: one backup message per run, and it must stay readable when many devices fail.
func TestBackupMessageListsDevicesAndCapsTheList(t *testing.T) {
	names := make([]string, 13)
	for i := range names {
		names[i] = "dev" + string(rune('a'+i))
	}
	ev := BackupFailed(baseConfig(), 40, names, t0)
	if ev.Title != "Scheduled backup: 13 of 40 devices failed" || !strings.HasSuffix(ev.Text, "and 3 more") || strings.Contains(ev.Text, "devk") {
		t.Fatalf("event = %+v", ev)
	}
	if got := Link(config.Config{}, "/x"); got != "" {
		t.Errorf("link without PUBLIC_URL = %q, want empty", got)
	}
}

// Why (review): a secret header sent to an https address must not follow a redirect to http on the
// same host, where it would travel in clear text.
func TestWebhookRefusesARedirectFromHTTPSToPlainHTTP(t *testing.T) {
	cl := newWebhookClient()
	mk := func(u string) *http.Request { r, _ := http.NewRequest("POST", u, nil); return r }
	for _, tc := range []struct {
		from, to string
		ok       bool
	}{
		{"https://hass.lan/h", "http://hass.lan/h", false},
		{"https://hass.lan/h", "https://hass.lan/other", true},
		{"http://hass.lan/h", "https://hass.lan/h", true},
		{"http://hass.lan/h", "http://elsewhere.lan/h", false},
	} {
		err := cl.CheckRedirect(mk(tc.to), []*http.Request{mk(tc.from)})
		if (err == nil) != tc.ok {
			t.Errorf("%s -> %s: err=%v, want ok=%v", tc.from, tc.to, err, tc.ok)
		}
	}
}
