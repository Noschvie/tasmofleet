package simulator

import "testing"

// After an OTA the simulated device must report the image tag a real device
// reports for that build, or fwselect.Detect sees a family mismatch (e.g. a
// C3 flashed with tasmota32c3.bin answering "release-tasmota32").
func TestImageKindFollowsBuildName(t *testing.T) {
	cases := []struct {
		url   string
		esp32 bool
		want  string
	}{
		{"/fw/16.0.0/tasmota32.bin", true, "tasmota32"},
		{"/fw/16.0.0/tasmota32c3.bin", true, "tasmota32c3"},
		{"/fw/16.0.0/tasmota32solo1.bin", true, "tasmota32solo1"},
		{"/fw/16.0.0/tasmota32-ir.bin", true, "tasmota32-ir"},
		{"/fw/16.0.0/tasmota32-safeboot.bin", true, "safeboot"},
		{"/fw/16.0.0/tasmota.bin.gz", false, "tasmota"},
		{"/fw/16.0.0/tasmota-sensors.bin.gz", false, "sensors"},
		{"/fw/16.0.0/tasmota-minimal.bin.gz", false, "minimal"},
	}
	for _, c := range cases {
		if got := imageKind(c.url, c.esp32); got != c.want {
			t.Errorf("imageKind(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
