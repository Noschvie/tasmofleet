package fwselect

import (
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// TestDecide: the update policy is one table-tested function (TasmoAdmin
// lesson A5: scattered version checks flashed devices that were already
// current, or skipped ones that were not). Versions compare numerically per
// field, so 16.10.0 is newer than 16.9.0 even though it sorts lower as text.
func TestDecide(t *testing.T) {
	newer, reinst, down := model.PolicyOnlyNewer, model.PolicyReinstall, model.PolicyAllowDowngrade
	tests := []struct {
		current, target string
		variant         string
		policy          model.UpdatePolicy
		want            Decision
		wantErr         bool
	}{
		{"15.0.0", "16.0.0", "tasmota", newer, DecideUpgrade, false},
		{"15.0.0", "16.0.0", "tasmota", reinst, DecideUpgrade, false},
		{"15.0.0", "16.0.0", "tasmota", down, DecideUpgrade, false},
		{"16.0.0", "16.0.0", "tasmota", newer, DecideSkip, false},
		{"16.0.0", "16.0.0", "tasmota", reinst, DecideReinstall, false},
		{"16.0.0", "16.0.0", "tasmota", down, DecideSkip, false},
		{"16.0.0", "15.0.0", "tasmota", newer, DecideSkip, false},
		{"16.0.0", "15.0.0", "tasmota", reinst, DecideSkip, false},
		{"16.0.0", "15.0.0", "tasmota", down, DecideDowngrade, false},
		// An unset policy is the documented default, the most conservative one.
		{"16.0.0", "16.0.0", "tasmota", "", DecideSkip, false},
		{"15.0.0", "16.0.0", "tasmota", "", DecideUpgrade, false},
		{"16.0.0", "15.0.0", "tasmota", "", DecideSkip, false},
		// Numeric, not lexical.
		{"16.9.0", "16.10.0", "tasmota", newer, DecideUpgrade, false},
		{"16.10.0", "16.9.0", "tasmota", newer, DecideSkip, false},
		{"9.5.0", "10.0.0", "tasmota", newer, DecideUpgrade, false},
		// Different field counts: missing fields are zero.
		{"16.0", "16.0.0", "tasmota", reinst, DecideReinstall, false},
		{"16.0.0", "16.0.0.1", "tasmota", newer, DecideUpgrade, false},
		{"16.0.0.1", "16.0.0", "tasmota", newer, DecideSkip, false},
		{"16.0.0.1", "16.0.0", "tasmota", down, DecideDowngrade, false},
		{"16.0.1", "16.0.0.9", "tasmota", down, DecideDowngrade, false},
		{"16.1.0", "16.0.9", "tasmota", newer, DecideSkip, false},
		{"15.9.9", "16.0.0", "tasmota", newer, DecideUpgrade, false},
		// Minimal and safeboot must always leave: even for an older target
		// and the "only-newer" policy.
		{"16.0.0", "16.0.0", "minimal", newer, DecideUpgrade, false},
		{"16.0.0", "15.0.0", "minimal", newer, DecideUpgrade, false},
		{"16.0.0", "16.0.0", "safeboot", newer, DecideUpgrade, false},
		{"", "16.0.0", "minimal", newer, DecideUpgrade, false},
		// Bad input.
		{"16.0.0", "16.0.0", "tasmota", "sometimes", "", true},
		{"", "16.0.0", "tasmota", newer, "", true},
		{"16.0.0", "", "tasmota", newer, "", true},
		{"16.0.0", "v16", "tasmota", newer, "", true},
		{"16.x", "16.0.0", "tasmota", newer, "", true},
		{"16.0.0", "16.0.0", "minimal", "sometimes", "", true},
		{"16.0.0", "", "minimal", newer, "", true},
	}
	for _, tt := range tests {
		name := tt.current + "->" + tt.target + "/" + tt.variant + "/" + string(tt.policy)
		t.Run(name, func(t *testing.T) {
			id := model.Identity{Version: tt.current, Variant: tt.variant}
			got, err := Decide(id, Choice{Release: tt.target, Policy: tt.policy})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				if got != "" {
					t.Errorf("decision on error = %q, want empty", got)
				}
				return
			}
			wantErr(t, err, nil)
			if got != tt.want {
				t.Errorf("Decide = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCompareVersions pins the comparison itself, including very long and
// very large fields that must not overflow into a wrong order.
func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
		err  bool
	}{
		{"1", "1", 0, false},
		{"1", "2", -1, false},
		{"2", "1", 1, false},
		{"1.0.0.0", "1", 0, false},
		{"1.2", "1.10", -1, false},
		{"0.0.1", "0.0.0", 1, false},
		{"99999999999999999999", "1", 0, true},
		{"1.-1", "1", 0, true},
		{"1.+1", "1", 0, true},
		{"1. 1", "1", 0, true},
		{"", "1", 0, true},
		{"1", "1.", 0, true},
	}
	for _, tt := range tests {
		got, err := compareVersions(tt.a, tt.b)
		if tt.err {
			if err == nil {
				t.Errorf("compareVersions(%q,%q): want error", tt.a, tt.b)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("compareVersions(%q,%q) = %d,%v, want %d", tt.a, tt.b, got, err, tt.want)
		}
	}
}
