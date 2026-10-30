package model

import "testing"

// Why (#289): the CR warning compares used against available characters of the settings text
// area; a CR that does not parse must give no number rather than a wrong percentage.
func TestFWRFactsCRUsage(t *testing.T) {
	for _, tt := range []struct {
		cr          string
		used, total int
		ok          bool
	}{
		{"650/699", 650, 699, true},
		{"0/699", 0, 699, true},
		{"", 0, 0, false},
		{"650", 0, 0, false},
		{"a/b", 0, 0, false},
		{"5/0", 5, 0, false},
	} {
		used, total, ok := FWRFacts{CR: tt.cr}.CRUsage()
		if used != tt.used || total != tt.total || ok != tt.ok {
			t.Errorf("CR %q: got %d/%d ok=%v, want %d/%d ok=%v", tt.cr, used, total, ok, tt.used, tt.total, tt.ok)
		}
	}
}
