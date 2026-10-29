package model

import "testing"

// Why: the job and transaction views show Target(); an ESP32-S2 update has two builds that look
// the same otherwise, so the chosen one must be visible there (#88).
func TestJobTargetShowsTheChosenBuild(t *testing.T) {
	for _, tt := range []struct {
		j    Job
		want string
	}{
		{Job{TargetRelease: "16.0.0", TargetVariant: "tasmota"}, "16.0.0 tasmota"},
		{Job{TargetRelease: "16.0.0", TargetVariant: "tasmota", TargetBuild: "s2cdc"}, "16.0.0 tasmota (s2cdc)"},
		{Job{Kind: KindPolicy, TargetBuild: "s2"}, "Make compliant"},
	} {
		if got := tt.j.Target(); got != tt.want {
			t.Errorf("Target() = %q, want %q", got, tt.want)
		}
	}
}
