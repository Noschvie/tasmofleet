package config

import "testing"

// Why: a channel that is on with an address the sender cannot use fails on the first real event,
// when nobody is watching. The settings page must refuse it at save time; a channel that is off
// must never block a save (a half-filled form of an unused channel).
func TestNotifySettingsAreValidatedOnlyWhileTheChannelIsOn(t *testing.T) {
	ok := func(c *Config) {}
	cases := []struct {
		name string
		mod  func(*Config)
		key  string // "" = valid
	}{
		{"defaults", ok, ""},
		{"webhook off, junk URL", func(c *Config) { c.NotifyWebhookURL = "junk" }, ""},
		{"webhook on without URL", func(c *Config) { c.NotifyWebhook = true }, "NOTIFY_WEBHOOK_URL"},
		{"webhook ftp", func(c *Config) { c.NotifyWebhook, c.NotifyWebhookURL = true, "ftp://x/y" }, "NOTIFY_WEBHOOK_URL"},
		{"webhook credentials in the address", func(c *Config) { c.NotifyWebhook, c.NotifyWebhookURL = true, "http://u:p@x/y" }, "NOTIFY_WEBHOOK_URL"},
		{"webhook ok", func(c *Config) { c.NotifyWebhook, c.NotifyWebhookURL = true, "http://hass.lan:8123/api/webhook/x" }, ""},
		{"header Host is the transport's", func(c *Config) {
			c.NotifyWebhook, c.NotifyWebhookURL, c.NotifyWebhookAuth, c.NotifyWebhookHeader = true, "http://x/y", NotifyAuthHeader, "Host"
		}, "NOTIFY_WEBHOOK_HEADER"},
		{"secret with a line break", func(c *Config) {
			c.NotifyWebhook, c.NotifyWebhookURL, c.NotifyWebhookAuth, c.NotifyWebhookSecret = true, "http://x/y", NotifyAuthHeader, "a\r\nX-Evil: 1"
		}, "NOTIFY_WEBHOOK_SECRET"},
		{"smtp on without host", func(c *Config) { c.NotifySMTP = true }, "NOTIFY_SMTP_HOST"},
		{"smtp host with a scheme", func(c *Config) { c.NotifySMTP, c.NotifySMTPHost = true, "smtp://mail" }, "NOTIFY_SMTP_HOST"},
		{"smtp bad port", func(c *Config) { c.NotifySMTP, c.NotifySMTPHost, c.NotifySMTPPort = true, "mail", 70000 }, "NOTIFY_SMTP_PORT"},
		{"smtp bad to", func(c *Config) {
			c.NotifySMTP, c.NotifySMTPHost, c.NotifySMTPFrom, c.NotifySMTPTo = true, "mail", "a@b.c", "nope"
		}, "NOTIFY_SMTP_TO"},
		{"smtp ok with a named sender and two recipients", func(c *Config) {
			c.NotifySMTP, c.NotifySMTPHost, c.NotifySMTPFrom, c.NotifySMTPTo = true, "mail", "TasmoFleet <a@b.c>", "x@y.z, u@v.w"
		}, ""},
		{"unknown security", func(c *Config) { c.NotifySMTPSecurity = "ssl3" }, "NOTIFY_SMTP_SECURITY"},
	}
	for _, tc := range cases {
		c := Config{NotifyUpdateWhen: NotifyWhenAny, NotifyWebhookFormat: NotifyFormatJSON, NotifyWebhookAuth: NotifyAuthNone,
			NotifyWebhookHeader: "Authorization", NotifySMTPPort: 587, NotifySMTPSecurity: NotifySecStartTLS}
		tc.mod(&c)
		var got []string
		c.validateNotify(func(key, _ string) { got = append(got, key) })
		switch {
		case tc.key == "" && len(got) != 0:
			t.Errorf("%s: unexpected errors %v", tc.name, got)
		case tc.key != "" && (len(got) == 0 || got[0] != tc.key):
			t.Errorf("%s: errors %v, want %s first", tc.name, got, tc.key)
		}
	}
}

func TestRedactWebhookURLKeepsOnlySchemeHostAndPort(t *testing.T) {
	for in, want := range map[string]string{
		"http://hass.lan:8123/api/webhook/x?token=y": "http://hass.lan:8123/***",
		"https://ntfy.example/topic":                 "https://ntfy.example/***",
		"http://hass.lan:8123":                       "http://hass.lan:8123",
		"http://hass.lan:8123/":                      "http://hass.lan:8123",
		"junk":                                       "",
		"":                                           "",
	} {
		if got := RedactWebhookURL(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
