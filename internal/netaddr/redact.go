package netaddr

import (
	"net/url"
	"strings"
)

// RedactURL makes a URL-looking value safe to show: the user information
// becomes "***" and the query is dropped (it can carry a token). A value
// without "://" is not a URL and passes unchanged. A value that looks like a
// URL but does not parse becomes "***", because it cannot be proven free of
// credentials. This is the one redaction for broker URLs and settings values;
// config.RedactWebhookURL and settings.RedactURLCredentials follow other rules
// on purpose (see the source test in redact_source_test.go).
func RedactURL(v string) string {
	if !strings.Contains(v, "://") {
		return v
	}
	u, err := url.Parse(v)
	if err != nil {
		return "***"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	u.RawQuery = ""
	return u.String()
}

// URLHost returns scheme://host[:port] of a URL for a log line or an error text:
// no user information, path or query. A value that does not parse, has no
// host, or has an "@" outside user information (an unencoded password that
// url.Parse split at "#" or "/") becomes "<unparsable>".
func URLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.User == nil && strings.Contains(raw, "@")) {
		return "<unparsable>"
	}
	return u.Scheme + "://" + u.Host
}
