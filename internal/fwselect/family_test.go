package fwselect

import (
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// TestFamilyFromHardware has one row per §6.1 entry. The chip family is the
// primary key of the safety guard: a wrong family here sends a binary for the
// wrong CPU to a device, which bricks it. Unknown strings must stay unknown.
func TestFamilyFromHardware(t *testing.T) {
	tests := []struct {
		hw     string
		family model.ChipFamily
		single bool
	}{
		// ESP8266 line.
		{"ESP8266EX", model.ChipESP8266, false},
		{"ESP8266", model.ChipESP8266, false},
		{"ESP8285N08", model.ChipESP8266, false},
		{"ESP8285H16", model.ChipESP8266, false},
		{"ESP8285", model.ChipESP8266, false},
		// Classic dual-core ESP32, including the real fixture string with a revision suffix.
		{"ESP32-D0WD", model.ChipESP32, false},
		{"ESP32-D0WDQ6", model.ChipESP32, false},
		{"ESP32-D0WDQ6 v1.0", model.ChipESP32, false},
		{"ESP32-D0WD-V3", model.ChipESP32, false},
		{"ESP32-D0WDR2-V3", model.ChipESP32, false},
		{"ESP32-D2WD", model.ChipESP32, false},
		{"ESP32-PICO", model.ChipESP32, false},
		{"ESP32-PICO-D4", model.ChipESP32, false},
		{"ESP32-U4WDH", model.ChipESP32, false}, // #155: was unknown, so the Chip row said "Unknown" and Guard refused every update
		// Single core needs solo1 builds: the flag is as important as the family.
		{"ESP32-S0WD", model.ChipESP32, true},
		{"ESP32-SOLO", model.ChipESP32, true},
		{"ESP32-SOLO-1", model.ChipESP32, true},
		{"ESP32-U4WDH-S v3.0", model.ChipESP32, true}, // #155, real hardware string of a solo1 build
		{"ESP32-U4WDH v3.0", model.ChipESP32, false},
		{"ESP32-U4WDH-D v3.0", model.ChipESP32, false}, // "-D": the dual-core part (Tasmota support_esp.ino)
		// Newer chips.
		{"ESP32-S2", model.ChipESP32S2, false},
		{"ESP32-S2FH4", model.ChipESP32S2, false},
		{"ESP32-S3", model.ChipESP32S3, false},
		{"ESP32-C2", model.ChipESP32C2, false},
		{"ESP32-C3", model.ChipESP32C3, false},
		{"ESP32-C5", model.ChipESP32C5, false},
		{"ESP32-C61", model.ChipESP32C61, false}, // must not be taken for C6
		{"ESP32-C6", model.ChipESP32C6, false},
		{"ESP32-P4", model.ChipESP32P4, false},
		// Unknown is final: no fallbacks, no substring guesses.
		{"ESP32-H2", model.ChipUnknown, false},
		{"ESP32-S31", model.ChipUnknown, false}, // a new chip, not an S3
		{"ESP32-U4WDH2", model.ChipUnknown, false},
		{"ESP32-C31", model.ChipUnknown, false},
		{"ESP32-P41", model.ChipUnknown, false},
		{"ESP32", model.ChipUnknown, false},
		{"ESP32-", model.ChipUnknown, false},
		{"ESP82", model.ChipUnknown, false},
		{"", model.ChipUnknown, false},
		{"esp8266ex", model.ChipUnknown, false}, // case-sensitive
		{"esp32-d0wd", model.ChipUnknown, false},
		{"esp32-c3", model.ChipUnknown, false},
		{" ESP8266EX", model.ChipUnknown, false},
		{"XESP32-C3", model.ChipUnknown, false},
		{"BK7231N", model.ChipUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.hw, func(t *testing.T) {
			if got := FamilyFromHardware(tt.hw); got != tt.family {
				t.Errorf("FamilyFromHardware(%q) = %q, want %q", tt.hw, got, tt.family)
			}
			fam, single := hardwareInfo(tt.hw)
			if fam != tt.family || single != tt.single {
				t.Errorf("hardwareInfo(%q) = %q,%v, want %q,%v", tt.hw, fam, single, tt.family, tt.single)
			}
		})
	}
}

// TestMatchHardwareLongestPrefix: in the real table every overlapping
// prefix maps to the same family, so "longest wins" is proven on a table
// where it matters. A future entry (say a single-core variant under a
// shared prefix) must not depend on table order.
func TestMatchHardwareLongestPrefix(t *testing.T) {
	table := []hardwareEntry{
		{"CHIP", model.ChipESP32, false},
		{"CHIP-SOLO", model.ChipESP32, true},
		{"CHIP-X", model.ChipESP32C3, false},
		{"CHIP", model.ChipESP32S3, false}, // same length as the first: first one wins
	}
	tests := []struct {
		hw     string
		family model.ChipFamily
		single bool
	}{
		{"CHIP", model.ChipESP32, false},
		{"CHIP-SOLO-1", model.ChipESP32, true},
		{"CHIP-X", model.ChipESP32C3, false},
		{"CHIP-Y", model.ChipESP32, false},
		{"CHIP1", model.ChipUnknown, false},
		{"CHI", model.ChipUnknown, false},
	}
	for _, tt := range tests {
		fam, single := matchHardware(table, tt.hw)
		if fam != tt.family || single != tt.single {
			t.Errorf("matchHardware(%q) = %q,%v, want %q,%v", tt.hw, fam, single, tt.family, tt.single)
		}
	}
	// Reversed order must give the same answers for the longer prefixes.
	rev := []hardwareEntry{table[2], table[1], table[0]}
	for _, hw := range []string{"CHIP-SOLO-1", "CHIP-X"} {
		a, as := matchHardware(table, hw)
		b, bs := matchHardware(rev, hw)
		if a != b || as != bs {
			t.Errorf("%q depends on table order: %q,%v vs %q,%v", hw, a, as, b, bs)
		}
	}
}

// TestIsLanguage pins the language suffix check at its edges: upstream
// language builds are two upper-case letters (DE, AF, UZ...); "4M" is a
// variant and must not be taken for a language.
func TestIsLanguage(t *testing.T) {
	for s, want := range map[string]bool{
		"DE": true, "AZ": true, "ZA": true, "AA": true, "ZZ": true,
		"4M": false, "@A": false, "A@": false, "[A": false, "A[": false,
		"de": false, "D": false, "DEU": false, "": false,
	} {
		if got := isLanguage(s); got != want {
			t.Errorf("isLanguage(%q) = %v, want %v", s, got, want)
		}
	}
}
