package config

import (
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Values of the notification settings that are a choice.
const (
	NotifyWhenAny      = "any"
	NotifyWhenProblems = "problems"

	NotifyFormatJSON          = "json"
	NotifyFormatHomeAssistant = "homeassistant"

	NotifyAuthNone   = "none"
	NotifyAuthHeader = "header"

	NotifySecStartTLS = "starttls"
	NotifySecTLS      = "tls"
	NotifySecNone     = "none"
)

// NotifyChoices lists the valid values of a notification setting that is a choice.
func NotifyChoices(key string) []string {
	switch key {
	case "NOTIFY_UPDATE_WHEN":
		return []string{NotifyWhenAny, NotifyWhenProblems}
	case "NOTIFY_WEBHOOK_FORMAT":
		return []string{NotifyFormatJSON, NotifyFormatHomeAssistant}
	case "NOTIFY_WEBHOOK_AUTH":
		return []string{NotifyAuthNone, NotifyAuthHeader}
	case "NOTIFY_SMTP_SECURITY":
		return []string{NotifySecStartTLS, NotifySecTLS, NotifySecNone}
	}
	return nil
}

// headerName is an HTTP header name (RFC 9110 token, letters digits and hyphen only here).
var headerName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)

// reservedHeaders are headers the webhook sender sets itself or the transport owns.
var reservedHeaders = []string{"host", "content-length", "content-type", "transfer-encoding", "connection", "user-agent"}

// SMTPRecipients parses the To setting (comma separated addresses).
func SMTPRecipients(to string) ([]*mail.Address, error) {
	if strings.TrimSpace(to) == "" {
		return nil, nil
	}
	return mail.ParseAddressList(to)
}

// validateNotify checks the notification settings. A channel is only checked while it is on, so a
// half-filled form of a channel that is off never blocks a save. A missing webhook header value
// is not checked here: the secret is stored apart from the other settings.
func (c Config) validateNotify(bad func(key, msg string)) {
	for _, k := range []string{"NOTIFY_UPDATE_WHEN", "NOTIFY_WEBHOOK_FORMAT", "NOTIFY_WEBHOOK_AUTH", "NOTIFY_SMTP_SECURITY"} {
		v, _ := Format(c, k)
		if !slices.Contains(NotifyChoices(k), v) {
			bad(k, "must be one of "+strings.Join(NotifyChoices(k), ", "))
		}
	}
	if c.NotifyWebhook {
		u, err := url.Parse(c.NotifyWebhookURL)
		switch {
		case c.NotifyWebhookURL == "":
			bad("NOTIFY_WEBHOOK_URL", "required while the webhook is on")
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
			bad("NOTIFY_WEBHOOK_URL", "must be an http:// or https:// address")
		case u.User != nil:
			bad("NOTIFY_WEBHOOK_URL", "must not contain a user name or password; use the secret header")
		}
		if c.NotifyWebhookAuth == NotifyAuthHeader {
			if strings.ContainsAny(c.NotifyWebhookSecret, "\r\n\x00") {
				bad("NOTIFY_WEBHOOK_SECRET", "must be one line")
			}
			switch {
			case !headerName.MatchString(c.NotifyWebhookHeader):
				bad("NOTIFY_WEBHOOK_HEADER", "must be a header name such as Authorization")
			case slices.Contains(reservedHeaders, strings.ToLower(c.NotifyWebhookHeader)):
				bad("NOTIFY_WEBHOOK_HEADER", "this header is set by TasmoFleet itself")
			}
		}
	}
	if c.NotifySMTP {
		if c.NotifySMTPHost == "" || strings.ContainsAny(c.NotifySMTPHost, " /\\\\@") {
			bad("NOTIFY_SMTP_HOST", "required while e-mail is on: a host name, no scheme")
		}
		if c.NotifySMTPPort < 1 || c.NotifySMTPPort > 65535 {
			bad("NOTIFY_SMTP_PORT", "must be 1 to 65535")
		}
		if _, err := mail.ParseAddress(c.NotifySMTPFrom); err != nil {
			bad("NOTIFY_SMTP_FROM", "must be an address such as tasmofleet@example.com")
		}
		if to, err := SMTPRecipients(c.NotifySMTPTo); err != nil || len(to) == 0 {
			bad("NOTIFY_SMTP_TO", "must be one or more addresses, comma separated")
		}
	}
}
