package fwselect

import (
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Upstream v16.0.0 sizes from the spec (§7.6).
const (
	sizeTasmota = 469152
	sizeSensors = 512993
	sizeMinimal = 263670
)

// TestPlan: a 1 MB ESP8266 cannot hold the old and the new full image at
// once. Sending a too-large image directly makes the device fail the
// download (or fall back on its own); staging via minimal is the only safe
// path. ESP32 never needs staging, and staging it would flash a file that
// does not exist for ESP32.
func TestPlan(t *testing.T) {
	fleet := fixtureIdentity(t, "bedroom-cabinet") // Free 352, FlashSize 1024
	esp32 := fixtureIdentity(t, "office-monitor-guest")
	esp8266 := func(free, flash int) model.Identity {
		return model.Identity{Family: model.ChipESP8266, Hardware: "ESP8266EX", Version: "15.0.0", Variant: "tasmota", FreeKB: free, FlashSizeKB: flash}
	}
	esp32Tiny := esp32
	esp32Tiny.FreeKB = 1
	esp32Unknown := esp32
	esp32Unknown.FreeKB, esp32Unknown.FlashSizeKB = 0, 0
	// Exactly at the margin: Free*1024 - 16 KiB.
	edge := int64(352*1024 - 16*1024)

	tests := []struct {
		name    string
		id      model.Identity
		size    int64
		want    model.Plan
		wantErr error
	}{
		{"fleet tasmota staged", fleet, sizeTasmota, model.PlanStaged, nil},
		{"fleet sensors staged", fleet, sizeSensors, model.PlanStaged, nil},
		{"fleet minimal fits", fleet, sizeMinimal, model.PlanDirect, nil},
		{"exactly at margin fits", esp8266(352, 1024), edge, model.PlanDirect, nil},
		{"one byte over margin", esp8266(352, 1024), edge + 1, model.PlanStaged, nil},
		{"4 MB device with room", esp8266(2000, 4096), sizeSensors, model.PlanDirect, nil},
		{"4 MB device without room", esp8266(400, 4096), sizeSensors, model.PlanStaged, nil},
		{"unknown free on 1 MB", esp8266(0, 1024), sizeMinimal, model.PlanStaged, nil},
		{"unknown free on 512 KB", esp8266(0, 512), sizeMinimal, model.PlanStaged, nil},
		{"unknown free and unknown flash", esp8266(0, 0), sizeMinimal, model.PlanStaged, nil},
		{"unknown free on 2 MB", esp8266(0, 2048), sizeSensors, model.PlanDirect, nil},
		{"unknown free on 1025 KB", esp8266(0, 1025), sizeSensors, model.PlanDirect, nil},
		{"esp32 always direct", esp32, 2_000_000, model.PlanDirect, nil},
		{"esp32 ignores tiny free", esp32Tiny, 2_000_000, model.PlanDirect, nil},
		{"esp32 ignores unknown flash", esp32Unknown, 2_000_000, model.PlanDirect, nil},
		{"c3 direct", idC3, 2_000_000, model.PlanDirect, nil},
		{"p4 direct", idP4, 3_000_000, model.PlanDirect, nil},
		{"unknown hardware", idUnknownHW, sizeMinimal, "", ErrUnknownHardware},
		{"empty identity", model.Identity{}, sizeMinimal, "", ErrUnknownHardware},
		{"zero size", fleet, 0, "", errBadSize},
		{"negative size", fleet, -1, "", errBadSize},
		{"esp32 zero size", esp32, 0, "", errBadSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Plan(tt.id, tt.size)
			wantErr(t, err, tt.wantErr)
			if got != tt.want {
				t.Errorf("Plan = %q, want %q", got, tt.want)
			}
		})
	}
}
