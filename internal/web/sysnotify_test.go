package web

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/notify"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// fakeNotify records what the test button sends.
type fakeNotify struct {
	cfg    config.Config
	tested []config.Config
	chans  []notify.Channel
	result notify.Attempt
	status notify.Status
}

func (f *fakeNotify) Config() config.Config               { return f.cfg }
func (f *fakeNotify) Status(notify.Channel) notify.Status { return f.status }
func (f *fakeNotify) Test(_ context.Context, ch notify.Channel, c config.Config) (notify.Attempt, error) {
	f.tested, f.chans = append(f.tested, c), append(f.chans, ch)
	return f.result, nil
}

func notifyEnv(t *testing.T, f *fakeNotify) (*env, *sysconfig.Service) {
	e, svc, _, _ := sysEnvWithDeps(t, func(d *Deps) { d.Notify = f })
	return e, svc
}

// Why: both secrets are write-only like every other secret of this page. After saving, neither
// the page nor the preview may carry them, and the field says only that a value is stored.
func TestNotificationSecretsAreNeverShownBack(t *testing.T) {
	e, svc := notifyEnv(t, &fakeNotify{})
	if _, err := svc.Apply(t.Context(), sysconfig.Request{Actor: "admin", Values: map[string]string{
		"NOTIFY_WEBHOOK_AUTH": "header", "NOTIFY_WEBHOOK_SECRET": "fake-hook-secret-1", "NOTIFY_SMTP_PASSWORD": "fake-mail-pass-2"}}); err != nil {
		t.Fatal(err)
	}
	_, body := e.do(t, "GET", "/settings/system?tab=notifications", "admin", nil)
	for _, leak := range []string{"fake-hook-secret-1", "fake-mail-pass-2"} {
		if strings.Contains(body, leak) {
			t.Errorf("page contains %q", leak)
		}
	}
	if !strings.Contains(body, `type="password" name="v.NOTIFY_WEBHOOK_SECRET"`) || !strings.Contains(body, `type="password" name="v.NOTIFY_SMTP_PASSWORD"`) || !strings.Contains(body, "unchanged (a value is stored)") {
		t.Errorf("secret fields must be password inputs that say a value is stored")
	}
}

// Why: "Send test" saves nothing and tests what is on screen: the typed address, the stored
// secret when the field is left empty (it is write-only, so it cannot be typed again), and a
// typed secret over the stored one. The rest of the form (other tabs) must not matter.
func TestNotificationTestUsesTheFormAndTheStoredSecretAndSavesNothing(t *testing.T) {
	f := &fakeNotify{result: notify.Attempt{OK: true, Detail: "HTTP 200"}}
	e, svc := notifyEnv(t, f)
	if _, err := svc.Apply(t.Context(), sysconfig.Request{Actor: "admin", Values: map[string]string{"NOTIFY_WEBHOOK_URL": "http://hass.lan:8123/api/webhook/old",
		"NOTIFY_WEBHOOK_AUTH": "header", "NOTIFY_WEBHOOK_SECRET": "stored-secret"}}); err != nil {
		t.Fatal(err)
	}
	keep := svc.Snapshot().BackupKeep
	form := url.Values{"csrf": {"tok"}, "v.NOTIFY_WEBHOOK_URL": {"http://hass.lan:8123/api/webhook/old"}, "v.NOTIFY_WEBHOOK_AUTH": {"header"},
		"v.NOTIFY_WEBHOOK_HEADER": {"Authorization"}, "v.BACKUP_KEEP": {"2"}, "v.NOTIFY_SMTP_HOST": {"not a host !"}}
	r, body := e.do(t, "POST", "/settings/system/notify-test/webhook", "admin", form)
	if r.StatusCode != 200 || !strings.Contains(body, "Webhook test: sent, HTTP 200") || strings.Contains(body, "secret not sent") {
		t.Fatalf("status %d: %s", r.StatusCode, body)
	}
	if len(f.tested) != 1 || f.chans[0] != notify.Webhook || f.tested[0].NotifyWebhookURL != "http://hass.lan:8123/api/webhook/old" || f.tested[0].NotifyWebhookSecret != "stored-secret" {
		t.Fatalf("tested %+v", f.tested)
	}
	if svc.Snapshot().NotifySMTPHost != "" || svc.Snapshot().BackupKeep != keep {
		t.Fatal("a test saved a setting")
	}
	form.Set("v.NOTIFY_WEBHOOK_SECRET", "typed-secret")
	e.do(t, "POST", "/settings/system/notify-test/webhook", "admin", form)
	if len(f.tested) != 2 || f.tested[1].NotifyWebhookSecret != "typed-secret" {
		t.Fatalf("a typed secret must win over the stored one: %+v", f.tested)
	}
}

// Why (security review): the stored header value and SMTP password are write-only. If a test
// could send them to whatever address is typed into the form, any admin could point it at their
// own host and read them there. A changed target must therefore get no stored secret, and the
// result line must say so.
func TestNotificationTestNeverSendsAStoredSecretToAnotherTarget(t *testing.T) {
	f := &fakeNotify{result: notify.Attempt{OK: true, Detail: "HTTP 200"}}
	e, svc := notifyEnv(t, f)
	if _, err := svc.Apply(t.Context(), sysconfig.Request{Actor: "admin", Values: map[string]string{
		"NOTIFY_WEBHOOK_URL": "http://hass.lan:8123/api/webhook/old", "NOTIFY_WEBHOOK_AUTH": "header", "NOTIFY_WEBHOOK_SECRET": "stored-secret",
		"NOTIFY_SMTP_HOST": "mail.lan", "NOTIFY_SMTP_USER": "mailer", "NOTIFY_SMTP_PASSWORD": "stored-mail-pass",
		"NOTIFY_SMTP_FROM": "a@b.c", "NOTIFY_SMTP_TO": "d@e.f"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		channel string
		form    url.Values
	}{
		{"webhook", url.Values{"v.NOTIFY_WEBHOOK_URL": {"http://evil.example:8123/api/webhook/old"}, "v.NOTIFY_WEBHOOK_AUTH": {"header"}}},
		{"webhook", url.Values{"v.NOTIFY_WEBHOOK_URL": {"http://hass.lan:9999/api/webhook/old"}, "v.NOTIFY_WEBHOOK_AUTH": {"header"}}},
		{"webhook", url.Values{"v.NOTIFY_WEBHOOK_URL": {"http://hass.lan:8123/api/webhook/other"}, "v.NOTIFY_WEBHOOK_AUTH": {"header"}}},
		{"webhook", url.Values{"v.NOTIFY_WEBHOOK_URL": {"https://hass.lan:8123/api/webhook/old"}, "v.NOTIFY_WEBHOOK_AUTH": {"header"}}},
		{"email", url.Values{"v.NOTIFY_SMTP_HOST": {"evil.example"}, "v.NOTIFY_SMTP_USER": {"mailer"}, "v.NOTIFY_SMTP_FROM": {"a@b.c"}, "v.NOTIFY_SMTP_TO": {"d@e.f"}}},
		{"email", url.Values{"v.NOTIFY_SMTP_HOST": {"mail.lan"}, "v.NOTIFY_SMTP_USER": {"other"}, "v.NOTIFY_SMTP_FROM": {"a@b.c"}, "v.NOTIFY_SMTP_TO": {"d@e.f"}}},
		{"email", url.Values{"v.NOTIFY_SMTP_HOST": {"mail.lan"}, "v.NOTIFY_SMTP_USER": {"mailer"}, "v.NOTIFY_SMTP_SECURITY": {"none"}, "v.NOTIFY_SMTP_FROM": {"a@b.c"}, "v.NOTIFY_SMTP_TO": {"d@e.f"}}},
	} {
		f.tested = nil
		tc.form.Set("csrf", "tok")
		_, body := e.do(t, "POST", "/settings/system/notify-test/"+tc.channel, "admin", tc.form)
		sent := func(c config.Config) string { // the secret this channel's sender would use
			if tc.channel == "email" {
				return c.NotifySMTPPassword
			}
			return c.NotifyWebhookSecret
		}
		if len(f.tested) != 1 || sent(f.tested[0]) != "" {
			t.Errorf("%s %v: the stored secret reached the test: %+v", tc.channel, tc.form, f.tested)
		}
		if !strings.Contains(body, "secret not sent: target changed, enter it to test") {
			t.Errorf("%s %v: the result does not say the secret was left out: %s", tc.channel, tc.form, body)
		}
	}
	// Typing the secret for the new target is the way to test it.
	f.tested = nil
	e.do(t, "POST", "/settings/system/notify-test/webhook", "admin", url.Values{"csrf": {"tok"}, "v.NOTIFY_WEBHOOK_URL": {"http://new.lan/h"}, "v.NOTIFY_WEBHOOK_AUTH": {"header"}, "v.NOTIFY_WEBHOOK_SECRET": {"typed"}})
	if len(f.tested) != 1 || f.tested[0].NotifyWebhookSecret != "typed" {
		t.Fatalf("typed secret: %+v", f.tested)
	}
}

// Why (review): a ticked "Reset to default" on the secret means the save would remove it, so the
// test must not send it either.
func TestNotificationTestHonoursAResetOfTheSecret(t *testing.T) {
	f := &fakeNotify{result: notify.Attempt{OK: true, Detail: "HTTP 200"}}
	e, svc := notifyEnv(t, f)
	if _, err := svc.Apply(t.Context(), sysconfig.Request{Actor: "admin", Values: map[string]string{"NOTIFY_WEBHOOK_URL": "http://hass.lan/h", "NOTIFY_WEBHOOK_AUTH": "header", "NOTIFY_WEBHOOK_SECRET": "stored-secret"}}); err != nil {
		t.Fatal(err)
	}
	e.do(t, "POST", "/settings/system/notify-test/webhook", "admin", url.Values{"csrf": {"tok"}, "v.NOTIFY_WEBHOOK_URL": {"http://hass.lan/h"}, "v.NOTIFY_WEBHOOK_AUTH": {"header"}, "clear.NOTIFY_WEBHOOK_SECRET": {"1"}})
	if len(f.tested) != 1 || f.tested[0].NotifyWebhookSecret != "" {
		t.Fatalf("a secret that is reset on save was sent: %+v", f.tested)
	}
}

// Why: a bad form must be explained next to the button, not sent as a broken request, and a
// failed delivery must show the reason the sender gave in the danger tone.
func TestNotificationTestReportsInvalidFormAndFailedDelivery(t *testing.T) {
	f := &fakeNotify{result: notify.Attempt{Detail: "HTTP 404"}}
	e, _ := notifyEnv(t, f)
	_, body := e.do(t, "POST", "/settings/system/notify-test/webhook", "admin", url.Values{"csrf": {"tok"}})
	if !strings.Contains(body, "failed: required while the webhook is on") || len(f.tested) != 0 {
		t.Fatalf("empty address: tested=%d body=%s", len(f.tested), body)
	}
	_, body = e.do(t, "POST", "/settings/system/notify-test/webhook", "admin", url.Values{"csrf": {"tok"}, "v.NOTIFY_WEBHOOK_URL": {"http://x.lan/h"}})
	if !strings.Contains(body, "Webhook test: failed: HTTP 404") || !strings.Contains(body, `role="alert"`) {
		t.Fatalf("body: %s", body)
	}
	_, body = e.do(t, "POST", "/settings/system/notify-test/email", "admin", url.Values{"csrf": {"tok"}, "v.NOTIFY_SMTP_HOST": {"mail.lan"}})
	if !strings.Contains(body, "failed:") || !strings.Contains(body, "addresses") {
		t.Fatalf("e-mail without From and To must be explained: %s", body)
	}
	if r, _ := e.do(t, "POST", "/settings/system/notify-test/sms", "admin", url.Values{"csrf": {"tok"}}); r.StatusCode != 404 {
		t.Fatalf("unknown channel: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/settings/system/notify-test/webhook", "operator", url.Values{"csrf": {"tok"}}); r.StatusCode != 403 {
		t.Fatalf("operator: %d", r.StatusCode)
	}
}

// Why: the status line tells what happened last, in words that name the cause and the event, so
// an admin can act without reading logs.
func TestNotificationStatusLineWordsDeliveryTestAndNothing(t *testing.T) {
	at := time.Date(2026, 10, 31, 3, 12, 0, 0, time.UTC)
	cases := []struct {
		name string
		conf bool
		st   notify.Status
		want string
	}{
		{"not set up", false, notify.Status{}, "Not configured"},
		{"nothing yet", true, notify.Status{}, "Nothing sent yet"},
		{"delivery failed", true, notify.Status{Delivery: notify.Attempt{At: at, Detail: "HTTP 404", Context: "update job #41 (partial)"}}, "Last delivery failed"},
		{"delivery ok", true, notify.Status{Delivery: notify.Attempt{At: at, OK: true, Detail: "HTTP 200"}}, "Last delivery OK"},
		{"only a test", true, notify.Status{Test: notify.Attempt{At: at, OK: true, Detail: "HTTP 200"}}, "Last test OK"},
	}
	for _, c := range cases {
		if got := notifyChannelView(c.conf, "x", c.st).Title; got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
