package web

import "testing"

// Why: the address link opens the device's own web UI in the browser. It must
// be predictable (default ports hidden), safe (never credentials, never a
// script URL) and must not guess: a value that is not a plain host gets no link.
func TestDeviceWebLink(t *testing.T) {
	for _, tc := range []struct {
		name, addr string
		port       int
		href, text string // empty href: no link
	}{
		{"ip, default port", "172.31.13.28", 0, "http://172.31.13.28", "172.31.13.28"},
		{"explicit port 80 is the default", "172.31.13.28", 80, "http://172.31.13.28", "172.31.13.28"},
		{"non-default port is shown", "172.31.13.28", 8080, "http://172.31.13.28:8080", "172.31.13.28:8080"},
		{"443 means https without the port", "plug.example.internal", 443, "https://plug.example.internal", "plug.example.internal"},
		{"hostname", "lamp-1.example.internal", 0, "http://lamp-1.example.internal", "lamp-1.example.internal"},
		{"ipv6 gets brackets", "fd00::1", 0, "http://[fd00::1]", "[fd00::1]"},
		{"ipv6 with port", "fd00::1", 8081, "http://[fd00::1]:8081", "[fd00::1]:8081"},
		{"ipv6 already in brackets", "[fd00::1]", 443, "https://[fd00::1]", "[fd00::1]"},
		{"userinfo is refused", "admin:secret@172.31.13.28", 0, "", ""},
		{"at sign is refused", "user@host", 0, "", ""},
		{"path is refused", "host/evil", 0, "", ""},
		{"scheme is refused", "javascript:alert(1)", 0, "", ""},
		{"query is refused", "host?x=1", 0, "", ""},
		{"space is refused", "my host", 0, "", ""},
		{"ipv6 zone is refused", "fe80::1%eth0", 0, "", ""},
		{"empty is refused", "", 0, "", ""},
		{"bad port is refused", "172.31.13.28", 70000, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deviceWebLink(tc.addr, tc.port)
			if tc.href == "" {
				if got != nil {
					t.Fatalf("want no link, got %+v", got)
				}
				return
			}
			if got == nil || got.Href != tc.href || got.Display != tc.text {
				t.Fatalf("got %+v, want href %q text %q", got, tc.href, tc.text)
			}
		})
	}
}

// Why: every URL the UI displays follows one rule set. Links only for plain
// http(s) URLs; anything that could carry a secret or is not http(s) stays
// text, and credentials never appear in either form.
func TestURLLinkAndText(t *testing.T) {
	for _, tc := range []struct {
		name, raw, href, text string
	}{
		{"https default port dropped", "https://git.example.test:443/app", "https://git.example.test/app", "git.example.test/app"},
		{"http default port dropped", "http://host:80/", "http://host/", "host"},
		{"http on 443 stays http with the port", "http://host:443", "http://host:443", "host:443"},
		{"https on 80 keeps the port", "https://host:80", "https://host:80", "host:80"},
		{"custom port shown", "http://172.31.1.2:8123/x/y", "http://172.31.1.2:8123/x/y", "172.31.1.2:8123/x/y"},
		{"ipv6", "https://[fd00::2]:8443/", "https://[fd00::2]:8443/", "[fd00::2]:8443"},
		{"userinfo is never a link", "https://user:pw@host/", "", ""},
		{"query is never a link (token)", "https://host/restore?token=abc", "", ""},
		{"fragment is never a link", "https://host/#tok", "", ""},
		{"mqtt is text", "mqtt://broker:1883", "", ""},
		{"tcp is text", "tcp://broker:1883", "", ""},
		{"javascript is text", "javascript:alert(1)", "", ""},
		{"not a url", "hello world", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := urlLink(tc.raw)
			if tc.href == "" {
				if got != nil {
					t.Fatalf("want no link, got %+v", got)
				}
				return
			}
			if got == nil || got.Href != tc.href || got.Display != tc.text {
				t.Fatalf("got %+v, want %q / %q", got, tc.href, tc.text)
			}
		})
	}
	if got := urlText("mqtt://user:secret@broker:1883"); got != "mqtt://broker:1883" {
		t.Fatalf("credentials must be stripped from text: %q", got)
	}
	if got := urlText("plain text"); got != "plain text" {
		t.Fatalf("got %q", got)
	}
}
