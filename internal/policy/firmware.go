package policy

import "github.com/nofuturekid/tasmofleet/internal/model"

// Firmware variants that answer almost every command with "Unknown": the ESP32 safeboot image
// and the ESP8266 minimal firmware (docs/research/tasmota-minimal-and-ota.md).
const (
	variantMinimal  = "minimal"
	variantSafeboot = "safeboot"
)

// LimitedFirmware reports whether a device with this normalised Identity.Variant runs a firmware
// that does not know the settings commands, so nothing but the recovery commands may be sent to it.
func LimitedFirmware(variant string) bool {
	return variant == variantMinimal || variant == variantSafeboot
}

// FirmwareNote says in one phrase why a device on limited firmware is not judged and nothing is
// sent to it; "" for any other device. The wording is shown on the policy page.
func FirmwareNote(d model.Device) string {
	if !LimitedFirmware(d.Identity.Variant) {
		return ""
	}
	return "runs " + d.Identity.Variant + " firmware"
}
