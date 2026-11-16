package fwselect

import (
	"errors"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

func info1(version string) []byte {
	return []byte(`{"Info1":{"Module":"Sonoff Basic","Version":"` + version + `","FallbackTopic":"cmnd/x_fb/","GroupTopic":"cmnd/tasmotas/"}}`)
}

// Why (#87): the version a device announces at boot must be read with the same grammar as
// Status 2, otherwise the devices #84/#86 fixed would show Unknown again over MQTT.
func TestDetectInfo1UsesTheStatus2Grammar(t *testing.T) {
	tests := []struct {
		version, semver, image, variant string
	}{
		{"16.0.0(release-tasmota)", "16.0.0", "release-tasmota", "tasmota"},
		{"16.0.0.3(85cff52-tasmota)", "16.0.0.3", "85cff52-tasmota", "tasmota"},
		{"16.0.0(release-solo1)single-core", "16.0.0", "release-solo1", "tasmota"},
		{"15.1.0(release-sensors)", "15.1.0", "release-sensors", "sensors"},
	}
	for _, tt := range tests {
		got, err := DetectInfo1(info1(tt.version), model.Identity{})
		if err != nil {
			t.Fatalf("%s: %v", tt.version, err)
		}
		if got.Version != tt.semver || got.Image != tt.image || got.Variant != tt.variant {
			t.Errorf("%s: version %q image %q variant %q", tt.version, got.Version, got.Image, got.Variant)
		}
	}
}

// Why: INFO1 has no Hardware. The identity it gives must never pass Known(), or Guard could be
// asked to flash on a chip nobody proved.
func TestDetectInfo1AloneIsNeverKnown(t *testing.T) {
	got, err := DetectInfo1(info1("16.0.0(release-tasmota)"), model.Identity{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Known() || got.Family != model.ChipUnknown {
		t.Errorf("identity from INFO1 alone is usable for updates: %+v", got)
	}
}

// Why: when Status 2 gave the hardware earlier, a new boot with another version updates the
// version only, and the image is still checked against that hardware (family mismatch stops).
func TestDetectInfo1KeepsAndChecksTheKnownHardware(t *testing.T) {
	base, err := Detect(Inputs{Status2: status2("15.0.0(release-tasmota)", "ESP8266EX")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DetectInfo1(info1("16.0.0(release-tasmota)"), base)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "16.0.0" || got.Family != model.ChipESP8266 || got.Hardware != "ESP8266EX" || !got.Known() {
		t.Errorf("got %+v", got)
	}
	if _, err := DetectInfo1(info1("16.0.0(release-webcam)"), base); !errors.Is(err, ErrFamilyMismatch) {
		t.Errorf("webcam image on an ESP8266: err = %v, want ErrFamilyMismatch", err)
	}
}

// Why: nothing is guessed from a message that is not an INFO1 with a version of the grammar.
func TestDetectInfo1Rejects(t *testing.T) {
	for name, payload := range map[string][]byte{
		"not json":    []byte(`nope`),
		"no Info1":    []byte(`{"Info2":{"Hostname":"x"}}`),
		"no version":  []byte(`{"Info1":{"Module":"x"}}`),
		"bad version": info1("16.0.0"),
		"empty":       nil,
	} {
		if _, err := DetectInfo1(payload, model.Identity{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// Why (#584): a device left in a self-compiled safeboot must be named the same way over INFO1 as
// over the main page, or a rescue job stops on one transport where it would pass on the other. The
// bare tag is trusted for safeboot only (it names no family and no variant to keep); every other
// bare tag stays a custom build whose variant is never guessed (spec 6.2, #52).
func TestDetectInfo1BareSafebootTag(t *testing.T) {
	esp32 := model.Identity{Family: model.ChipESP32, Hardware: "ESP32-D0WD-V3"}
	solo := model.Identity{Family: model.ChipESP32, Hardware: "ESP32-S0WD", SingleCore: true}
	esp8266 := model.Identity{Family: model.ChipESP8266, Hardware: "ESP8266EX"}
	tests := []struct {
		name, version string
		base          model.Identity
		wantVariant   string
		wantErr       error
	}{
		{"safeboot on an esp32", "16.0.0(safeboot)", esp32, "safeboot", nil},
		{"safeboot on a single-core esp32", "16.0.0(safeboot)single-core", solo, "safeboot", nil},
		{"safeboot without hardware", "16.0.0(safeboot)", model.Identity{}, "safeboot", nil},
		{"safeboot on an esp8266 is another device", "16.0.0(safeboot)", esp8266, "", ErrFamilyMismatch},
		{"official safeboot for comparison", "16.0.0(release-safeboot)", esp32, "safeboot", nil},
		// No other bare tag is loosened: a custom full build is never replaced without a choice.
		{"bare tasmota32 on an esp32", "16.0.0(tasmota32)", esp32, "unknown", nil},
		{"bare solo1 on a single-core esp32", "16.0.0(solo1)single-core", solo, "unknown", nil},
		{"bare minimal on an esp8266", "16.0.0(minimal)", esp8266, "unknown", nil},
		{"bare minimal on an esp32", "16.0.0(minimal)", esp32, "unknown", nil},
		{"bare tasmota on an esp8266", "16.0.0(tasmota)", esp8266, "unknown", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DetectInfo1(info1(tt.version), tt.base)
			wantErr(t, err, tt.wantErr)
			if tt.wantErr != nil {
				return
			}
			if got.Variant != tt.wantVariant || got.Version != "16.0.0" {
				t.Errorf("variant %q version %q, want %q 16.0.0", got.Variant, got.Version, tt.wantVariant)
			}
			_, rest, _ := strings.Cut(tt.version, "(")
			wantImage, _, _ := strings.Cut(rest, ")")
			if got.Image != wantImage {
				t.Errorf("image %q, want the raw tag %q", got.Image, wantImage)
			}
		})
	}
}

// Why (#584, safety rule 1): naming a bare safeboot must not make it updatable. Without stored
// hardware it is never Known() (Guard refuses it), and with hardware Select still demands an
// explicit variant choice, exactly as for the official release-safeboot.
func TestBareSafebootIsNeverKnownAndNeedsAChoice(t *testing.T) {
	alone, err := DetectInfo1(info1("16.0.0(safeboot)"), model.Identity{})
	if err != nil {
		t.Fatal(err)
	}
	if alone.Known() || alone.Family != model.ChipUnknown {
		t.Errorf("bare safeboot from INFO1 alone is usable for updates: %+v", alone)
	}
	// The stored identity from before: hardware and a known language, as Select needs them.
	idESP32Stored := model.Identity{Family: model.ChipESP32, Hardware: "ESP32-D0WD-V3", LanguageKnown: true}
	onChip, err := DetectInfo1(info1("16.0.0(safeboot)"), idESP32Stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Select(onChip, Choice{Release: rel}); !errors.Is(err, ErrVariantChoiceNeeded) {
		t.Errorf("Select on a bare safeboot without a choice: err = %v, want ErrVariantChoiceNeeded", err)
	}
}
