package fwselect

import (
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why (#155, split from #127): the chip comes from StatusFWR.Hardware, never from the build name.
// Both payloads are the reporter's. The generic "tasmota32" build on an ESP32-S3 is an S3 (the
// bare tag only keeps the variant unknown, so no update picks a file for it). The solo1 build on
// an "ESP32-U4WDH-S" is a single-core classic ESP32: the Chip row said "Unknown" because the
// hardware table did not know the package.
func TestDetectChipFromHardwareNotFromTheBuildName(t *testing.T) {
	tests := []struct {
		name, status2 string
		family        model.ChipFamily
		single        bool
	}{
		{"S3 on the generic tasmota32 build",
			`{"StatusFWR":{"Version":"16.0.0(tasmota32)","BuildDateTime":"2026-10-16T18:34:59","Core":"3.3.12","SDK":"5.5.5","CpuFrequency":240,"Hardware":"ESP32-S3 v0.2","CR":"448/699"}}`,
			model.ChipESP32S3, false},
		{"U4WDH-S on a solo1 build",
			`{"StatusFWR":{"Version":"16.0.0(solo1)","Hardware":"ESP32-U4WDH-S v3.0"}}`,
			model.ChipESP32, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := Detect(Inputs{Status2: []byte(tt.status2)})
			if err != nil {
				t.Fatal(err)
			}
			if id.Family != tt.family || id.SingleCore != tt.single {
				t.Fatalf("family %q single %v, want %q %v", id.Family, id.SingleCore, tt.family, tt.single)
			}
			if id.Variant != variantUnknown {
				t.Fatalf("variant %q: a bare build tag must stay unknown (never guess a file)", id.Variant)
			}
		})
	}
}
