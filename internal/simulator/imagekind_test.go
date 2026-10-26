package simulator

import "testing"

// After an OTA the simulated device must report the image tag a real device
// reports for that build (its CODE_IMAGE_STR, not the file name; #85), or the
// simulator tests a Detect that real devices would never meet.
func TestImageKindFollowsBuildName(t *testing.T) {
	cases := []struct {
		url   string
		esp32 bool
		want  string
	}{
		{"/fw/16.0.0/tasmota32.bin", true, "tasmota32"},
		{"/fw/16.0.0/tasmota32c3.bin", true, "tasmota32"},
		{"/fw/16.0.0/tasmota32s2cdc.bin", true, "tasmota32"},
		{"/fw/16.0.0/tasmota32solo1.bin", true, "solo1"},
		{"/fw/16.0.0/tasmota32-ir.bin", true, "ir"},
		{"/fw/16.0.0/tasmota32-lvgl.bin", true, "lvgl-haspmota"},
		{"/fw/16.0.0/tasmota32-DE.bin", true, "tasmota32"},
		{"/fw/16.0.0/tasmota32-safeboot.bin", true, "safeboot"},
		{"/fw/16.0.0/tasmota-minimal.bin.gz", true, "safeboot"},
		{"/fw/16.0.0/tasmota.bin.gz", false, "tasmota"},
		{"/fw/16.0.0/tasmota-sensors.bin.gz", false, "sensors"},
		{"/fw/16.0.0/tasmota-ir.bin.gz", false, "ir"},
		{"/fw/16.0.0/tasmota-4M.bin.gz", false, "tasmota-4M"},
		{"/fw/16.0.0/tasmota-DE.bin.gz", false, "tasmota"},
		{"/fw/16.0.0/tasmota-minimal.bin.gz", false, "minimal"},
		{"/fw/custom/my-build.bin.gz", false, "tasmota"},
		{"/fw/custom/my-build.bin", true, "tasmota32"},
	}
	for _, c := range cases {
		if got := imageKind(c.url, c.esp32); got != c.want {
			t.Errorf("imageKind(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
