package fwselect

import (
	"errors"
	"slices"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// idS2Real is an ESP32-S2 as Detect reads it from a device: the shared image
// tag "tasmota32" cannot name the build, so the variant is "unknown" (§6.2).
var idS2Real = ident(model.ChipESP32S2, "ESP32-S2", "release-tasmota32", "unknown", "")

// Self-compiled or custom S2 images stay "unknown" and must never be replaced without an explicit choice.
var (
	idS2Bare   = ident(model.ChipESP32S2, "ESP32-S2", "tasmota32", "unknown", "")
	idS2Bare2  = ident(model.ChipESP32S2, "ESP32-S2", "tasmota32s2", "unknown", "")
	idS2Custom = ident(model.ChipESP32S2, "ESP32-S2", "release-mycustom", "unknown", "")
)

func buildChoice(build string) Choice { return Choice{Release: rel, Build: build} }

// TestSelectBuild: the build is the user's explicit decision. Without it an
// ESP32-S2 stays refused (nothing the device reports names the build, #85), a
// build that does not belong to the chip is never accepted, and a build that
// contradicts a provable chip revision is refused: a wrong chip build does not
// boot.
func TestSelectBuild(t *testing.T) {
	tests := []struct {
		name    string
		id      model.Identity
		c       Choice
		file    string
		variant string
		wantErr error
	}{
		{name: "s2 without build is refused", id: idS2Real, c: Choice{Release: rel}, wantErr: ErrBuildChoiceNeeded},
		{name: "s2 without build with variant change is still refused", id: idS2Real, c: Choice{Release: rel, Variant: "tasmota", AllowVariantChange: true}, wantErr: ErrBuildChoiceNeeded},
		{name: "s2 custom bare tag with a build alone is not replaced", id: idS2Bare, c: buildChoice("s2"), wantErr: ErrVariantChange},
		{name: "s2 custom bare s2 tag with a build alone is not replaced", id: idS2Bare2, c: buildChoice("s2cdc"), wantErr: ErrVariantChange},
		{name: "s2 custom image with a build alone is not replaced", id: idS2Custom, c: buildChoice("s2"), wantErr: ErrVariantChange},
		{name: "s2 bare tag with explicit variant change and build", id: idS2Bare, c: Choice{Release: rel, Variant: "tasmota", AllowVariantChange: true, Build: "s2"}, file: "tasmota32s2.bin", variant: "tasmota"},
		{name: "s2 custom image with explicit variant change and build", id: idS2Custom, c: Choice{Release: rel, Variant: "tasmota", AllowVariantChange: true, Build: "s2cdc"}, file: "tasmota32s2cdc.bin", variant: "tasmota"},
		{name: "s2 explicit s2", id: idS2Real, c: buildChoice("s2"), file: "tasmota32s2.bin", variant: "tasmota"},
		{name: "s2 explicit s2cdc", id: idS2Real, c: buildChoice("s2cdc"), file: "tasmota32s2cdc.bin", variant: "tasmota"},
		{name: "s2 build with explicit plain variant", id: idS2Real, c: Choice{Release: rel, Build: "s2cdc", Variant: "tasmota"}, file: "tasmota32s2cdc.bin", variant: "tasmota"},
		{name: "s2 build from a device in minimal", id: ident(model.ChipESP32S2, "ESP32-S2", "release-minimal", "minimal", ""), c: Choice{Release: rel, Build: "s2", Variant: "tasmota", AllowVariantChange: true}, file: "tasmota32s2.bin", variant: "tasmota"},
		{name: "s2 build with another variant", id: idS2Real, c: Choice{Release: rel, Build: "s2", Variant: "bluetooth", AllowVariantChange: true}, wantErr: ErrNoSuchVariant},
		{name: "s2 build with a language", id: idS2Real, c: Choice{Release: rel, Build: "s2", Language: "DE", AllowVariantChange: true}, wantErr: ErrNoSuchVariant},
		{name: "s2 with the build of another chip", id: idS2Real, c: buildChoice("c3"), wantErr: ErrNoSuchVariant},
		{name: "s2 with a p4 build", id: idS2Real, c: buildChoice("p4"), wantErr: ErrNoSuchVariant},
		{name: "s2 with a path as build", id: idS2Real, c: buildChoice("../s2"), wantErr: ErrNoSuchVariant},
		{name: "s2 with an empty-looking build", id: idS2Real, c: buildChoice(" s2"), wantErr: ErrNoSuchVariant},
		{name: "classic esp32 takes no build", id: idESP32, c: buildChoice("s2"), wantErr: ErrNoSuchVariant},
		{name: "classic esp32 build empty-named solo1", id: idESP32, c: buildChoice("solo1"), wantErr: ErrNoSuchVariant},
		{name: "esp8266 takes no build", id: idESP8266, c: buildChoice("s2"), wantErr: ErrNoSuchVariant},
		{name: "s3 takes no build even its own", id: idS3, c: buildChoice("s3"), wantErr: ErrNoSuchVariant},
		{name: "c3 takes no build", id: idC3, c: buildChoice("s2cdc"), wantErr: ErrNoSuchVariant},
		{name: "p4 revision 3.1 given p4 is refused", id: idP4R3, c: buildChoice("p4"), wantErr: ErrNoSuchVariant},
		{name: "p4 revision 1.0 given p4r3 is refused", id: idP4, c: buildChoice("p4r3"), wantErr: ErrNoSuchVariant},
		{name: "p4 revision 3.1 given its own build is fine", id: idP4R3, c: buildChoice("p4r3"), file: "tasmota32p4r3.bin", variant: "tasmota"},
		{name: "p4 outside known revisions stays refused with a build", id: ident(model.ChipESP32P4, "ESP32-P4 v2.0", "release-tasmota32", "tasmota", ""), c: buildChoice("p4"), wantErr: ErrUnknownHardware},
		{name: "p4 without revision stays refused with a build", id: ident(model.ChipESP32P4, "ESP32-P4", "release-tasmota32", "tasmota", ""), c: buildChoice("p4r3"), wantErr: ErrUnknownHardware},
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
			if sel.Filename != tt.file || sel.Variant != tt.variant {
				t.Errorf("Select = %+v, want %s / %s", sel, tt.file, tt.variant)
			}
		})
	}
}

// TestBuildChoiceNeededIsUnknownHardware: callers that only know the #86
// refusal (ErrUnknownHardware) must keep refusing an S2 without a choice.
func TestBuildChoiceNeededIsUnknownHardware(t *testing.T) {
	_, err := Select(idS2Real, Choice{Release: rel})
	if !errors.Is(err, ErrBuildChoiceNeeded) || !errors.Is(err, ErrUnknownHardware) {
		t.Fatalf("err = %v, want both ErrBuildChoiceNeeded and ErrUnknownHardware", err)
	}
	if _, err := Select(idP4R3, Choice{Release: rel, Variant: "tasmota", AllowVariantChange: true}); errors.Is(err, ErrBuildChoiceNeeded) {
		t.Errorf("P4 is decided by its revision, never a build choice: %v", err)
	}
}

// TestBuildOptions: the picker offers exactly the S2 builds. Offering a build
// for any other chip would invite a choice Select must refuse.
func TestBuildOptions(t *testing.T) {
	for _, tt := range []struct {
		name string
		id   model.Identity
		want []string
	}{
		{"s2", idS2Real, []string{"s2", "s2cdc"}},
		{"p4 is decided by its revision", idP4R3, nil},
		{"classic esp32", idESP32, nil},
		{"s3", idS3, nil},
		{"esp8266", idESP8266, nil},
		{"unknown chip", idUnknownHW, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuildOptions(tt.id); !slices.Equal(got, tt.want) {
				t.Errorf("BuildOptions = %v, want %v", got, tt.want)
			}
		})
	}
	// Every offered build must be one Select accepts.
	for _, b := range BuildOptions(idS2Real) {
		if _, err := Select(idS2Real, buildChoice(b)); err != nil {
			t.Errorf("offered build %q is refused: %v", b, err)
		}
	}
}

// TestDetectedVariant: after an update the device reports what Detect makes
// of its image tag. For a chip with two builds that is "unknown" whatever was
// installed; comparing the plain selection with it ended every correct S2
// update as "not taken". Every other chip reports the selected variant.
func TestDetectedVariant(t *testing.T) {
	for _, tt := range []struct {
		name string
		sel  Selection
		want string
	}{
		{"s2 reports the shared tag", Selection{Family: model.ChipESP32S2, Variant: "tasmota"}, "unknown"},
		{"p4 reports the shared tag", Selection{Family: model.ChipESP32P4, Variant: "tasmota"}, "unknown"},
		{"classic esp32", Selection{Family: model.ChipESP32, Variant: "tasmota"}, "tasmota"},
		{"classic esp32 bluetooth", Selection{Family: model.ChipESP32, Variant: "bluetooth"}, "bluetooth"},
		{"esp8266 sensors", Selection{Family: model.ChipESP8266, Variant: "sensors"}, "sensors"},
		{"c3", Selection{Family: model.ChipESP32C3, Variant: "tasmota"}, "tasmota"},
		{"s3", Selection{Family: model.ChipESP32S3, Variant: "tasmota"}, "tasmota"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectedVariant(tt.sel); got != tt.want {
				t.Errorf("DetectedVariant = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDetectedVariantMatchesDetect: the helper must agree with what Detect
// really reads back from the tag of every selection, otherwise verify would
// accept or reject on a made-up expectation.
func TestDetectedVariantMatchesDetect(t *testing.T) {
	for _, tt := range []struct {
		id model.Identity
		c  Choice
	}{
		{idS2Real, buildChoice("s2")},
		{idS2Real, buildChoice("s2cdc")},
		{idC3, Choice{Release: rel}},
		{idS3, Choice{Release: rel}},
		{idESP32, Choice{Release: rel}},
		{idP4R3, Choice{Release: rel}},
	} {
		sel, err := Select(tt.id, tt.c)
		if err != nil {
			t.Fatal(err)
		}
		hw := tt.id.Hardware
		id, err := Detect(Inputs{Status2: status2("16.0.0(release-tasmota32)", hw)})
		if err != nil {
			t.Fatal(err)
		}
		if got := DetectedVariant(sel); got != id.Variant {
			t.Errorf("%s: DetectedVariant = %q, Detect reads %q", sel.Filename, got, id.Variant)
		}
	}
}
