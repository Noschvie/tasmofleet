package fwselect

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

var htmlLangRe = regexp.MustCompile(`<html lang="([^"]*)"`)

// fixtureInputs builds Detect inputs from a captured device directory, the
// way the tasmota client hands them over.
func fixtureInputs(t *testing.T, dev string) Inputs {
	t.Helper()
	root := readFixture(t, filepath.Join("devices", dev, "root.html"))
	m := htmlLangRe.FindSubmatch(root)
	if m == nil {
		t.Fatalf("%s: no <html lang> in root.html", dev)
	}
	return Inputs{
		Status2: readFixture(t, filepath.Join("devices", dev, "status-2.json")),
		Status4: readFixture(t, filepath.Join("devices", dev, "status-4.json")),
		Root:    &RootInfo{Lang: string(m[1])},
	}
}

// TestDetectFixtures has one row per captured device. These are the devices
// of the reference fleet: if detection is wrong for any of them, the fleet
// gets the wrong firmware or the wrong plan (1 MB devices must be staged).
func TestDetectFixtures(t *testing.T) {
	type want struct {
		hw                   string
		family               model.ChipFamily
		variant, image       string
		flash, program, free int
	}
	esp8285 := want{"ESP8285N08", model.ChipESP8266, "tasmota", "release-tasmota", 1024, 1024, 352}
	esp8266 := want{"ESP8266EX", model.ChipESP8266, "tasmota", "release-tasmota", 1024, 1024, 352}
	rows := map[string]want{
		"bedroom-cabinet":        esp8285,
		"livingroom-readinglamp": esp8285,
		"kitchen-spotlight":      esp8266,
		"mr-bambu-plug":          esp8266,
		"prusa-plug":             esp8266,
		"office-monitor-guest":   {"ESP32-D0WDQ6 v1.0", model.ChipESP32, "tasmota", "release-tasmota32", 4096, 4096, 697},
	}

	// Every captured device must have a row, so a new fixture cannot slip in untested.
	entries, err := os.ReadDir(filepath.Join(fixturesDir, "devices"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := rows[e.Name()]; e.IsDir() && !ok {
			t.Errorf("fixture devices/%s has no row in TestDetectFixtures", e.Name())
		}
	}

	for dev, w := range rows {
		t.Run(dev, func(t *testing.T) {
			id, err := Detect(fixtureInputs(t, dev))
			wantErr(t, err, nil)
			if id.Family != w.family || id.Hardware != w.hw || id.SingleCore {
				t.Errorf("family/hw/single = %q/%q/%v, want %q/%q/false", id.Family, id.Hardware, id.SingleCore, w.family, w.hw)
			}
			if id.Version != "16.0.0" || id.Image != w.image || id.Variant != w.variant || id.Language != "" {
				t.Errorf("version/image/variant/lang = %q/%q/%q/%q", id.Version, id.Image, id.Variant, id.Language)
			}
			if id.FlashSizeKB != w.flash || id.ProgramFlashSizeKB != w.program || id.FreeKB != w.free {
				t.Errorf("flash/program/free = %d/%d/%d, want %d/%d/%d", id.FlashSizeKB, id.ProgramFlashSizeKB, id.FreeKB, w.flash, w.program, w.free)
			}
			if !id.Known() {
				t.Error("fixture identity must be Known(), otherwise the device can never be updated")
			}
		})
	}
}

func status2(version, hw string) []byte {
	return []byte(`{"StatusFWR":{"Version":"` + version + `","Hardware":"` + hw + `"}}`)
}

// TestDetectImageTags covers the §6.2 table: the variant decides which build
// is kept on update, and an image whose family disagrees with the hardware
// means something is badly wrong and must stop everything.
func TestDetectImageTags(t *testing.T) {
	tests := []struct {
		name, version, hw string
		lang              string
		wantVariant       string
		wantLang          string
		wantFamily        model.ChipFamily
		wantSingle        bool
		wantSemver        string
		wantErr           error
	}{
		{"esp8266 tasmota", "16.0.0(release-tasmota)", "ESP8266EX", "en", "tasmota", "", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 dev build", "16.0.0.3(85cff52-tasmota)", "ESP8266EX", "", "tasmota", "", model.ChipESP8266, false, "16.0.0.3", nil},
		{"esp8266 minimal", "16.0.0(release-minimal)", "ESP8285N08", "", "minimal", "", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 lite", "16.0.0(release-lite)", "ESP8266EX", "", "lite", "", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 sensors", "16.0.0(release-sensors)", "ESP8266EX", "", "sensors", "", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 knx", "16.0.0(release-knx)", "ESP8266EX", "", "knx", "", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 display", "16.0.0(release-display)", "ESP8266EX", "", "display", "", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 ir", "16.0.0(release-ir)", "ESP8266EX", "", "ir", "", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 zbbridge", "16.0.0(release-zbbridge)", "ESP8266EX", "", "zbbridge", "", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 zigbee", "16.0.0(release-zigbee)", "ESP8266EX", "", "zigbee", "", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 4M", "16.0.0(release-4M)", "ESP8266EX", "", "4M", "", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 german", "16.0.0(release-tasmota)", "ESP8266EX", "de", "tasmota", "DE", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 lang upper", "16.0.0(release-tasmota)", "ESP8266EX", "FR", "tasmota", "FR", model.ChipESP8266, false, "16.0.0", nil},
		{"esp8266 lang EN upper", "16.0.0(release-tasmota)", "ESP8266EX", "EN", "tasmota", "", model.ChipESP8266, false, "16.0.0", nil},
		{"esp32 tasmota32", "16.0.0(release-tasmota32)", "ESP32-D0WD-V3", "", "tasmota", "", model.ChipESP32, false, "16.0.0", nil},
		{"esp32 german", "16.0.0(release-tasmota32)", "ESP32-D0WD-V3", "de", "tasmota", "DE", model.ChipESP32, false, "16.0.0", nil},
		{"esp32 solo1", "16.0.0(release-tasmota32solo1)", "ESP32-S0WD", "", "tasmota", "", model.ChipESP32, true, "16.0.0", nil},
		{"esp32 bluetooth", "16.0.0(release-tasmota32-bluetooth)", "ESP32-D0WD", "", "bluetooth", "", model.ChipESP32, false, "16.0.0", nil},
		{"esp32 display", "16.0.0(release-tasmota32-display)", "ESP32-D0WD", "", "display", "", model.ChipESP32, false, "16.0.0", nil},
		{"esp32 ir", "16.0.0(release-tasmota32-ir)", "ESP32-D0WD", "", "ir", "", model.ChipESP32, false, "16.0.0", nil},
		{"esp32 lvgl", "16.0.0(release-tasmota32-lvgl)", "ESP32-D0WD", "", "lvgl", "", model.ChipESP32, false, "16.0.0", nil},
		{"esp32 nspanel", "16.0.0(release-tasmota32-nspanel)", "ESP32-D0WD", "", "nspanel", "", model.ChipESP32, false, "16.0.0", nil},
		{"esp32 webcam", "16.0.0(release-tasmota32-webcam)", "ESP32-D0WD", "", "webcam", "", model.ChipESP32, false, "16.0.0", nil},
		{"esp32 zbbrdgpro", "16.0.0(release-tasmota32-zbbrdgpro)", "ESP32-D0WD", "", "zbbrdgpro", "", model.ChipESP32, false, "16.0.0", nil},
		{"s2", "16.0.0(release-tasmota32s2)", "ESP32-S2", "", "tasmota", "", model.ChipESP32S2, false, "16.0.0", nil},
		{"s2cdc", "16.0.0(release-tasmota32s2cdc)", "ESP32-S2", "", "tasmota", "", model.ChipESP32S2, false, "16.0.0", nil},
		{"s3", "16.0.0(release-tasmota32s3)", "ESP32-S3", "", "tasmota", "", model.ChipESP32S3, false, "16.0.0", nil},
		{"c2", "16.0.0(release-tasmota32c2)", "ESP32-C2", "", "tasmota", "", model.ChipESP32C2, false, "16.0.0", nil},
		{"c3", "16.0.0(release-tasmota32c3)", "ESP32-C3", "", "tasmota", "", model.ChipESP32C3, false, "16.0.0", nil},
		{"c5", "16.0.0(release-tasmota32c5)", "ESP32-C5", "", "tasmota", "", model.ChipESP32C5, false, "16.0.0", nil},
		{"c6", "16.0.0(release-tasmota32c6)", "ESP32-C6", "", "tasmota", "", model.ChipESP32C6, false, "16.0.0", nil},
		{"c61", "16.0.0(release-tasmota32c61)", "ESP32-C61", "", "tasmota", "", model.ChipESP32C61, false, "16.0.0", nil},
		{"p4", "16.0.0(release-tasmota32p4)", "ESP32-P4", "", "tasmota", "", model.ChipESP32P4, false, "16.0.0", nil},
		{"p4r3", "16.0.0(release-tasmota32p4r3)", "ESP32-P4", "", "tasmota", "", model.ChipESP32P4, false, "16.0.0", nil},
		{"esp32 safeboot", "16.0.0(release-safeboot)", "ESP32-D0WD", "", "safeboot", "", model.ChipESP32, false, "16.0.0", nil},
		{"c3 safeboot", "16.0.0(release-safeboot)", "ESP32-C3", "", "safeboot", "", model.ChipESP32C3, false, "16.0.0", nil},
		// Unknown image tags: identity stays usable for display but the
		// variant is "unknown", which Select/Guard refuse without a choice.
		{"unknown tag", "16.0.0(release-foobar)", "ESP8266EX", "", "unknown", "", model.ChipESP8266, false, "16.0.0", nil},
		{"unknown esp32 variant", "16.0.0(release-tasmota32-foobar)", "ESP32-D0WD", "", "unknown", "", model.ChipESP32, false, "16.0.0", nil},
		{"unknown chip suffix", "16.0.0(release-tasmota32h2)", "ESP32-D0WD", "", "unknown", "", model.ChipESP32, false, "16.0.0", nil},
		// Self-compiled builds report the bare environment name (issue #52: "16.0.0(tasmota32)").
		{"self-compiled esp8266", "16.0.0(tasmota)", "ESP8266EX", "", "tasmota", "", model.ChipESP8266, false, "16.0.0", nil},
		{"self-compiled esp32", "16.0.0(tasmota32)", "ESP32-D0WD-V3", "", "tasmota", "", model.ChipESP32, false, "16.0.0", nil},
		{"self-compiled s3", "16.0.0(tasmota32s3)", "ESP32-S3", "", "tasmota", "", model.ChipESP32S3, false, "16.0.0", nil},
		{"self-compiled plain image on c3 stays unknown", "16.0.0(tasmota32)", "ESP32-C3", "", "unknown", "", model.ChipESP32C3, false, "16.0.0", nil},
		{"self-compiled custom name", "16.0.0(my-build)", "ESP32-D0WD", "", "unknown", "", model.ChipESP32, false, "16.0.0", nil},
		{"self-compiled custom bare name", "16.0.0(mybuild)", "ESP32-D0WD", "", "unknown", "", model.ChipESP32, false, "16.0.0", nil},
		// Unknown hardware is not an error but never Known().
		{"unknown hardware", "16.0.0(release-tasmota32)", "ESP32-H2", "", "tasmota", "", model.ChipUnknown, false, "16.0.0", nil},
		// Family mismatches between image tag and hardware must stop.
		{"esp32 image on esp8266", "16.0.0(release-tasmota32)", "ESP8266EX", "", "", "", "", false, "", ErrFamilyMismatch},
		{"esp8266 image on esp32", "16.0.0(release-tasmota)", "ESP32-D0WD", "", "", "", "", false, "", ErrFamilyMismatch},
		{"esp8266 variant on esp32", "16.0.0(release-sensors)", "ESP32-D0WD", "", "", "", "", false, "", ErrFamilyMismatch},
		{"minimal on esp32", "16.0.0(release-minimal)", "ESP32-C3", "", "", "", "", false, "", ErrFamilyMismatch},
		{"c3 image on esp32", "16.0.0(release-tasmota32c3)", "ESP32-D0WD", "", "", "", "", false, "", ErrFamilyMismatch},
		{"esp32 image on c3", "16.0.0(release-tasmota32)", "ESP32-C3", "", "", "", "", false, "", ErrFamilyMismatch},
		{"s2cdc image on s3", "16.0.0(release-tasmota32s2cdc)", "ESP32-S3", "", "", "", "", false, "", ErrFamilyMismatch},
		{"p4r3 image on c6", "16.0.0(release-tasmota32p4r3)", "ESP32-C6", "", "", "", "", false, "", ErrFamilyMismatch},
		{"c6 image on c61", "16.0.0(release-tasmota32c6)", "ESP32-C61", "", "", "", "", false, "", ErrFamilyMismatch},
		{"solo1 image on dual core", "16.0.0(release-tasmota32solo1)", "ESP32-D0WD", "", "", "", "", false, "", ErrFamilyMismatch},
		{"dual core image on solo1", "16.0.0(release-tasmota32)", "ESP32-S0WD", "", "", "", "", false, "", ErrFamilyMismatch},
		{"esp32 variant on solo1", "16.0.0(release-tasmota32-bluetooth)", "ESP32-SOLO", "", "", "", "", false, "", ErrFamilyMismatch},
		{"esp32 variant on c3", "16.0.0(release-tasmota32-bluetooth)", "ESP32-C3", "", "", "", "", false, "", ErrFamilyMismatch},
		{"safeboot on esp8266", "16.0.0(release-safeboot)", "ESP8266EX", "", "", "", "", false, "", ErrFamilyMismatch},
		// Malformed versions are refused instead of guessed.
		{"no tag", "16.0.0", "ESP8266EX", "", "", "", "", false, "", errBadVersion},
		{"empty version", "", "ESP8266EX", "", "", "", "", false, "", errBadVersion},
		{"letters in semver", "16.0.x(release-tasmota)", "ESP8266EX", "", "", "", "", false, "", errBadVersion},
		{"empty field", "16..0(release-tasmota)", "ESP8266EX", "", "", "", "", false, "", errBadVersion},
		{"leading dot", ".16.0(release-tasmota)", "ESP8266EX", "", "", "", "", false, "", errBadVersion},
		{"trailing garbage", "16.0.0(release-tasmota)x", "ESP8266EX", "", "", "", "", false, "", errBadVersion},
		{"empty tag", "16.0.0()", "ESP8266EX", "", "", "", "", false, "", errBadVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := Detect(Inputs{Status2: status2(tt.version, tt.hw), Root: &RootInfo{Lang: tt.lang}})
			wantErr(t, err, tt.wantErr)
			if tt.wantErr != nil {
				if id != (model.Identity{}) {
					t.Errorf("identity must be empty on error, got %+v", id)
				}
				return
			}
			if id.Family != tt.wantFamily || id.SingleCore != tt.wantSingle || id.Hardware != tt.hw {
				t.Errorf("family/single/hw = %q/%v/%q, want %q/%v/%q", id.Family, id.SingleCore, id.Hardware, tt.wantFamily, tt.wantSingle, tt.hw)
			}
			if id.Variant != tt.wantVariant || id.Language != tt.wantLang || id.Version != tt.wantSemver {
				t.Errorf("variant/lang/version = %q/%q/%q, want %q/%q/%q", id.Variant, id.Language, id.Version, tt.wantVariant, tt.wantLang, tt.wantSemver)
			}
			wantImage := tt.version[len(tt.wantSemver)+1 : len(tt.version)-1]
			if id.Image != wantImage {
				t.Errorf("image = %q, want raw tag %q", id.Image, wantImage)
			}
		})
	}
}

// TestDetectMinimal covers the minimal firmware path: Status 2 is unknown
// there, so version comes from the Upgrade probe and the image from the main
// page. Minimal only exists for ESP8266, so the image proves the family.
// Safeboot does not tell the chip, so its family stays unknown (the engine
// relies on the identity from before the update).
func TestDetectMinimal(t *testing.T) {
	unknown := readFixture(t, "devices/bedroom-cabinet/unknown-foobar.json")
	tests := []struct {
		name       string
		in         Inputs
		wantFamily model.ChipFamily
		wantVar    string
		wantVer    string
		wantLang   string
		wantErr    error
	}{
		{"minimal via probe and root", Inputs{Status2: unknown,
			Upgrade: &UpgradeProbe{Version: "16.0.0"},
			Root:    &RootInfo{Version: "16.0.0", Image: "release-minimal", Lang: "en", Minimal: true}},
			model.ChipESP8266, "minimal", "16.0.0", "", nil},
		{"minimal status 2 empty", Inputs{
			Upgrade: &UpgradeProbe{Version: "16.0.0"},
			Root:    &RootInfo{Image: "release-minimal", Lang: "de", Minimal: true}},
			model.ChipESP8266, "minimal", "16.0.0", "DE", nil},
		{"minimal version from root only", Inputs{Status2: unknown,
			Root: &RootInfo{Version: "15.1.0", Image: "release-minimal", Minimal: true}},
			model.ChipESP8266, "minimal", "15.1.0", "", nil},
		{"safeboot", Inputs{Status2: unknown,
			Upgrade: &UpgradeProbe{Version: "16.0.0"},
			Root:    &RootInfo{Version: "16.0.0", Image: "release-safeboot", Minimal: true}},
			model.ChipUnknown, "safeboot", "16.0.0", "", nil},
		{"no root page", Inputs{Status2: unknown, Upgrade: &UpgradeProbe{Version: "16.0.0"}},
			"", "", "", "", ErrUnknownHardware},
		{"status 2 unknown but image is full firmware", Inputs{Status2: unknown,
			Upgrade: &UpgradeProbe{Version: "16.0.0"},
			Root:    &RootInfo{Version: "16.0.0", Image: "release-tasmota"}},
			"", "", "", "", ErrUnknownHardware},
		{"probe and root disagree on version", Inputs{Status2: unknown,
			Upgrade: &UpgradeProbe{Version: "16.0.0"},
			Root:    &RootInfo{Version: "15.0.0", Image: "release-minimal"}},
			"", "", "", "", errBadVersion},
		{"no version anywhere", Inputs{Status2: unknown,
			Root: &RootInfo{Image: "release-minimal"}},
			"", "", "", "", errBadVersion},
		{"bad version", Inputs{Status2: unknown,
			Upgrade: &UpgradeProbe{Version: "1 or >16"},
			Root:    &RootInfo{Image: "release-minimal"}},
			"", "", "", "", errBadVersion},
		{"minimal heading without image tag", Inputs{Status2: unknown,
			Upgrade: &UpgradeProbe{Version: "16.0.0"},
			Root:    &RootInfo{Version: "16.0.0", Minimal: true}},
			"", "", "", "", ErrUnknownHardware},
		{"minimal heading but full image tag", Inputs{Status2: unknown,
			Upgrade: &UpgradeProbe{Version: "16.0.0"},
			Root:    &RootInfo{Version: "16.0.0", Image: "release-tasmota", Minimal: true}},
			"", "", "", "", ErrUnknownHardware},
		{"image without sha prefix", Inputs{Status2: unknown,
			Upgrade: &UpgradeProbe{Version: "16.0.0"},
			Root:    &RootInfo{Image: "minimal"}},
			"", "", "", "", ErrUnknownHardware},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := Detect(tt.in)
			wantErr(t, err, tt.wantErr)
			if tt.wantErr != nil {
				return
			}
			if id.Family != tt.wantFamily || id.Variant != tt.wantVar || id.Version != tt.wantVer || id.Language != tt.wantLang {
				t.Errorf("got family/variant/version/lang %q/%q/%q/%q, want %q/%q/%q/%q",
					id.Family, id.Variant, id.Version, id.Language, tt.wantFamily, tt.wantVar, tt.wantVer, tt.wantLang)
			}
			if id.Hardware != "" || id.FlashSizeKB != 0 || id.FreeKB != 0 {
				t.Errorf("minimal firmware reports no hardware/flash, got %+v", id)
			}
			if id.Image != tt.in.Root.Image {
				t.Errorf("image = %q, want %q", id.Image, tt.in.Root.Image)
			}
		})
	}
}

// TestDetectBadInput: device output is untrusted. Garbage must give an
// error, never a half-filled identity that looks updatable.
func TestDetectBadInput(t *testing.T) {
	good2 := status2("16.0.0(release-tasmota)", "ESP8266EX")
	tests := []struct {
		name string
		in   Inputs
	}{
		{"status 2 not json", Inputs{Status2: []byte("<html>")}},
		{"status 2 other object", Inputs{Status2: []byte(`{"Status":{"Module":1}}`)}},
		{"status 2 empty object", Inputs{Status2: []byte(`{}`)}},
		{"status 2 wrong types", Inputs{Status2: []byte(`{"StatusFWR":{"Version":16}}`)}},
		{"status 2 command error", Inputs{Status2: []byte(`{"Command":"Error"}`)}},
		{"status 4 not json", Inputs{Status2: good2, Status4: []byte("{")}},
		{"status 4 wrong types", Inputs{Status2: good2, Status4: []byte(`{"StatusMEM":{"Free":"x"}}`)}},
		{"status 4 negative", Inputs{Status2: good2, Status4: []byte(`{"StatusMEM":{"Free":-1,"FlashSize":1024}}`)}},
		{"status 4 negative flash", Inputs{Status2: good2, Status4: []byte(`{"StatusMEM":{"Free":1,"FlashSize":-1}}`)}},
		{"status 4 negative program flash", Inputs{Status2: good2, Status4: []byte(`{"StatusMEM":{"Free":1,"FlashSize":1024,"ProgramFlashSize":-1}}`)}},
		{"status 4 other object", Inputs{Status2: good2, Status4: []byte(`{"Command":"Unknown"}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := Detect(tt.in)
			if err == nil {
				t.Fatalf("want error, got %+v", id)
			}
			if id.Known() {
				t.Errorf("identity on error must not be Known(): %+v", id)
			}
		})
	}
}

// TestDetectWithoutRootOrStatus4: on normal firmware the main page only adds
// the language and Status 4 only adds flash numbers; both may be missing
// (stored values are kept by the caller).
func TestDetectWithoutRootOrStatus4(t *testing.T) {
	id, err := Detect(Inputs{Status2: status2("16.0.0(release-tasmota)", "ESP8266EX")})
	wantErr(t, err, nil)
	if !id.Known() || id.Language != "" || id.FreeKB != 0 || id.FlashSizeKB != 0 {
		t.Errorf("got %+v", id)
	}
}

func FuzzDetect(f *testing.F) {
	for _, dev := range []string{"bedroom-cabinet", "office-monitor-guest", "kitchen-spotlight"} {
		f.Add(readFixture(f, "devices/"+dev+"/status-2.json"), readFixture(f, "devices/"+dev+"/status-4.json"), "en", "", "")
	}
	f.Add(readFixture(f, "devices/bedroom-cabinet/unknown-foobar.json"), []byte{}, "de", "release-minimal", "16.0.0")
	f.Add([]byte(`{"StatusFWR":{"Version":"16.0.0(release-tasmota32c3)","Hardware":"ESP32-C3"}}`), []byte(`{"StatusMEM":{"Free":10}}`), "", "", "")
	f.Fuzz(func(t *testing.T, s2, s4 []byte, lang, image, version string) {
		in := Inputs{Status2: s2, Status4: s4, Root: &RootInfo{Lang: lang, Image: image, Version: version}}
		if version != "" {
			in.Upgrade = &UpgradeProbe{Version: version}
		}
		id, err := Detect(in)
		if err != nil {
			if id != (model.Identity{}) {
				t.Fatalf("identity must be empty on error: %+v", id)
			}
			return
		}
		// A known family must always be backed by the hardware string or,
		// without hardware, by a minimal image (which exists only for ESP8266).
		if id.Family != model.ChipUnknown {
			if id.Hardware != "" && FamilyFromHardware(id.Hardware) != id.Family {
				t.Fatalf("family %q not backed by hardware %q", id.Family, id.Hardware)
			}
			if id.Hardware == "" && (id.Family != model.ChipESP8266 || id.Variant != "minimal") {
				t.Fatalf("family %q without hardware: %+v", id.Family, id)
			}
		}
		if id.Variant == "" || id.Version == "" {
			t.Fatalf("successful detect without variant or version: %+v", id)
		}
	})
}

// TestDetectStatus4Zero: zero is a legal reading (unknown Free), not
// garbage; Plan treats it as unknown and decides conservatively.
func TestDetectStatus4Zero(t *testing.T) {
	id, err := Detect(Inputs{
		Status2: status2("16.0.0(release-tasmota)", "ESP8266EX"),
		Status4: []byte(`{"StatusMEM":{"Free":0,"FlashSize":0,"ProgramFlashSize":0}}`),
	})
	wantErr(t, err, nil)
	if id.FreeKB != 0 || id.FlashSizeKB != 0 || !id.Known() {
		t.Errorf("got %+v", id)
	}
}
