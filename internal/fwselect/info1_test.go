package fwselect

import (
	"errors"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
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
