package web

import "testing"

// Why: "select all with update available" must pick devices that are really
// behind, and must leave alone devices whose version it cannot read.
func TestVersionOlder(t *testing.T) {
	for _, tc := range []struct {
		have, want string
		older      bool
	}{
		{"15.0.0", "16.0.0", true},
		{"16.0.0", "16.0.0", false},
		{"16.1.0", "16.0.9", false},
		{"15.9.9", "16", true},
		{"v14.2.1", "15.0.0", true},
		{"15.0.0(tasmota)", "15.0.1", true},
		{"", "16.0.0", false},
		{"unknown", "16.0.0", false},
		{"15.0.0", "latest", false},
	} {
		if got := versionOlder(tc.have, tc.want); got != tc.older {
			t.Errorf("versionOlder(%q, %q) = %v, want %v", tc.have, tc.want, got, tc.older)
		}
	}
}
