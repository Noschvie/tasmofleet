package fwselect

import (
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func ident(fam model.ChipFamily, hw, image, variant, lang string) model.Identity {
	return model.Identity{Family: fam, Hardware: hw, Version: "15.0.0", Image: image, Variant: variant, Language: lang}
}

var (
	idESP8266   = ident(model.ChipESP8266, "ESP8266EX", "release-tasmota", "tasmota", "")
	idESP8266DE = ident(model.ChipESP8266, "ESP8285N08", "release-tasmota", "tasmota", "DE")
	idSensors   = ident(model.ChipESP8266, "ESP8266EX", "release-sensors", "sensors", "")
	idMinimal   = ident(model.ChipESP8266, "", "release-minimal", "minimal", "")
	idUnknownV  = ident(model.ChipESP8266, "ESP8266EX", "release-foobar", "unknown", "")
	idESP32     = ident(model.ChipESP32, "ESP32-D0WD-V3", "release-tasmota32", "tasmota", "")
	idESP32DE   = ident(model.ChipESP32, "ESP32-D0WD-V3", "release-tasmota32", "tasmota", "DE")
	idESP32BT   = ident(model.ChipESP32, "ESP32-D0WD-V3", "release-tasmota32-bluetooth", "bluetooth", "")
	idSolo1     = model.Identity{Family: model.ChipESP32, Hardware: "ESP32-S0WD", SingleCore: true, Version: "15.0.0", Image: "release-tasmota32solo1", Variant: "tasmota"}
	idSafeboot  = ident(model.ChipESP32, "ESP32-D0WD-V3", "release-safeboot", "safeboot", "")
	idC3        = ident(model.ChipESP32C3, "ESP32-C3", "release-tasmota32c3", "tasmota", "")
	idS2        = ident(model.ChipESP32S2, "ESP32-S2 v0.0", "release-tasmota32", "tasmota", "")
	idS3        = ident(model.ChipESP32S3, "ESP32-S3", "release-tasmota32s3", "tasmota", "")
	idC2        = ident(model.ChipESP32C2, "ESP32-C2", "release-tasmota32c2", "tasmota", "")
	idC5        = ident(model.ChipESP32C5, "ESP32-C5", "release-tasmota32c5", "tasmota", "")
	idC6        = ident(model.ChipESP32C6, "ESP32-C6", "release-tasmota32c6", "tasmota", "")
	idC61       = ident(model.ChipESP32C61, "ESP32-C61", "release-tasmota32c61", "tasmota", "")
	idP4        = ident(model.ChipESP32P4, "ESP32-P4 v1.0", "release-tasmota32", "tasmota", "")
	idP4R3      = ident(model.ChipESP32P4, "ESP32-P4 v3.1", "release-tasmota32", "tasmota", "")
	idUnknownHW = ident(model.ChipUnknown, "ESP32-H2", "release-tasmota32", "tasmota", "")
)

// TestSelect: Select keeps the detected variant, language and chip build
// unless the user explicitly chose otherwise. A silently changed build loses
// the user's features (sensors, language) or, for the chip build, does not
// boot at all.
func TestSelect(t *testing.T) {
	rel := "16.0.0"
	keep := Choice{Release: rel}
	change := func(v, l string) Choice {
		return Choice{Release: rel, Variant: v, Language: l, AllowVariantChange: true}
	}
	tests := []struct {
		name     string
		id       model.Identity
		c        Choice
		file     string
		variant  string
		lang     string
		minimal  string
		wantErr  error
		wantFam  model.ChipFamily
		skipFam  bool
		skipMeta bool
	}{
		{name: "esp8266 keep", id: idESP8266, c: keep, file: "tasmota.bin.gz", variant: "tasmota"},
		{name: "esp8266 keep german", id: idESP8266DE, c: keep, file: "tasmota-DE.bin.gz", variant: "tasmota", lang: "DE"},
		{name: "esp8266 keep sensors", id: idSensors, c: keep, file: "tasmota-sensors.bin.gz", variant: "sensors"},
		{name: "esp8266 same variant named explicitly", id: idSensors, c: Choice{Variant: "sensors"}, file: "tasmota-sensors.bin.gz", variant: "sensors"},
		{name: "esp8266 same language named explicitly", id: idESP8266DE, c: Choice{Language: "DE"}, file: "tasmota-DE.bin.gz", variant: "tasmota", lang: "DE"},
		{name: "esp8266 to sensors with flag", id: idESP8266, c: change("sensors", ""), file: "tasmota-sensors.bin.gz", variant: "sensors"},
		{name: "esp8266 to lite", id: idESP8266, c: change("lite", ""), file: "tasmota-lite.bin.gz", variant: "lite"},
		{name: "esp8266 to ir", id: idESP8266, c: change("ir", ""), file: "tasmota-ir.bin.gz", variant: "ir"},
		{name: "esp8266 to knx", id: idESP8266, c: change("knx", ""), file: "tasmota-knx.bin.gz", variant: "knx"},
		{name: "esp8266 to display", id: idESP8266, c: change("display", ""), file: "tasmota-display.bin.gz", variant: "display"},
		{name: "esp8266 to zbbridge", id: idESP8266, c: change("zbbridge", ""), file: "tasmota-zbbridge.bin.gz", variant: "zbbridge"},
		{name: "esp8266 to zigbee", id: idESP8266, c: change("zigbee", ""), file: "tasmota-zigbee.bin.gz", variant: "zigbee"},
		{name: "esp8266 to 4M", id: idESP8266, c: change("4M", ""), file: "tasmota-4M.bin.gz", variant: "4M"},
		{name: "esp8266 sensors back to tasmota", id: idSensors, c: change("tasmota", ""), file: "tasmota.bin.gz", variant: "tasmota"},
		{name: "esp8266 to german", id: idESP8266, c: change("", "DE"), file: "tasmota-DE.bin.gz", variant: "tasmota", lang: "DE"},
		{name: "esp8266 german to english", id: idESP8266DE, c: change("", LanguageEnglish), file: "tasmota.bin.gz", variant: "tasmota"},
		{name: "esp8266 german to english sensors", id: idESP8266DE, c: change("sensors", LanguageEnglish), file: "tasmota-sensors.bin.gz", variant: "sensors"},
		{name: "minimal device with explicit variant", id: idMinimal, c: change("tasmota", ""), file: "tasmota.bin.gz", variant: "tasmota"},
		{name: "unknown variant with explicit variant", id: idUnknownV, c: change("sensors", ""), file: "tasmota-sensors.bin.gz", variant: "sensors"},
		{name: "esp32 keep", id: idESP32, c: keep, file: "tasmota32.bin", variant: "tasmota"},
		{name: "esp32 keep german", id: idESP32DE, c: keep, file: "tasmota32-DE.bin", variant: "tasmota", lang: "DE"},
		{name: "esp32 keep bluetooth", id: idESP32BT, c: keep, file: "tasmota32-bluetooth.bin", variant: "bluetooth"},
		{name: "esp32 to display", id: idESP32, c: change("display", ""), file: "tasmota32-display.bin", variant: "display"},
		{name: "esp32 to ir", id: idESP32, c: change("ir", ""), file: "tasmota32-ir.bin", variant: "ir"},
		{name: "esp32 to lvgl", id: idESP32, c: change("lvgl", ""), file: "tasmota32-lvgl.bin", variant: "lvgl"},
		{name: "esp32 to nspanel", id: idESP32, c: change("nspanel", ""), file: "tasmota32-nspanel.bin", variant: "nspanel"},
		{name: "esp32 to webcam", id: idESP32, c: change("webcam", ""), file: "tasmota32-webcam.bin", variant: "webcam"},
		{name: "esp32 to zbbrdgpro", id: idESP32, c: change("zbbrdgpro", ""), file: "tasmota32-zbbrdgpro.bin", variant: "zbbrdgpro"},
		{name: "solo1 keep", id: idSolo1, c: keep, file: "tasmota32solo1.bin", variant: "tasmota"},
		{name: "c3", id: idC3, c: keep, file: "tasmota32c3.bin", variant: "tasmota"},
		{name: "s3", id: idS3, c: keep, file: "tasmota32s3.bin", variant: "tasmota"},
		{name: "c2", id: idC2, c: keep, file: "tasmota32c2.bin", variant: "tasmota"},
		{name: "c5", id: idC5, c: keep, file: "tasmota32c5.bin", variant: "tasmota"},
		{name: "c6", id: idC6, c: keep, file: "tasmota32c6.bin", variant: "tasmota"},
		{name: "c61", id: idC61, c: keep, file: "tasmota32c61.bin", variant: "tasmota"},
		{name: "p4 below revision 3", id: idP4, c: keep, file: "tasmota32p4.bin", variant: "tasmota"},
		{name: "p4 revision 3", id: idP4R3, c: keep, file: "tasmota32p4r3.bin", variant: "tasmota"},
		{name: "c3 with dev image tag", id: ident(model.ChipESP32C3, "ESP32-C3", "85cff52-tasmota32", "tasmota", ""), c: keep, file: "tasmota32c3.bin", variant: "tasmota"},

		// Refusals.
		{name: "unknown hardware", id: idUnknownHW, c: keep, wantErr: ErrUnknownHardware},
		{name: "empty identity", id: model.Identity{}, c: keep, wantErr: ErrUnknownHardware},
		{name: "variant change without flag", id: idESP8266, c: Choice{Variant: "sensors"}, wantErr: ErrVariantChange},
		{name: "language change without flag", id: idESP8266, c: Choice{Language: "DE"}, wantErr: ErrVariantChange},
		{name: "to english without flag", id: idESP8266DE, c: Choice{Language: LanguageEnglish}, wantErr: ErrVariantChange},
		{name: "minimal device keep", id: idMinimal, c: keep, wantErr: ErrVariantChange},
		{name: "minimal device explicit variant without flag", id: idMinimal, c: Choice{Variant: "tasmota"}, wantErr: ErrVariantChange},
		{name: "safeboot device keep", id: idSafeboot, c: keep, wantErr: ErrVariantChange},
		{name: "unknown variant keep", id: idUnknownV, c: keep, wantErr: ErrVariantChange},
		{name: "sensors with german", id: idESP8266DE, c: change("sensors", ""), wantErr: ErrNoSuchVariant},
		{name: "german sensors device", id: ident(model.ChipESP8266, "ESP8266EX", "release-sensors", "sensors", "DE"), c: keep, wantErr: ErrNoSuchVariant},
		{name: "minimal as final target", id: idESP8266, c: change("minimal", ""), wantErr: ErrNoSuchVariant},
		{name: "safeboot as target", id: idESP32, c: change("safeboot", ""), wantErr: ErrNoSuchVariant},
		{name: "unknown as target", id: idESP32, c: change("unknown", ""), wantErr: ErrNoSuchVariant},
		{name: "esp32 variant on esp8266", id: idESP8266, c: change("bluetooth", ""), wantErr: ErrNoSuchVariant},
		{name: "esp8266 variant on esp32", id: idESP32, c: change("sensors", ""), wantErr: ErrNoSuchVariant},
		{name: "esp8266 4M on esp32", id: idESP32, c: change("4M", ""), wantErr: ErrNoSuchVariant},
		{name: "variant on c3", id: idC3, c: change("bluetooth", ""), wantErr: ErrNoSuchVariant},
		{name: "language on c3", id: idC3, c: change("", "DE"), wantErr: ErrNoSuchVariant},
		{name: "variant on solo1", id: idSolo1, c: change("bluetooth", ""), wantErr: ErrNoSuchVariant},
		{name: "language on solo1", id: idSolo1, c: change("", "DE"), wantErr: ErrNoSuchVariant},
		{name: "esp32 variant with language", id: idESP32DE, c: change("lvgl", ""), wantErr: ErrNoSuchVariant},
		{name: "bad language code", id: idESP8266, c: change("", "ZH-TW"), wantErr: ErrNoSuchVariant},
		{name: "lowercase language", id: idESP8266, c: change("", "de"), wantErr: ErrNoSuchVariant},
		{name: "one letter language", id: idESP8266, c: change("", "D"), wantErr: ErrNoSuchVariant},
		{name: "digit language", id: idESP8266, c: change("", "D1"), wantErr: ErrNoSuchVariant},
		{name: "detected odd language", id: ident(model.ChipESP8266, "ESP8266EX", "release-tasmota", "tasmota", "ZH-TW"), c: keep, wantErr: ErrNoSuchVariant},
		{name: "path in variant", id: idESP8266, c: change("../tasmota", ""), wantErr: ErrNoSuchVariant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, err := Select(tt.id, tt.c)
			wantErr(t, err, tt.wantErr)
			if tt.wantErr != nil {
				if sel != (Selection{}) {
					t.Errorf("selection must be empty on error, got %+v", sel)
				}
				return
			}
			wantMin := ""
			if tt.id.Family == model.ChipESP8266 {
				wantMin = "tasmota-minimal.bin.gz"
			}
			want := Selection{Family: tt.id.Family, Variant: tt.variant, Language: tt.lang, Filename: tt.file, Minimal: wantMin}
			if sel != want {
				t.Errorf("Select = %+v, want %+v", sel, want)
			}
			// Selected names must be names the Guard accepts as that family.
			fi, ok := parseFilename(sel.Filename)
			if !ok || fi.family != tt.id.Family || fi.variant != tt.variant || fi.lang != tt.lang || fi.solo1 != tt.id.SingleCore {
				t.Errorf("parseFilename(%q) = %+v, ok=%v", sel.Filename, fi, ok)
			}
		})
	}
}

// TestParseFilename: the file name is the third, independent statement of
// an artifact's family (besides device and header), so the parser must
// accept exactly the upstream naming and nothing that merely looks similar.
func TestParseFilename(t *testing.T) {
	ok := func(fam model.ChipFamily, variant, lang string, solo1, gz bool) fileInfo {
		return fileInfo{family: fam, variant: variant, lang: lang, solo1: solo1, gz: gz}
	}
	valid := map[string]fileInfo{
		"tasmota.bin.gz":           ok(model.ChipESP8266, "tasmota", "", false, true),
		"tasmota-minimal.bin.gz":   ok(model.ChipESP8266, "minimal", "", false, true),
		"tasmota-4M.bin.gz":        ok(model.ChipESP8266, "4M", "", false, true),
		"tasmota-DE.bin.gz":        ok(model.ChipESP8266, "tasmota", "DE", false, true),
		"tasmota.bin":              ok(model.ChipESP8266, "tasmota", "", false, false),
		"tasmota32.bin":            ok(model.ChipESP32, "tasmota", "", false, false),
		"tasmota32-DE.bin":         ok(model.ChipESP32, "tasmota", "DE", false, false),
		"tasmota32-zbbrdgpro.bin":  ok(model.ChipESP32, "zbbrdgpro", "", false, false),
		"tasmota32solo1.bin":       ok(model.ChipESP32, "tasmota", "", true, false),
		"tasmota32-safeboot.bin":   ok(model.ChipESP32, "safeboot", "", false, false),
		"tasmota32c3-safeboot.bin": ok(model.ChipESP32C3, "safeboot", "", false, false),
		"tasmota32s2cdc.bin":       ok(model.ChipESP32S2, "tasmota", "", false, false),
		"tasmota32p4r3.bin":        ok(model.ChipESP32P4, "tasmota", "", false, false),
		"tasmota32c61.bin.gz":      ok(model.ChipESP32C61, "tasmota", "", false, true),
	}
	for name, want := range valid {
		got, isOK := parseFilename(name)
		if !isOK || got != want {
			t.Errorf("parseFilename(%q) = %+v,%v, want %+v", name, got, isOK, want)
		}
	}
	for _, name := range []string{
		"", "tasmota", "tasmota.gz", "tasmota.map.gz", "tasmota.bin.gz.bin.zip",
		"tasmota-.bin.gz", "tasmota-tasmota.bin.gz", "tasmota-foo.bin.gz",
		"tasmota-bluetooth.bin.gz", "tasmota-de.bin.gz", "tasmota-DEU.bin.gz",
		"tasmota32-sensors.bin", "tasmota32-minimal.bin", "tasmota32-tasmota.bin",
		"tasmota32c3-DE.bin", "tasmota32c3-bluetooth.bin", "tasmota32solo1-DE.bin",
		"tasmota32solo1-bluetooth.bin", "tasmota32h2.bin", "tasmota33.bin",
		"Tasmota.bin.gz", "firmware.bin", "xtasmota.bin.gz",
	} {
		if got, isOK := parseFilename(name); isOK {
			t.Errorf("parseFilename(%q) accepted: %+v", name, got)
		}
	}
}

// TestChipBuildDefault: a chip with one upstream build gets exactly that build after an explicit
// variant choice, never another chip's build.
func TestChipBuildDefault(t *testing.T) {
	id := ident(model.ChipESP32C3, "ESP32-C3", "release-foobar", "unknown", "")
	sel, err := Select(id, Choice{Variant: "tasmota", AllowVariantChange: true})
	wantErr(t, err, nil)
	if sel.Filename != "tasmota32c3.bin" {
		t.Errorf("Select = %q, want tasmota32c3.bin", sel.Filename)
	}
}

// TestSelectTwoBuildChips (#85 review H1): ESP32-S2 (s2, s2cdc) and ESP32-P4 (p4, p4r3) have two
// upstream builds that report the same image tag, so neither the tag nor an explicit variant
// choice says which build the device needs. P4 is decided by the chip revision Status 2
// reports, inside the revision ranges each build boots on (ESP-IDF Kconfig.hw_support; Tasmota's
// board esp32p4 = chip_variant esp32p4_es = revisions 0.1..1.99, esp32p4r3 = ESP-IDF default
// 3.1..3.99). Anything else, and every S2, is refused: a wrong-silicon build does not boot.
func TestSelectTwoBuildChips(t *testing.T) {
	keepTasmota := Choice{Variant: "tasmota", AllowVariantChange: true}
	p4 := func(hw, variant string) model.Identity {
		return ident(model.ChipESP32P4, hw, "release-tasmota32", variant, "")
	}
	tests := []struct {
		name    string
		id      model.Identity
		c       Choice
		file    string
		wantErr error
	}{
		{"s2 explicit variant", ident(model.ChipESP32S2, "ESP32-S2 v0.0", "release-tasmota32", "unknown", ""), keepTasmota, "", ErrUnknownHardware},
		{"s2 kept variant", idS2, Choice{}, "", ErrUnknownHardware},
		// The safeboot and minimal auto-default (web updates, firmware fleet needs) picks
		// "tasmota" explicitly; it must not fall back to a default build either.
		{"s2 safeboot auto-default", ident(model.ChipESP32S2, "ESP32-S2 v0.0", "release-safeboot", "safeboot", ""), keepTasmota, "", ErrUnknownHardware},
		{"p4 safeboot auto-default rev 3", p4("ESP32-P4 v3.1", "safeboot"), keepTasmota, "tasmota32p4r3.bin", nil},
		{"p4 safeboot auto-default rev 1", p4("ESP32-P4 v1.0", "safeboot"), keepTasmota, "tasmota32p4.bin", nil},
		{"p4 explicit variant rev 3", p4("ESP32-P4 v3.1", "unknown"), keepTasmota, "tasmota32p4r3.bin", nil},
		{"p4 explicit variant rev 1", p4("ESP32-P4 v1.0", "unknown"), keepTasmota, "tasmota32p4.bin", nil},
		{"p4 rev 0.1 lowest es", p4("ESP32-P4 v0.1", "tasmota"), Choice{}, "tasmota32p4.bin", nil},
		{"p4 rev 1.99 highest es", p4("ESP32-P4 v1.99", "tasmota"), Choice{}, "tasmota32p4.bin", nil},
		{"p4 rev 3.99 highest r3", p4("ESP32-P4 v3.99", "tasmota"), Choice{}, "tasmota32p4r3.bin", nil},
		{"p4 rev 3.10", p4("ESP32-P4 v3.10", "tasmota"), Choice{}, "tasmota32p4r3.bin", nil},
		{"p4 rev 0.0 below es minimum", p4("ESP32-P4 v0.0", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 rev 2.0 in no range", p4("ESP32-P4 v2.0", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 rev 3.0 below r3 minimum", p4("ESP32-P4 v3.0", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 rev 4.0 above r3 maximum", p4("ESP32-P4 v4.0", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 without revision", p4("ESP32-P4", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 minor out of range", p4("ESP32-P4 v1.100", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 malformed revision", p4("ESP32-P4 v3.x", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 trailing text", p4("ESP32-P4 v3.1 eco", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 no minor", p4("ESP32-P4 v3", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 empty major", p4("ESP32-P4 v.1", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 signed minor", p4("ESP32-P4 v3.+1", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 three digit major", p4("ESP32-P4 v100.1", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 two digit major", p4("ESP32-P4 v13.1", "tasmota"), Choice{}, "", ErrUnknownHardware},
		{"p4 huge major", p4("ESP32-P4 v99999999999999999999.1", "tasmota"), Choice{}, "", ErrUnknownHardware},
		// A chip family without a known build must never fall back to the classic "tasmota32.bin".
		{"family without a build", ident(model.ChipFamily("esp32x9"), "", "release-tasmota32", "tasmota", ""), Choice{}, "", ErrUnknownHardware},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, err := Select(tt.id, tt.c)
			wantErr(t, err, tt.wantErr)
			if sel.Filename != tt.file {
				t.Errorf("file = %q, want %q", sel.Filename, tt.file)
			}
		})
	}
}
