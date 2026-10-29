package settings

import (
	"strings"
	"testing"
)

// Why: a URL read from a device may carry a password, and passwords can contain '/', '#' or '?',
// which url.Parse reads as path, fragment or query. The password must never reach a report,
// the audit log or the HTML, whatever it contains.
func TestRedactURLCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		secret         string // must not appear in the output
	}{
		{"user and password", "http://user:pa/ss@host/x.bin", "***", "pa/ss"},
		{"hash in password", "http://user:p#w@host/x.bin", "***", "p#w"},
		{"question mark in password", "http://user:p?w@host/x.bin", "***", "p?w"},
		{"plain userinfo", "http://user:secret@host/x.bin?t=1", "http://***@host/x.bin", "secret"},
		{"user only", "https://user@host/x.bin", "https://***@host/x.bin", "user"},
		{"no credentials keeps query", "http://host/x.bin?v=2", "http://host/x.bin?v=2", ""},
		{"no credentials", "http://host/x.bin", "http://host/x.bin", ""},
		// An '@' in the path cannot be told from a mis-parsed password ("http://u:1/abc@host"),
		// so it is hidden too: wrongly hiding is harmless, leaking is not.
		{"at sign only in path", "http://host/a@b", "***", ""},
		{"empty", "", "", ""},
		{"not a URL", "tasmota.bin", "tasmota.bin", ""},
		{"unparseable with at sign", "http://[::1@x", "***", "::1"},
	} {
		got := RedactURLCredentials(tc.in)
		if got != tc.want {
			t.Errorf("%s: RedactURLCredentials(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if tc.secret != "" && strings.Contains(got, tc.secret) {
			t.Errorf("%s: secret %q leaked in %q", tc.name, tc.secret, got)
		}
	}
	// Why: the policy compares the redacted value; a URL with credentials must differ from the
	// same URL without, or a wrong-credential OtaUrl would count as compliant.
	if RedactURLCredentials("http://u:p@host/x") == RedactURLCredentials("http://host/x") {
		t.Error("a URL with credentials compares equal to one without")
	}
}

// Why: parseOtaURL stores the split result, so the split must drop everything that can hold a
// secret and report when it cannot.
func TestSplitURLCredentials(t *testing.T) {
	for _, tc := range []struct {
		in, clean string
		creds, ok bool
	}{
		{"http://admin:s3cret@fw/x.bin", "http://fw/x.bin", true, true},
		{"http://fw/x.bin?token=abc#f", "http://fw/x.bin", false, true},
		{"  http://fw/x.bin  ", "http://fw/x.bin", false, true},
		{"", "", false, true},
		{"http://admin:12#tok@fw/x.bin", "", false, false},
		{"not a url", "", false, false},
	} {
		clean, creds, ok := SplitURLCredentials(tc.in)
		if clean != tc.clean || creds != tc.creds || ok != tc.ok {
			t.Errorf("%q: got %q,%v,%v want %q,%v,%v", tc.in, clean, creds, ok, tc.clean, tc.creds, tc.ok)
		}
	}
}

// Why: whatever a device sends, the password must not survive redaction.
func FuzzRedactURLCredentialsNeverLeaksPassword(f *testing.F) {
	for _, s := range []string{"a", "p/w", "p#w", "p?w", "p@w", "é", "%41", "@00/"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, pw string) {
		if len(pw) < 4 || strings.Contains(pw, "http") {
			return // too short to tell from the URL's own text
		}
		for _, in := range []string{"http://u:" + pw + "@host/x.bin", "https://u:" + pw + "@h?q=1#f"} {
			if out := RedactURLCredentials(in); strings.Contains(out, pw) {
				t.Fatalf("password %q leaked: %q -> %q", pw, in, out)
			}
		}
	})
}
