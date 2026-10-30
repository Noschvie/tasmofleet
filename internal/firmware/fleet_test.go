package firmware

import (
	"slices"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func dev(id int, name string, id2 model.Identity) model.Device {
	return model.Device{ID: model.DeviceID(id), Name: name, Identity: id2}
}

// Why: Prefetch downloads exactly what the fleet needs. A mixed fleet is the
// real case: the ESP8266 needs its build AND tasmota-minimal (the staged path),
// ESP32 and ESP32-C3 need their own chip builds and nothing else, and a device
// that cannot be updated must not drag a download in.
func TestFleetNeedsMixedFleet(t *testing.T) {
	rel := Release{Version: "16.1.0", Files: []string{
		"tasmota.bin.gz", "tasmota-sensors.bin.gz", "tasmota-minimal.bin.gz", "tasmota32.bin", "tasmota32c3.bin", "tasmota32s3.bin",
	}}
	devs := []model.Device{
		dev(1, "lamp-1m", model.Identity{LanguageKnown: true, Family: model.ChipESP8266, Version: "16.0.0", Variant: "tasmota", ProgramFlashSizeKB: 1024}),
		dev(2, "dimmer", model.Identity{LanguageKnown: true, Family: model.ChipESP32, Version: "16.1.0", Variant: "tasmota"}),
		dev(3, "c3-node", model.Identity{LanguageKnown: true, Family: model.ChipESP32C3, Version: "16.2.0", Variant: "tasmota", Image: "tasmota32c3"}),
		dev(4, "plug-sensors", model.Identity{LanguageKnown: true, Family: model.ChipESP8266, Version: "15.0.0", Variant: "sensors"}),
		dev(5, "mystery", model.Identity{}),
		dev(6, "excluded", model.Identity{LanguageKnown: true, Family: model.ChipESP32S3, Version: "15.0.0", Variant: "tasmota", Image: "tasmota32s3"}),
		dev(7, "stuck-minimal", model.Identity{LanguageKnown: true, Family: model.ChipESP8266, Version: "16.0.0", Variant: "minimal"}),
	}
	devs[5].OTAExcluded = true

	n := FleetNeeds(rel, devs)

	want := []string{"tasmota-minimal.bin.gz", "tasmota-sensors.bin.gz", "tasmota.bin.gz", "tasmota32.bin", "tasmota32c3.bin"}
	if !slices.Equal(n.Files, want) {
		t.Fatalf("files = %v, want %v", n.Files, want)
	}
	if len(n.Missing) != 0 {
		t.Errorf("missing = %v", n.Missing)
	}
	byName := map[string]DeviceNeed{}
	for _, d := range n.Devices {
		byName[d.Name] = d
	}
	if got := byName["lamp-1m"].Files; !slices.Equal(got, []string{"tasmota.bin.gz", "tasmota-minimal.bin.gz"}) {
		t.Errorf("1 MB ESP8266 files = %v", got)
	}
	if got := byName["plug-sensors"].Files; !slices.Equal(got, []string{"tasmota-sensors.bin.gz", "tasmota-minimal.bin.gz"}) {
		t.Errorf("variant must be kept: %v", got)
	}
	if got := byName["dimmer"].Files; !slices.Equal(got, []string{"tasmota32.bin"}) {
		t.Errorf("ESP32 files = %v (no minimal image on ESP32)", got)
	}
	if got := byName["c3-node"].Files; !slices.Equal(got, []string{"tasmota32c3.bin"}) {
		t.Errorf("C3 files = %v", got)
	}
	if got := byName["stuck-minimal"].Files; !slices.Equal(got, []string{"tasmota.bin.gz", "tasmota-minimal.bin.gz"}) {
		t.Errorf("a device stuck on minimal gets the standard build: %v", got)
	}
	if d := byName["mystery"]; len(d.Files) != 0 || d.Skip == "" || d.Position != PosUnknown {
		t.Errorf("unknown hardware must be skipped with a reason: %+v", d)
	}
	if d := byName["excluded"]; len(d.Files) != 0 || !strings.Contains(d.Skip, "excluded") {
		t.Errorf("excluded device: %+v", d)
	}
	for name, pos := range map[string]Position{"lamp-1m": PosBelow, "dimmer": PosAt, "c3-node": PosAbove, "mystery": PosUnknown} {
		if byName[name].Position != pos {
			t.Errorf("%s position = %s, want %s", name, byName[name].Position, pos)
		}
	}
	if used := n.UsedBy("tasmota-minimal.bin.gz"); len(used) != 3 {
		t.Errorf("minimal used by %d devices, want 3 ESP8266", len(used))
	}
}

// Why (#85 review H1): a device in safeboot is sent "tasmota" by default. On ESP32-S2 that default
// cannot be resolved to a build (s2 and s2cdc report the same name), so the device is skipped
// instead of prefetching a build that may not be its own; ESP32-P4 gets the build of its chip
// revision.
func TestFleetNeedsSafebootTwoBuildChips(t *testing.T) {
	rel := Release{Version: "16.1.0", Files: []string{"tasmota32s2.bin", "tasmota32s2cdc.bin", "tasmota32p4.bin", "tasmota32p4r3.bin"}}
	devs := []model.Device{
		dev(1, "s2-safeboot", model.Identity{LanguageKnown: true, Family: model.ChipESP32S2, Hardware: "ESP32-S2 v0.0", Version: "16.0.0", Image: "release-safeboot", Variant: "safeboot"}),
		dev(2, "p4-safeboot", model.Identity{LanguageKnown: true, Family: model.ChipESP32P4, Hardware: "ESP32-P4 v3.1", Version: "16.0.0", Image: "release-safeboot", Variant: "safeboot"}),
	}
	n := FleetNeeds(rel, devs)
	if !slices.Equal(n.Files, []string{"tasmota32p4r3.bin"}) {
		t.Fatalf("files = %v, want only the P4 r3 build", n.Files)
	}
	for _, d := range n.Devices {
		if d.Name == "s2-safeboot" && (len(d.Files) != 0 || d.Skip == "") {
			t.Errorf("S2 in safeboot must be skipped with a reason: %+v", d)
		}
	}
}

// Why: a release that does not offer a file the fleet needs must be visible,
// not silently counted as "covered".
func TestFleetNeedsReportsFilesTheReleaseLacks(t *testing.T) {
	rel := Release{Version: "15.0.0", Files: []string{"tasmota.bin.gz"}}
	n := FleetNeeds(rel, []model.Device{
		dev(1, "a", model.Identity{LanguageKnown: true, Family: model.ChipESP8266, Version: "14.0.0", Variant: "tasmota"}),
	})
	if !slices.Equal(n.Files, []string{"tasmota.bin.gz"}) || !slices.Equal(n.Missing, []string{"tasmota-minimal.bin.gz"}) {
		t.Fatalf("files %v missing %v", n.Files, n.Missing)
	}
}

// Why: the device list and the firmware page must agree on "update available".
func TestUpdateAvailable(t *testing.T) {
	rels := []Release{{Version: "15.3.0"}, {Version: "16.0.0"}, {Version: "9.5.0"}}
	latest := Latest(rels)
	if latest != "16.0.0" {
		t.Fatalf("latest = %s (numeric, not lexical)", latest)
	}
	for v, want := range map[string]bool{"15.3.0": true, "9.5.0": true, "16.0.0": false, "16.1.0": false, "": false} {
		if got := UpdateAvailable(latest, model.Identity{Version: v}); got != want {
			t.Errorf("UpdateAvailable(%q) = %v", v, got)
		}
	}
	if UpdateAvailable("", model.Identity{Version: "1.0.0"}) {
		t.Error("no index, no update")
	}
}
