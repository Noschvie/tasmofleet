package fwselect

import (
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

type hardwareEntry struct {
	prefix string
	family model.ChipFamily
	single bool
}

// hardwareTable is the §6.1 table, the only place that maps
// StatusFWR.Hardware to a chip family.
var hardwareTable = []hardwareEntry{
	{"ESP8266EX", model.ChipESP8266, false},
	{"ESP8266", model.ChipESP8266, false},
	{"ESP8285N08", model.ChipESP8266, false},
	{"ESP8285H16", model.ChipESP8266, false},
	{"ESP8285", model.ChipESP8266, false},
	{"ESP32-D0WD", model.ChipESP32, false},
	{"ESP32-D0WDQ6", model.ChipESP32, false},
	{"ESP32-D0WD-V3", model.ChipESP32, false},
	{"ESP32-D2WD", model.ChipESP32, false},
	{"ESP32-PICO", model.ChipESP32, false},
	{"ESP32-U4WDH", model.ChipESP32, false},
	{"ESP32-U4WDH-S", model.ChipESP32, true}, // "-S": the single-core part, which only solo1 builds run on (#155)
	{"ESP32-S0WD", model.ChipESP32, true},
	{"ESP32-SOLO", model.ChipESP32, true},
	{"ESP32-S2", model.ChipESP32S2, false},
	{"ESP32-S3", model.ChipESP32S3, false},
	{"ESP32-C2", model.ChipESP32C2, false},
	{"ESP32-C3", model.ChipESP32C3, false},
	{"ESP32-C5", model.ChipESP32C5, false},
	{"ESP32-C61", model.ChipESP32C61, false},
	{"ESP32-C6", model.ChipESP32C6, false},
	{"ESP32-P4", model.ChipESP32P4, false},
}

// hardwareInfo returns the family and the single-core flag for a Hardware
// string. Matching is by prefix, case-sensitive, longest prefix wins. A
// prefix only matches if the next character is not a digit, so that a new
// chip such as "ESP32-S31" is not taken for an "ESP32-S3" (suffixes like
// "Q6", "-V3" or " v1.0" are package/revision details of the same chip).
func hardwareInfo(hw string) (model.ChipFamily, bool) {
	return matchHardware(hardwareTable, hw)
}

func matchHardware(table []hardwareEntry, hw string) (model.ChipFamily, bool) {
	var best *hardwareEntry
	for i := range table {
		e := &table[i]
		if !strings.HasPrefix(hw, e.prefix) {
			continue
		}
		if len(hw) > len(e.prefix) && isDigit(hw[len(e.prefix)]) {
			continue
		}
		if best == nil || len(e.prefix) > len(best.prefix) {
			best = e
		}
	}
	if best == nil {
		return model.ChipUnknown, false
	}
	return best.family, best.single
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// chipBuilds maps the suffix of tasmota32<suffix> builds to their family.
// The empty suffix is the classic dual-core build, "solo1" the single-core
// one; s2cdc and p4r3 are alternative builds for the same chip.
var chipBuilds = map[string]model.ChipFamily{
	"":      model.ChipESP32,
	"solo1": model.ChipESP32,
	"s2":    model.ChipESP32S2,
	"s2cdc": model.ChipESP32S2,
	"s3":    model.ChipESP32S3,
	"c2":    model.ChipESP32C2,
	"c3":    model.ChipESP32C3,
	"c5":    model.ChipESP32C5,
	"c6":    model.ChipESP32C6,
	"c61":   model.ChipESP32C61,
	"p4":    model.ChipESP32P4,
	"p4r3":  model.ChipESP32P4,
}

// defaultBuild is the only tasmota32<suffix> build of each newer chip with
// one build. S2 and P4 have two each and are resolved in chipBuild.
var defaultBuild = map[model.ChipFamily]string{
	model.ChipESP32S3:  "s3",
	model.ChipESP32C2:  "c2",
	model.ChipESP32C3:  "c3",
	model.ChipESP32C5:  "c5",
	model.ChipESP32C6:  "c6",
	model.ChipESP32C61: "c61",
}

// Variants built upstream (§7.1). "tasmota" is the plain build.
var (
	esp8266Variants = map[string]bool{
		"tasmota": true, "lite": true, "sensors": true, "ir": true, "knx": true,
		"display": true, "zbbridge": true, "zigbee": true, "4M": true,
	}
	esp32Variants = map[string]bool{
		"tasmota": true, "bluetooth": true, "display": true, "ir": true,
		"lvgl": true, "nspanel": true, "webcam": true, "zbbrdgpro": true,
	}
)

const (
	variantTasmota  = "tasmota"
	variantMinimal  = "minimal"
	variantSafeboot = "safeboot"
	variantUnknown  = "unknown"
	minimalFile     = "tasmota-minimal.bin.gz"
)

// isLanguage reports whether s is an upstream language suffix (two upper
// case ASCII letters, e.g. "DE"). "4M" is a variant, not a language.
func isLanguage(s string) bool {
	return len(s) == 2 && isUpper(s[0]) && isUpper(s[1])
}

func isUpper(b byte) bool { return b >= 'A' && b <= 'Z' }
