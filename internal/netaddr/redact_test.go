package netaddr

import (
	"strings"
	"testing"
)

// Why: MQTT_URL may be tcp://user:password@host:1883 and is shown on the About
// page, in the settings audit trail and in the settings form. The password must
// never get there, and a value that cannot be proven free of credentials is
// hidden whole rather than shown.
func TestRedactURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"tcp://user:fake-pw-1@broker.lan:1883", "tcp://%2A%2A%2A@broker.lan:1883"},
		{"tcp://:fake-pw-1@broker.lan:1883", "tcp://%2A%2A%2A@broker.lan:1883"},
		{"ssl://broker.lan:8883", "ssl://broker.lan:8883"},
		{"https://idp.example/app/?token=fake-tok-2", "https://idp.example/app/"}, // a query can carry a token
		{"tcp://broker.lan", "tcp://broker.lan"},
		{"not a url", "not a url"}, // no "://": nothing to hide
		{"", ""},
		{"tcp://user:fake-pw-1@bro ker:1883", "***"}, // does not parse: cannot prove it is clean
		{"tcp://user:fake-pw-1@broker:port", "***"},
	}
	for _, tc := range tests {
		got := RedactURL(tc.in)
		if got != tc.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(got, "fake-pw-1") || strings.Contains(got, "fake-tok-2") {
			t.Errorf("RedactURL(%q) = %q shows a secret", tc.in, got)
		}
	}
}

// Why: the MQTT connect log line and its error text name the broker, not its
// credentials. A URL without a host says so instead of printing it, so a
// mistyped password never ends up in a log.
func TestURLHost(t *testing.T) {
	tests := []struct{ in, want string }{
		{"tcp://user:fake-pw-1@broker.lan:1883/path?q=fake-tok-2", "tcp://broker.lan:1883"},
		{"ssl://broker.lan", "ssl://broker.lan"},
		{"tcp://[fd00::1]:1883", "tcp://[fd00::1]:1883"},
		{"", "<unparsable>"},
		{"fake-pw-1", "<unparsable>"},
		{"tcp://user:fake-pw-1@", "<unparsable>"},
		{"tcp://bro ker", "<unparsable>"},
		// url.Parse reads the authority as host "user" with port "12" and puts
		// the rest of the password into the fragment: no part of it may show.
		{"tcp://user:12#fake-pw-1@broker.lan:1883", "<unparsable>"},
		{"tcp://user:/fake-pw-1@broker.lan:1883", "<unparsable>"},
	}
	for _, tc := range tests {
		got := URLHost(tc.in)
		if got != tc.want {
			t.Errorf("URLHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(got, "fake-pw-1") || strings.Contains(got, "fake-tok-2") {
			t.Errorf("URLHost(%q) = %q shows a secret", tc.in, got)
		}
	}
}
