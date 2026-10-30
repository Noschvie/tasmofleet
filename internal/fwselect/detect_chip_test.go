package fwselect

import (
	"fmt"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why (#155, split from #127): the chip comes from StatusFWR.Hardware, never from the build name.
// Both payloads are the reporter's. The generic "tasmota32" build on an ESP32-S3 is an S3 (the
// bare tag only keeps the variant unknown, so no update picks a file for it). The solo1 build on
// an "ESP32-U4WDH-S" is a single-core classic ESP32: the Chip row said "Unknown" because the
// hardware table did not know the package.
func TestDetectChipFromHardwareNotFromTheBuildName(t *testing.T) {
	tests := []struct {
		name, status2 string
		family        model.ChipFamily
		single        bool
	}{
		{"S3 on the generic tasmota32 build",
			`{"StatusFWR":{"Version":"16.0.0(tasmota32)","BuildDateTime":"2026-10-16T18:34:59","Core":"3.3.12","SDK":"5.5.5","CpuFrequency":240,"Hardware":"ESP32-S3 v0.2","CR":"448/699"}}`,
			model.ChipESP32S3, false},
		{"U4WDH-S on a solo1 build",
			`{"StatusFWR":{"Version":"16.0.0(solo1)","Hardware":"ESP32-U4WDH-S v3.0"}}`,
			model.ChipESP32, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := Detect(Inputs{Status2: []byte(tt.status2)})
			if err != nil {
				t.Fatal(err)
			}
			if id.Family != tt.family || id.SingleCore != tt.single {
				t.Fatalf("family %q single %v, want %q %v", id.Family, id.SingleCore, tt.family, tt.single)
			}
			if id.Variant != variantUnknown {
				t.Fatalf("variant %q: a bare build tag must stay unknown (never guess a file)", id.Variant)
			}
		})
	}
}

// Why (#289): the Identity card shows StatusFWR under Tasmota's names and with the raw values, so
// detection must keep them as sent and not only the parts TasmoFleet derives from them.
func TestDetectKeepsStatusFWRRaw(t *testing.T) {
	id, err := Detect(Inputs{Status2: []byte(`{"StatusFWR":{"Version":"16.0.0(tasmota32)","BuildDateTime":"2026-10-16T18:34:59","Core":"3.3.12","SDK":"5.5.5","CpuFrequency":240,"Hardware":"ESP32-S3 v0.2","CR":"448/699"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	want := model.FWRFacts{Version: "16.0.0(tasmota32)", BuildDateTime: "2026-10-16T18:34:59", Core: "3.3.12", SDK: "5.5.5", CPUFrequency: 240, CR: "448/699"}
	if id.FWR != want {
		t.Fatalf("FWR %+v, want %+v", id.FWR, want)
	}
	if id.Hardware != "ESP32-S3 v0.2" {
		t.Fatalf("Hardware %q is the raw StatusFWR.Hardware", id.Hardware)
	}
}

// Why (#289 review): CpuFrequency and CR are shown on the Identity card only. A device that sends
// them in another type must still get its identity (chip, hardware, variant): display fields
// never decide whether a device can be identified or updated.
func TestDetectIgnoresWronglyTypedDisplayFields(t *testing.T) {
	const tmpl = `{"StatusFWR":{"Version":"16.0.0(release-tasmota32)","Hardware":"ESP32-S3 v0.2","CpuFrequency":%s,"CR":%s,"Core":%s}}`
	good, err := Detect(Inputs{Status2: []byte(`{"StatusFWR":{"Version":"16.0.0(release-tasmota32)","Hardware":"ESP32-S3 v0.2"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][3]string{{`"80"`, `448`, `3`}, {`80.5`, `"448/699"`, `"3.3.12"`}, {`null`, `[1]`, `{}`}} {
		id, err := Detect(Inputs{Status2: []byte(fmt.Sprintf(tmpl, c[0], c[1], c[2]))})
		if err != nil {
			t.Fatalf("%v: a display field of the wrong type failed the detection: %v", c, err)
		}
		if id.Family != good.Family || id.Hardware != good.Hardware || id.Variant != good.Variant || id.Version != good.Version {
			t.Errorf("%v: identity changed: %+v vs %+v", c, id, good)
		}
	}
}
