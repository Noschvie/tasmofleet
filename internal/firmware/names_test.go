package firmware

import (
	"errors"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// The file name decides which family a downloaded image must prove (spec
// 7.4): a mistake here either lets a wrong-family image into the cache or
// rejects good ones, so every upstream naming pattern is pinned.
func TestParseName(t *testing.T) {
	tests := []struct {
		file     string
		family   model.ChipFamily
		variant  string
		lang     string
		safeboot bool
	}{
		{"tasmota.bin.gz", model.ChipESP8266, "tasmota", "", false},
		{"tasmota-minimal.bin.gz", model.ChipESP8266, "minimal", "", false},
		{"tasmota-sensors.bin.gz", model.ChipESP8266, "sensors", "", false},
		{"tasmota-4M.bin.gz", model.ChipESP8266, "4M", "", false},
		{"tasmota-DE.bin.gz", model.ChipESP8266, "tasmota", "DE", false},
		{"tasmota32.bin", model.ChipESP32, "tasmota", "", false},
		{"tasmota32-sensors.bin", model.ChipESP32, "sensors", "", false},
		{"tasmota32-DE.bin", model.ChipESP32, "tasmota", "DE", false},
		{"tasmota32solo1.bin", model.ChipESP32, "tasmota", "", false},
		{"tasmota32c3.bin", model.ChipESP32C3, "tasmota", "", false},
		{"tasmota32c61.bin", model.ChipESP32C61, "tasmota", "", false},
		{"tasmota32c6.bin", model.ChipESP32C6, "tasmota", "", false},
		{"tasmota32s2cdc.bin", model.ChipESP32S2, "tasmota", "", false},
		{"tasmota32p4r3.bin", model.ChipESP32P4, "tasmota", "", false},
		{"tasmota32c3-bluetooth.bin", model.ChipESP32C3, "bluetooth", "", false},
		{"tasmota32-safeboot.bin", model.ChipESP32, "tasmota", "", true},
		{"tasmota32c3-safeboot.bin", model.ChipESP32C3, "tasmota", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			got, err := parseName(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			if got.Family != tc.family || got.Variant != tc.variant || got.Language != tc.lang || got.Safeboot != tc.safeboot {
				t.Fatalf("parseName(%q) = %+v", tc.file, got)
			}
		})
	}
}

// Anything that is not an OTA app image must not get into the cache, and
// path-like names must never reach the file system.
func TestParseNameRejects(t *testing.T) {
	for _, f := range []string{
		"", "tasmota32.factory.bin", "tasmota32-DE.factory.bin", "tasmota32-sensors.factory.bin", "tasmota32c3-safeboot.factory.bin", "tasmota.map.gz", "tasmota.bin", "tasmota32.bin.gz",
		"../tasmota.bin.gz", "a/tasmota.bin.gz", `a\tasmota.bin.gz`, ".tasmota.bin.gz", "other.bin", "tasmota32xyz.bin",
	} {
		if _, err := parseName(f); !errors.Is(err, ErrBadName) {
			t.Errorf("parseName(%q) err = %v, want ErrBadName", f, err)
		}
	}
}

func TestValidRelease(t *testing.T) {
	for _, r := range []string{"16.0.0", "15.3.0", "16.1.0-rc1"} {
		if !validRelease(r) {
			t.Errorf("%q should be valid", r)
		}
	}
	for _, r := range []string{"", ".", "..", "../x", "a/b", `a\b`, ".hidden", "a b", "%2e%2e"} {
		if validRelease(r) {
			t.Errorf("%q must be invalid (becomes a directory name)", r)
		}
	}
}

// Prune keeps the "newest" releases, so ordering must be numeric per dotted
// field: 16.0.0 > 15.10.0 > 15.9.0 (a string sort gets this wrong).
func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"16.0.0", "15.3.0", 1}, {"15.10.0", "15.9.0", 1}, {"15.9.0", "15.10.0", -1},
		{"16.0.0", "16.0.0", 0}, {"16.0", "16.0.0", 0}, {"16.1.0-rc1", "16.0.0", 1},
	}
	for _, tc := range tests {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
