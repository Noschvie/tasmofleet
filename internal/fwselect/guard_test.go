package fwselect

import (
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

const rel = "16.0.0"

func art(fn string, fam model.ChipFamily, variant, lang string) model.Artifact {
	return model.Artifact{Release: rel, Filename: fn, Family: fam, Variant: variant, Language: lang, Safeboot: strings.Contains(fn, "safeboot")}
}

func fwURL(fn string) string { return "http://10.0.0.5/fw/" + rel + "/" + fn }

var (
	artTasmota  = art("tasmota.bin.gz", model.ChipESP8266, "tasmota", "")
	artSensors  = art("tasmota-sensors.bin.gz", model.ChipESP8266, "sensors", "")
	artMinimal  = art("tasmota-minimal.bin.gz", model.ChipESP8266, "minimal", "")
	artDE       = art("tasmota-DE.bin.gz", model.ChipESP8266, "tasmota", "DE")
	artESP32    = art("tasmota32.bin", model.ChipESP32, "tasmota", "")
	artESP32DE  = art("tasmota32-DE.bin", model.ChipESP32, "tasmota", "DE")
	artSolo1    = art("tasmota32solo1.bin", model.ChipESP32, "tasmota", "")
	artC3       = art("tasmota32c3.bin", model.ChipESP32C3, "tasmota", "")
	artS2CDC    = art("tasmota32s2cdc.bin", model.ChipESP32S2, "tasmota", "")
	artP4       = art("tasmota32p4.bin", model.ChipESP32P4, "tasmota", "")
	artP4R3     = art("tasmota32p4r3.bin", model.ChipESP32P4, "tasmota", "")
	keepChoice  = Choice{Release: rel}
	changeTo    = func(v string) Choice { return Choice{Release: rel, Variant: v, AllowVariantChange: true} }
	changeLang  = func(l string) Choice { return Choice{Release: rel, Language: l, AllowVariantChange: true} }
	withVariant = func(a model.Artifact, v string) model.Artifact { a.Variant = v; return a }
	withFamily  = func(a model.Artifact, f model.ChipFamily) model.Artifact { a.Family = f; return a }
)

type guardCase struct {
	name string
	id   model.Identity
	a    model.Artifact
	url  string // empty: the correct firmware server URL for a
	c    Choice
	want error
}

func runGuard(t *testing.T, tests []guardCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := tt.url
			if u == "" {
				u = fwURL(tt.a.Filename)
			}
			wantErr(t, Guard(tt.id, tt.a, u, tt.c), tt.want)
		})
	}
}

// TestGuardPasses: with every rule satisfied the guard must let the update
// through, otherwise the fleet can never be updated at all.
func TestGuardPasses(t *testing.T) {
	runGuard(t, []guardCase{
		{name: "esp8266 same variant", id: idESP8266, a: artTasmota, c: keepChoice},
		{name: "esp8266 minimal stage", id: idESP8266, a: artMinimal, c: keepChoice},
		{name: "esp8266 minimal stage for sensors device", id: idSensors, a: artMinimal, c: keepChoice},
		{name: "esp8266 minimal stage for german device", id: idESP8266DE, a: artMinimal, c: keepChoice},
		{name: "esp8266 sensors keep", id: idSensors, a: artSensors, c: keepChoice},
		{name: "esp8266 german keep", id: idESP8266DE, a: artDE, c: keepChoice},
		{name: "esp8266 variant change with flag", id: idESP8266, a: artSensors, c: changeTo("sensors")},
		{name: "esp8266 language change with flag", id: idESP8266, a: artDE, c: changeLang("DE")},
		{name: "esp8266 back to english with flag", id: idESP8266DE, a: artTasmota, c: changeLang(LanguageEnglish)},
		{name: "minimal device with explicit choice", id: idMinimal, a: artTasmota, c: changeTo("tasmota")},
		{name: "esp32", id: idESP32, a: artESP32, c: keepChoice},
		{name: "esp32 german", id: idESP32DE, a: artESP32DE, c: keepChoice},
		{name: "solo1", id: idSolo1, a: artSolo1, c: keepChoice},
		{name: "c3", id: idC3, a: artC3, c: keepChoice},
		{name: "p4 below revision 3", id: idP4, a: artP4, c: keepChoice},
		{name: "p4r3", id: idP4R3, a: artP4R3, c: keepChoice},
		{name: "choice without release", id: idESP8266, a: artTasmota, c: Choice{}},
		{name: "url with port and base path", id: idESP8266, a: artTasmota, c: keepChoice, url: "http://fw.lan:8080/tasmofleet/fw/16.0.0/tasmota.bin.gz"},
		{name: "url exactly 100 characters", id: idESP8266, a: artTasmota, c: keepChoice, url: "http://" + strings.Repeat("h", 100-len("http:///fw/16.0.0/tasmota.bin.gz")) + "/fw/16.0.0/tasmota.bin.gz"},
		{name: "fixture identity", id: fixtureIdentity(t, "bedroom-cabinet"), a: artTasmota, c: keepChoice},
		{name: "fixture identity esp32", id: fixtureIdentity(t, "office-monitor-guest"), a: artESP32, c: keepChoice},
	})
}

func fixtureIdentity(t *testing.T, dev string) model.Identity {
	t.Helper()
	id, err := Detect(fixtureInputs(t, dev))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Rule 1: never update a device whose hardware we do not know.
func TestGuardRule1UnknownHardware(t *testing.T) {
	noVersion := idESP8266
	noVersion.Version = ""
	noVariant := idESP8266
	noVariant.Variant = ""
	oddHW := idESP8266
	oddHW.Hardware = "ESP8299"
	runGuard(t, []guardCase{
		{name: "unknown family", id: idUnknownHW, a: artESP32, c: keepChoice, want: ErrUnknownHardware},
		{name: "empty identity", id: model.Identity{}, a: artTasmota, c: keepChoice, want: ErrUnknownHardware},
		{name: "no version", id: noVersion, a: artTasmota, c: keepChoice, want: ErrUnknownHardware},
		{name: "no variant", id: noVariant, a: artTasmota, c: keepChoice, want: ErrUnknownHardware},
		{name: "hardware string no longer recognised", id: oddHW, a: artTasmota, c: keepChoice, want: ErrUnknownHardware},
	})
}

// Rule 2: the header-proven artifact family must equal the device family.
// This is the rule that prevents bricking by a binary for another CPU.
func TestGuardRule2FamilyMismatch(t *testing.T) {
	wrongHW := idESP8266
	wrongHW.Hardware = "ESP32-D0WD"
	runGuard(t, []guardCase{
		{name: "esp32 artifact to esp8266", id: idESP8266, a: artESP32, c: keepChoice, want: ErrFamilyMismatch},
		{name: "esp8266 artifact to esp32", id: idESP32, a: artTasmota, c: keepChoice, want: ErrFamilyMismatch},
		{name: "c3 artifact to esp32", id: idESP32, a: artC3, c: keepChoice, want: ErrFamilyMismatch},
		{name: "esp32 artifact to c3", id: idC3, a: artESP32, c: keepChoice, want: ErrFamilyMismatch},
		{name: "s2 artifact to p4", id: idP4, a: artS2CDC, c: keepChoice, want: ErrFamilyMismatch},
		{name: "header says esp32 for a c3 name", id: idC3, a: withFamily(artC3, model.ChipESP32), c: keepChoice, want: ErrFamilyMismatch},
		{name: "identity family contradicts its hardware", id: wrongHW, a: artTasmota, c: keepChoice, want: ErrFamilyMismatch},
	})
}

// Rule 3: the file name family must agree with header and device, and the
// URL must be exactly the firmware server URL of this artifact. A name that
// lies, or a URL pointing elsewhere, means the device would fetch something
// the header check never saw.
func TestGuardRule3FilenameMismatch(t *testing.T) {
	lying := artTasmota
	lying.Filename = "tasmota32.bin.gz"
	otherRelease := artTasmota
	otherRelease.Release = "15.0.0"
	noRelease := artTasmota
	noRelease.Release = ""
	runGuard(t, []guardCase{
		{name: "esp32 name with esp8266 header", id: idESP8266, a: lying, c: keepChoice, want: ErrFilenameMismatch},
		{name: "c3 name with esp32 header and device", id: idESP32, a: withFamily(artC3, model.ChipESP32), c: keepChoice, want: ErrFilenameMismatch},
		{name: "unparseable name", id: idESP8266, a: art("firmware.bin.gz", model.ChipESP8266, "tasmota", ""), c: keepChoice, want: ErrFilenameMismatch},
		{name: "unknown chip build name", id: idESP32, a: art("tasmota32h2.bin", model.ChipESP32, "tasmota", ""), c: keepChoice, want: ErrFilenameMismatch},
		{name: "variant metadata disagrees with name", id: idSensors, a: withVariant(artTasmota, "sensors"), c: keepChoice, want: ErrFilenameMismatch},
		{name: "language metadata disagrees with name", id: idESP8266DE, a: art("tasmota.bin.gz", model.ChipESP8266, "tasmota", "DE"), c: keepChoice, want: ErrFilenameMismatch},
		{name: "url basename differs", id: idESP8266, a: artTasmota, c: keepChoice, url: fwURL("tasmota-sensors.bin.gz"), want: ErrFilenameMismatch},
		{name: "url basename differs only by case", id: idESP8266, a: artTasmota, c: keepChoice, url: fwURL("Tasmota.bin.gz"), want: ErrFilenameMismatch},
		{name: "url other release dir", id: idESP8266, a: artTasmota, c: keepChoice, url: "http://10.0.0.5/fw/15.0.0/tasmota.bin.gz", want: ErrFilenameMismatch},
		{name: "url not under /fw/", id: idESP8266, a: artTasmota, c: keepChoice, url: "http://10.0.0.5/16.0.0/tasmota.bin.gz", want: ErrFilenameMismatch},
		{name: "url upstream host", id: idESP8266, a: artTasmota, c: keepChoice, url: "http://ota.tasmota.com/tasmota/release/tasmota.bin.gz", want: ErrFilenameMismatch},
		{name: "url https", id: idESP8266, a: artTasmota, c: keepChoice, url: "https://10.0.0.5/fw/16.0.0/tasmota.bin.gz", want: ErrFilenameMismatch},
		{name: "url no scheme", id: idESP8266, a: artTasmota, c: keepChoice, url: "10.0.0.5/fw/16.0.0/tasmota.bin.gz", want: ErrFilenameMismatch},
		{name: "url no host", id: idESP8266, a: artTasmota, c: keepChoice, url: "http:///fw/16.0.0/tasmota.bin.gz", want: ErrFilenameMismatch},
		{name: "url with query", id: idESP8266, a: artTasmota, c: keepChoice, url: fwURL("tasmota.bin.gz") + "?x=1", want: ErrFilenameMismatch},
		{name: "url with query hiding the name", id: idESP8266, a: artTasmota, c: keepChoice, url: "http://10.0.0.5/evil?/fw/16.0.0/tasmota.bin.gz", want: ErrFilenameMismatch},
		{name: "url with fragment", id: idESP8266, a: artTasmota, c: keepChoice, url: "http://10.0.0.5/x#/fw/16.0.0/tasmota.bin.gz", want: ErrFilenameMismatch},
		{name: "url with space", id: idESP8266, a: artTasmota, c: keepChoice, url: "http://10.0.0.5 /fw/16.0.0/tasmota.bin.gz", want: ErrFilenameMismatch},
		{name: "url with dot segment", id: idESP8266, a: artTasmota, c: keepChoice, url: "http://10.0.0.5/fw/x/../16.0.0/tasmota.bin.gz", want: ErrFilenameMismatch},
		{name: "artifact of another release than chosen", id: idESP8266, a: otherRelease, c: keepChoice, url: "http://10.0.0.5/fw/15.0.0/tasmota.bin.gz", want: ErrFilenameMismatch},
		{name: "artifact without release", id: idESP8266, a: noRelease, c: Choice{}, url: "http://10.0.0.5/fw//tasmota.bin.gz", want: ErrFilenameMismatch},
	})
}

// Rule 4: safeboot and factory images are never OTA targets (factory images
// contain bootloader and partition table; safeboot replaces the recovery
// partition, not the app).
func TestGuardRule4Safeboot(t *testing.T) {
	flagged := artESP32
	flagged.Safeboot = true
	unflagged := art("tasmota32-safeboot.bin", model.ChipESP32, "safeboot", "")
	unflagged.Safeboot = false
	runGuard(t, []guardCase{
		{name: "safeboot flag", id: idESP32, a: flagged, c: keepChoice, want: ErrSafeboot},
		{name: "safeboot name without flag", id: idESP32, a: unflagged, c: keepChoice, want: ErrSafeboot},
		{name: "c3 safeboot", id: idC3, a: art("tasmota32c3-safeboot.bin", model.ChipESP32C3, "safeboot", ""), c: keepChoice, want: ErrSafeboot},
		{name: "factory image", id: idESP32, a: art("tasmota32.factory.bin", model.ChipESP32, "tasmota", ""), c: keepChoice, want: ErrSafeboot},
		{name: "c3 factory image", id: idC3, a: art("tasmota32c3.factory.bin", model.ChipESP32C3, "tasmota", ""), c: keepChoice, want: ErrSafeboot},
	})
}

// Rule 5: ESP8266 takes gzip OTA images, ESP32 the plain app image. The
// wrong container is rejected by the device at best, flashed raw at worst.
func TestGuardRule5Extension(t *testing.T) {
	runGuard(t, []guardCase{
		{name: "esp8266 plain bin", id: idESP8266, a: art("tasmota.bin", model.ChipESP8266, "tasmota", ""), c: keepChoice, want: ErrFilenameMismatch},
		{name: "esp32 gz", id: idESP32, a: art("tasmota32.bin.gz", model.ChipESP32, "tasmota", ""), c: keepChoice, want: ErrFilenameMismatch},
		{name: "c3 gz", id: idC3, a: art("tasmota32c3.bin.gz", model.ChipESP32C3, "tasmota", ""), c: keepChoice, want: ErrFilenameMismatch},
		{name: "map file", id: idESP8266, a: art("tasmota.map.gz", model.ChipESP8266, "tasmota", ""), c: keepChoice, want: ErrFilenameMismatch},
		{name: "zip", id: idESP32, a: art("tasmota32.bin.zip", model.ChipESP32, "tasmota", ""), c: keepChoice, want: ErrFilenameMismatch},
		{name: "no extension", id: idESP32, a: art("tasmota32", model.ChipESP32, "tasmota", ""), c: keepChoice, want: ErrFilenameMismatch},
	})
}

// Rule 6: solo1 and dual-core builds share chip_id 0, so the header cannot
// tell them apart; a dual-core build does not boot on single-core hardware.
func TestGuardRule6SingleCore(t *testing.T) {
	runGuard(t, []guardCase{
		{name: "dual core build to solo1", id: idSolo1, a: artESP32, c: keepChoice, want: ErrFamilyMismatch},
		{name: "solo1 build to dual core", id: idESP32, a: artSolo1, c: keepChoice, want: ErrFamilyMismatch},
		{name: "esp32 variant to solo1", id: idSolo1, a: art("tasmota32-bluetooth.bin", model.ChipESP32, "bluetooth", ""), c: changeTo("bluetooth"), want: ErrFamilyMismatch},
		{name: "single flag contradicts hardware", id: func() model.Identity { i := idESP32; i.SingleCore = true; return i }(), a: artSolo1, c: keepChoice, want: ErrFamilyMismatch},
	})
}

// Rule 7: the artifact must be exactly what Select gives for this device and
// choice; changing variant or language needs the explicit flag. The minimal
// stage is the one exception (it is a transit image, not a variant change).
func TestGuardRule7Variant(t *testing.T) {
	runGuard(t, []guardCase{
		{name: "variant change without flag", id: idESP8266, a: artSensors, c: keepChoice, want: ErrVariantChange},
		{name: "variant named but flag missing", id: idESP8266, a: artSensors, c: Choice{Release: rel, Variant: "sensors"}, want: ErrVariantChange},
		{name: "language change without flag", id: idESP8266, a: artDE, c: keepChoice, want: ErrVariantChange},
		{name: "language dropped without flag", id: idESP8266DE, a: artTasmota, c: keepChoice, want: ErrVariantChange},
		{name: "flag set but artifact is not the chosen variant", id: idESP8266, a: art("tasmota-lite.bin.gz", model.ChipESP8266, "lite", ""), c: changeTo("sensors"), want: ErrVariantChange},
		{name: "flag set without a chosen variant", id: idESP8266, a: artSensors, c: Choice{Release: rel, AllowVariantChange: true}, want: ErrVariantChange},
		{name: "esp32 variant change without flag", id: idESP32, a: art("tasmota32-lvgl.bin", model.ChipESP32, "lvgl", ""), c: keepChoice, want: ErrVariantChange},
		// #85 review H1: an S2 build is never provable, whatever the choice.
		{name: "s2 device gets s2", id: idS2, a: art("tasmota32s2.bin", model.ChipESP32S2, "tasmota", ""), c: changeTo("tasmota"), want: ErrUnknownHardware},
		{name: "s2 device gets s2cdc", id: idS2, a: artS2CDC, c: changeTo("tasmota"), want: ErrUnknownHardware},
		{name: "p4r3 device gets plain p4", id: idP4R3, a: artP4, c: changeTo("tasmota"), want: ErrVariantChange},
		{name: "p4 device gets p4r3", id: idP4, a: artP4R3, c: changeTo("tasmota"), want: ErrVariantChange},
		{name: "minimal device without choice", id: idMinimal, a: artTasmota, c: keepChoice, want: ErrVariantChange},
		{name: "unknown variant device", id: idUnknownV, a: artTasmota, c: keepChoice, want: ErrVariantChange},
		{name: "minimal stage when target is refused", id: idUnknownV, a: artMinimal, c: keepChoice, want: ErrVariantChange},
		{name: "minimal stage with impossible target", id: idESP8266DE, a: artMinimal, c: changeTo("sensors"), want: ErrNoSuchVariant},
		{name: "minimal artifact with wrong metadata language", id: idESP8266, a: art("tasmota-minimal.bin.gz", model.ChipESP8266, "minimal", "DE"), c: keepChoice, want: ErrFilenameMismatch},
	})
}

// Rule 8: Tasmota stores OtaUrl in a 100 character settings slot; a longer
// URL is truncated on the device and fetches something else.
func TestGuardRule8URLLength(t *testing.T) {
	long := "http://" + strings.Repeat("h", 101-len("http:///fw/16.0.0/tasmota.bin.gz")) + "/fw/16.0.0/tasmota.bin.gz"
	if len(long) != 101 {
		t.Fatalf("test URL has %d characters", len(long))
	}
	runGuard(t, []guardCase{
		{name: "101 characters", id: idESP8266, a: artTasmota, c: keepChoice, url: long, want: ErrFilenameMismatch},
		{name: "very long", id: idESP8266, a: artTasmota, c: keepChoice, url: "http://" + strings.Repeat("a", 500) + "/fw/16.0.0/tasmota.bin.gz", want: ErrFilenameMismatch},
	})
}
