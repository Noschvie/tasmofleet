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
	idS2        = ident(model.ChipESP32S2, "ESP32-S2", "release-tasmota32s2", "tasmota", "")
	idS2CDC     = ident(model.ChipESP32S2, "ESP32-S2", "release-tasmota32s2cdc", "tasmota", "")
	idS3        = ident(model.ChipESP32S3, "ESP32-S3", "release-tasmota32s3", "tasmota", "")
	idC2        = ident(model.ChipESP32C2, "ESP32-C2", "release-tasmota32c2", "tasmota", "")
	idC5        = ident(model.ChipESP32C5, "ESP32-C5", "release-tasmota32c5", "tasmota", "")
	idC6        = ident(model.ChipESP32C6, "ESP32-C6", "release-tasmota32c6", "tasmota", "")
	idC61       = ident(model.ChipESP32C61, "ESP32-C61", "release-tasmota32c61", "tasmota", "")
	idP4        = ident(model.ChipESP32P4, "ESP32-P4", "release-tasmota32p4", "tasmota", "")
	idP4R3      = ident(model.ChipESP32P4, "ESP32-P4", "release-tasmota32p4r3", "tasmota", "")
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
		{name: "s2", id: idS2, c: keep, file: "tasmota32s2.bin", variant: "tasmota"},
		{name: "s2cdc keeps cdc build", id: idS2CDC, c: keep, file: "tasmota32s2cdc.bin", variant: "tasmota"},
		{name: "s3", id: idS3, c: keep, file: "tasmota32s3.bin", variant: "tasmota"},
		{name: "c2", id: idC2, c: keep, file: "tasmota32c2.bin", variant: "tasmota"},
		{name: "c5", id: idC5, c: keep, file: "tasmota32c5.bin", variant: "tasmota"},
		{name: "c6", id: idC6, c: keep, file: "tasmota32c6.bin", variant: "tasmota"},
		{name: "c61", id: idC61, c: keep, file: "tasmota32c61.bin", variant: "tasmota"},
		{name: "p4", id: idP4, c: keep, file: "tasmota32p4.bin", variant: "tasmota"},
		{name: "p4r3 keeps r3 build", id: idP4R3, c: keep, file: "tasmota32p4r3.bin", variant: "tasmota"},
		{name: "c3 with dev image tag", id: ident(model.ChipESP32C3, "ESP32-C3", "85cff52-tasmota32c3", "tasmota", ""), c: keep, file: "tasmota32c3.bin", variant: "tasmota"},

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

// TestChipBuildDefault: without a recognisable chip build in the image tag
// (unknown image, chosen variant) the chip's standard build is selected,
// never another chip's build.
func TestChipBuildDefault(t *testing.T) {
	tests := []struct {
		image string
		fam   model.ChipFamily
		want  string
	}{
		{"release-foobar", model.ChipESP32C3, "c3"},
		{"", model.ChipESP32S2, "s2"},
		{"release-tasmota32", model.ChipESP32P4, "p4"},
		{"release-tasmota32s2cdc", model.ChipESP32C3, "c3"}, // other chip's build is ignored
		{"release-tasmota32p4r3", model.ChipESP32P4, "p4r3"},
	}
	for _, tt := range tests {
		if got := chipBuild(model.Identity{Family: tt.fam, Image: tt.image}); got != tt.want {
			t.Errorf("chipBuild(%q, %s) = %q, want %q", tt.image, tt.fam, got, tt.want)
		}
	}
	id := ident(model.ChipESP32C3, "ESP32-C3", "release-foobar", "unknown", "")
	sel, err := Select(id, Choice{Variant: "tasmota", AllowVariantChange: true})
	wantErr(t, err, nil)
	if sel.Filename != "tasmota32c3.bin" {
		t.Errorf("Select = %q, want tasmota32c3.bin", sel.Filename)
	}
}
