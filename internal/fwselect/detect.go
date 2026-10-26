package fwselect

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// errBadVersion: a version string that is not "<n>[.<n>...]" (plus image
// tag where required). Never guessed around.
var errBadVersion = errors.New("fwselect: malformed firmware version")

type status2Reply struct {
	StatusFWR *struct {
		Version  string
		Hardware string
	}
	Command string
}

type status4Reply struct {
	StatusMEM *struct {
		FlashSize        int
		ProgramFlashSize int
		Free             int
	}
}

func detect(in Inputs) (model.Identity, error) {
	var s2 status2Reply
	if len(in.Status2) > 0 {
		if err := json.Unmarshal(in.Status2, &s2); err != nil {
			return model.Identity{}, fmt.Errorf("fwselect: parse Status 2: %w", err)
		}
	}
	if s2.StatusFWR == nil {
		if len(in.Status2) == 0 || s2.Command == "Unknown" {
			return detectMinimal(in)
		}
		return model.Identity{}, fmt.Errorf("fwselect: Status 2 reply has no StatusFWR: %w", ErrUnknownHardware)
	}

	hw := s2.StatusFWR.Hardware
	fam, single := hardwareInfo(hw)
	id := model.Identity{
		Family:     fam,
		Hardware:   hw,
		SingleCore: single,
		Variant:    variantUnknown,
		Language:   languageFromRoot(in.Root),
	}
	// An unparseable Version does not cost the device its identity (#45): the version stays
	// empty and the variant unknown, so the identity is never Known() and Guard refuses
	// every OTA, exactly as for unknown hardware. Nothing is guessed from the string.
	if fv, ok := parseVersion(s2.StatusFWR.Version); ok {
		variant, ok := variantFromTag(fv.tag, fam, single)
		// CORE32SOLO1 builds exist only for the classic ESP32.
		if fv.cores == coresSingle && fam != model.ChipUnknown && fam != model.ChipESP32 {
			ok = false
		}
		if !ok {
			return model.Identity{}, fmt.Errorf("fwselect: image %q (%q) does not run on hardware %q: %w", fv.tag, fv.cores, hw, ErrFamilyMismatch)
		}
		id.Version, id.Image, id.Variant = fv.semver, fv.tag, variant
	}
	if len(in.Status4) > 0 {
		var s4 status4Reply
		if err := json.Unmarshal(in.Status4, &s4); err != nil {
			return model.Identity{}, fmt.Errorf("fwselect: parse Status 4: %w", err)
		}
		m := s4.StatusMEM
		if m == nil || m.FlashSize < 0 || m.ProgramFlashSize < 0 || m.Free < 0 {
			return model.Identity{}, errors.New("fwselect: Status 4 reply has no valid StatusMEM")
		}
		id.FlashSizeKB, id.ProgramFlashSizeKB, id.FreeKB = m.FlashSize, m.ProgramFlashSize, m.Free
	}
	return id, nil
}

// detectMinimal handles firmware that answers Status 2 with Unknown:
// minimal (ESP8266 only, so the image proves the family) and safeboot (the
// chip is not reported, so the family stays unknown). The version comes from
// the bare Upgrade reply and/or the main page footer; they must agree.
// RootInfo.Minimal is not enough on its own: the heading is set for minimal
// and for safeboot alike, so the footer image tag decides.
func detectMinimal(in Inputs) (model.Identity, error) {
	if in.Root == nil {
		return model.Identity{}, fmt.Errorf("fwselect: Status 2 unavailable and no main page: %w", ErrUnknownHardware)
	}
	_, image, ok := strings.Cut(in.Root.Image, "-")
	var fam model.ChipFamily
	switch {
	case ok && image == variantMinimal:
		fam = model.ChipESP8266
	case ok && image == variantSafeboot:
		fam = model.ChipUnknown
	default:
		return model.Identity{}, fmt.Errorf("fwselect: Status 2 unavailable on image %q: %w", in.Root.Image, ErrUnknownHardware)
	}
	version := in.Root.Version
	if in.Upgrade != nil && in.Upgrade.Version != "" {
		if version != "" && version != in.Upgrade.Version {
			return model.Identity{}, fmt.Errorf("%w: Upgrade says %q, main page %q", errBadVersion, in.Upgrade.Version, version)
		}
		version = in.Upgrade.Version
	}
	if !isNumericVersion(version) {
		return model.Identity{}, fmt.Errorf("%w: %q", errBadVersion, version)
	}
	return model.Identity{
		Family:   fam,
		Version:  version,
		Image:    in.Root.Image,
		Variant:  image,
		Language: languageFromRoot(in.Root),
	}, nil
}

// coresSingle is what GetCodeCores() appends for CORE32SOLO1 builds
// (support_esp32.ino); every other build appends nothing.
const coresSingle = "single-core"

// fwVersion is StatusFWR.Version split the way Tasmota builds it
// (support_command.ino, Status 2: version, image_name, GetCodeCores()).
type fwVersion struct {
	semver string // "16.0.0", or "16.0.0.3" for development and patch builds
	tag    string // the image name inside the parentheses, e.g. "release-tasmota", "solo1"
	cores  string // "" or coresSingle
}

// parseVersion splits "16.0.0(release-solo1)single-core" into its parts.
// Anything that does not follow the grammar reports ok=false.
func parseVersion(v string) (fwVersion, bool) {
	semver, rest, ok := strings.Cut(v, "(")
	if !ok || !isTasmotaVersion(semver) {
		return fwVersion{}, false
	}
	tag, cores, ok := strings.Cut(rest, ")")
	if !ok || tag == "" || strings.Contains(tag, "(") || (cores != "" && cores != coresSingle) {
		return fwVersion{}, false
	}
	return fwVersion{semver: semver, tag: tag, cores: cores}, true
}

// isTasmotaVersion: Tasmota prints "%d.%d.%d" and appends ".%d" only for
// development and patch builds (tasmota.ino), so three or four fields.
func isTasmotaVersion(v string) bool {
	n := strings.Count(v, ".")
	return (n == 2 || n == 3) && isNumericVersion(v)
}

func isNumericVersion(v string) bool {
	if v == "" {
		return false
	}
	for _, f := range strings.Split(v, ".") {
		if f == "" {
			return false
		}
		for i := 0; i < len(f); i++ {
			if !isDigit(f[i]) {
				return false
			}
		}
	}
	return true
}

// imageTag is one row of the §6.2 table: what an upstream build reports as
// CODE_IMAGE_STR (Tasmota source; testdata/fixtures/firmware-versions.json
// names the lines) and where that build runs.
type imageTag struct {
	variant string
	esp8266 bool // built for the ESP8266
	esp32   bool // built for the classic dual-core ESP32
}

// imageTags covers every tag except "tasmota32", "solo1" and "safeboot",
// whose chip rules are in variantFromTag. A tag not listed here is unknown.
var imageTags = map[string]imageTag{
	"tasmota":       {variantTasmota, true, false},
	"minimal":       {variantMinimal, true, false},
	"lite":          {"lite", true, false},
	"sensors":       {"sensors", true, false},
	"knx":           {"knx", true, false},
	"zbbridge":      {"zbbridge", true, false},
	"zigbee":        {"zigbee", true, false},
	"tasmota-4M":    {"4M", true, false},
	"display":       {"display", true, true}, // one CODE_IMAGE_STR for both families
	"ir":            {"ir", true, true},
	"webcam":        {"webcam", false, true},
	"bluetooth":     {"bluetooth", false, true},
	"lvgl-haspmota": {"lvgl", false, true},
	"lvgl":          {"lvgl", false, true}, // up to v11
	"nspanel":       {"nspanel", false, true},
	"zbbrdgpro":     {"zbbrdgpro", false, true},
}

// ambiguousChips have two upstream builds that both report "tasmota32"
// (s2/s2cdc, p4/p4r3): the tag cannot tell which one runs.
var ambiguousChips = map[model.ChipFamily]bool{
	model.ChipESP32S2: true,
	model.ChipESP32P4: true,
}

// variantFromTag applies the §6.2 table to a raw image tag
// ("<sha-or-release>-<image>"). ok is false when the image cannot run on
// the detected hardware. With unknown hardware there is nothing to check
// against; the identity is unusable for updates anyway.
func variantFromTag(tag string, fam model.ChipFamily, single bool) (string, bool) {
	_, image, found := strings.Cut(tag, "-")
	if !found {
		// A self-compiled build reports a bare tag ("16.0.0(tasmota32)"). It stays unknown on
		// purpose: we never overwrite a custom build without an explicit variant choice.
		return variantUnknown, true
	}
	known := fam != model.ChipUnknown
	switch image {
	case "tasmota32":
		// Every FIRMWARE_TASMOTA32 build but solo1 reports "tasmota32", one
		// build per chip, so the chip comes from Hardware.
		switch {
		case fam == model.ChipESP8266:
			return "", false
		case single || ambiguousChips[fam]:
			// Up to v12.0 solo1 reported "tasmota32" too; S2 and P4 have two
			// builds each. Which one runs is not provable: never guess.
			return variantUnknown, true
		default:
			return variantTasmota, true
		}
	case "solo1":
		// The upstream env tasmota32solo1 sets CODE_IMAGE_STR "solo1"
		// (platformio_tasmota_env32.ini): the official single-core build.
		return variantTasmota, !known || (fam == model.ChipESP32 && single)
	case variantSafeboot:
		return variantSafeboot, !known || fam.IsESP32()
	}
	t, ok := imageTags[image]
	if !ok {
		return variantUnknown, true
	}
	return t.variant, !known || (t.esp8266 && fam == model.ChipESP8266) || (t.esp32 && fam == model.ChipESP32 && !single)
}

// languageFromRoot maps <html lang> to the upstream language suffix:
// "en" (or nothing) is the plain build, anything else is upper-cased.
func languageFromRoot(r *RootInfo) string {
	if r == nil {
		return ""
	}
	l := strings.ToUpper(strings.TrimSpace(r.Lang))
	if l == "EN" {
		return ""
	}
	return l
}
