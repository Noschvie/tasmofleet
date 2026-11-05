package notify

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/config"
)

type seen struct {
	method, path, ctype, click, prio, secret string
	body                                     string
	titleRaw                                 string
}

// capture starts a receiver that records the request as it arrived on the wire.
func capture(t *testing.T, s *seen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*s = seen{method: r.Method, path: r.URL.RequestURI(), ctype: r.Header.Get("Content-Type"), titleRaw: r.Header.Get("Title"),
			click: r.Header.Get("Click"), prio: r.Header.Get("Priority"), secret: r.Header.Get("X-Token"), body: string(b)}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func send(t *testing.T, format string, ev Event, mod func(*config.Config)) seen {
	t.Helper()
	var s seen
	srv := capture(t, &s)
	c := baseConfig()
	c.NotifyWebhook, c.NotifyWebhookURL, c.NotifyWebhookFormat = true, srv.URL+"/tf-topic?x=1", format
	if mod != nil {
		mod(&c)
	}
	d := newDispatcher(&c, nil)
	d.Emit(ev)
	d.Wait()
	return s
}

// Why: ntfy shows the text body as the message and the Title header as the headline. Posting JSON
// there would show raw braces on the phone (#469).
func TestNtfyPostsPlainTextWithTitleClickAndPriority(t *testing.T) {
	ev := UpdateFinished(baseConfig(), 41, 11, 1, 0, "16.1.0", t0)
	s := send(t, config.NotifyFormatNtfy, ev, nil)
	if s.method != "POST" || s.path != "/tf-topic?x=1" {
		t.Errorf("request = %s %s, want POST to the URL as typed", s.method, s.path)
	}
	if s.body != "12 devices to 16.1.0; 1 failed" {
		t.Errorf("body = %q, want the plain text", s.body)
	}
	if !strings.HasPrefix(s.ctype, "text/plain") {
		t.Errorf("content type = %q, want text/plain", s.ctype)
	}
	if s.titleRaw != "Update job #41: 11 ok, 1 failed" {
		t.Errorf("Title = %q", s.titleRaw)
	}
	if s.click != "http://tasmofleet.local/transactions/41" {
		t.Errorf("Click = %q", s.click)
	}
	if s.prio != "4" {
		t.Errorf("Priority = %q, want 4 for a partial result (it should stand out)", s.prio)
	}
}

func TestNtfyPriorityDefaultForGoodNews(t *testing.T) {
	s := send(t, config.NotifyFormatNtfy, UpdateFinished(baseConfig(), 1, 3, 0, 0, "1", t0), nil)
	if s.prio != "3" {
		t.Errorf("Priority = %q, want 3 for an ok result", s.prio)
	}
	s = send(t, config.NotifyFormatNtfy, SnapshotFailed(baseConfig(), "disk full", t0), nil)
	if s.prio != "4" {
		t.Errorf("Priority = %q, want 4 for a failure", s.prio)
	}
}

// Why: Click needs an address the phone can open. Without PUBLIC_URL there is none, and a
// made-up one would be a dead link.
func TestNtfyNoClickWithoutPublicURL(t *testing.T) {
	c := baseConfig()
	c.PublicURL = ""
	s := send(t, config.NotifyFormatNtfy, UpdateFinished(c, 41, 1, 0, 0, "1", t0), nil)
	if s.click != "" {
		t.Errorf("Click = %q, want none", s.click)
	}
}

// Why: header values are ASCII on the wire; a title with an umlaut or arrow must survive as RFC
// 2047 (ntfy's documented way), and a newline in a title must never become a header line (the
// Go client would refuse the whole request, the message would be lost).
func TestNtfyTitleIsRFC2047AndNeverHoldsLineBreaks(t *testing.T) {
	ev := Event{Name: EventUpdateFinished, Result: ResultOK, Title: "Küche → Update finished", Text: "x", Time: t0}
	s := send(t, config.NotifyFormatNtfy, ev, nil)
	if strings.ContainsAny(s.titleRaw, "üö→") || !strings.HasPrefix(s.titleRaw, "=?UTF-8?") {
		t.Errorf("Title = %q, want an RFC 2047 encoded word", s.titleRaw)
	}
	dec, err := new(mime.WordDecoder).DecodeHeader(s.titleRaw)
	if err != nil || dec != ev.Title {
		t.Errorf("decoded Title = %q (%v), want %q", dec, err, ev.Title)
	}

	ev.Title = "line one\r\nInjected: yes\nline three"
	s = send(t, config.NotifyFormatNtfy, ev, nil)
	if s.method != "POST" {
		t.Fatal("a title with a line break must still be delivered")
	}
	if strings.ContainsAny(s.titleRaw, "\r\n") || s.titleRaw == "" {
		t.Errorf("Title = %q, want one line", s.titleRaw)
	}
}

// Why: Gotify reads title/message/priority from JSON; click.url is how a notification opens
// the TasmoFleet page.
func TestGotifyPostsJSONWithClickExtras(t *testing.T) {
	ev := UpdateFinished(baseConfig(), 41, 11, 1, 0, "16.1.0", t0)
	s := send(t, config.NotifyFormatGotify, ev, nil)
	if s.method != "POST" || s.path != "/tf-topic?x=1" || s.ctype != "application/json" {
		t.Errorf("request = %s %s %q", s.method, s.path, s.ctype)
	}
	var got struct {
		Title    string                                  `json:"title"`
		Message  string                                  `json:"message"`
		Priority int                                     `json:"priority"`
		Extras   map[string]map[string]map[string]string `json:"extras"`
	}
	if err := json.Unmarshal([]byte(s.body), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got.Title != ev.Title || got.Message != ev.Text || got.Priority != 8 {
		t.Errorf("got %+v, want title, message and priority 8 for a partial result", got)
	}
	if u := got.Extras["client::notification"]["click"]["url"]; u != ev.URL {
		t.Errorf("click url = %q, want %q", u, ev.URL)
	}

	s = send(t, config.NotifyFormatGotify, UpdateFinished(baseConfig(), 1, 3, 0, 0, "1", t0), nil)
	if !strings.Contains(s.body, `"priority":5`) {
		t.Errorf("body = %s, want priority 5 for an ok result", s.body)
	}
}

func TestGotifyNoExtrasWithoutPublicURL(t *testing.T) {
	c := baseConfig()
	c.PublicURL = ""
	s := send(t, config.NotifyFormatGotify, UpdateFinished(c, 41, 1, 0, 0, "1", t0), nil)
	if strings.Contains(s.body, "extras") {
		t.Errorf("body = %s, want no click extras", s.body)
	}
}

// Why: whatever the format, the secret header is applied, and the format must not change that.
func TestSecretHeaderAppliesToEveryFormat(t *testing.T) {
	for _, f := range []string{config.NotifyFormatJSON, config.NotifyFormatHomeAssistant, config.NotifyFormatNtfy, config.NotifyFormatGotify} {
		s := send(t, f, UpdateFinished(baseConfig(), 1, 1, 0, 0, "1", t0), func(c *config.Config) {
			c.NotifyWebhookAuth, c.NotifyWebhookHeader, c.NotifyWebhookSecret = config.NotifyAuthHeader, "X-Token", "s3cret"
		})
		if s.secret != "s3cret" {
			t.Errorf("%s: secret header = %q", f, s.secret)
		}
	}
}

// Why: a secret header must never follow a redirect to another host, in the new formats either.
func TestNtfySecretNeverFollowsRedirectElsewhere(t *testing.T) {
	var leaked string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = r.Header.Get("X-Token") }))
	defer other.Close()
	// "localhost" is another host than 127.0.0.1:port, so the redirect leaves the host.
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1), http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	c := baseConfig()
	c.NotifyWebhook, c.NotifyWebhookURL, c.NotifyWebhookFormat = true, redir.URL, config.NotifyFormatNtfy
	c.NotifyWebhookAuth, c.NotifyWebhookHeader, c.NotifyWebhookSecret = config.NotifyAuthHeader, "X-Token", "s3cret"
	d := newDispatcher(&c, nil)
	a, _ := d.Test(t.Context(), Webhook, c)
	if a.OK || leaked != "" {
		t.Errorf("ok=%v leaked=%q, want the redirect refused and no secret sent", a.OK, leaked)
	}
}

// Why: an unknown format (a hand-edited database) must not be sent as something else.
func TestUnknownFormatIsRefused(t *testing.T) {
	var s seen
	srv := capture(t, &s)
	c := baseConfig()
	c.NotifyWebhook, c.NotifyWebhookURL, c.NotifyWebhookFormat = true, srv.URL, "shoutrrr"
	d := newDispatcher(&c, nil)
	a, err := d.Test(t.Context(), Webhook, c)
	if err != nil {
		t.Fatal(err)
	}
	if a.OK || s.method != "" {
		t.Errorf("ok=%v method=%q, want refused without sending", a.OK, s.method)
	}
}

// Why: the Message card promises "what is sent"; it must show the request of the chosen format.
func TestWebhookPreviewFollowsFormat(t *testing.T) {
	ev := Sample(baseConfig())
	cases := map[string][]string{
		config.NotifyFormatJSON:          {"POST <webhook URL>", "Content-Type: application/json", `"event": "update.finished"`},
		config.NotifyFormatHomeAssistant: {"POST <webhook URL>", "Content-Type: application/json", `"title"`},
		config.NotifyFormatNtfy:          {"POST <webhook URL>", "Title: Update job #41: 11 ok, 1 failed", "Priority: 4", "Click: http://tasmofleet.local/transactions/41", "12 devices to 16.1.0; 1 failed"},
		config.NotifyFormatGotify:        {"POST <webhook URL>", "Content-Type: application/json", `"priority": 8`, `"client::notification"`},
	}
	for f, wants := range cases {
		c := baseConfig()
		c.NotifyWebhookFormat = f
		p := WebhookPreview(c, ev)
		for _, w := range wants {
			if !strings.Contains(p, w) {
				t.Errorf("%s preview lacks %q:\n%s", f, w, p)
			}
		}
	}
	c := baseConfig()
	c.NotifyWebhookAuth, c.NotifyWebhookHeader, c.NotifyWebhookSecret, c.NotifyWebhookFormat = config.NotifyAuthHeader, "X-Token", "s3cret", config.NotifyFormatNtfy
	if p := WebhookPreview(c, ev); strings.Contains(p, "s3cret") || strings.Contains(p, "X-Token") {
		t.Errorf("preview shows the secret header:\n%s", p)
	}
}
