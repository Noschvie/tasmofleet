package fwselect

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hellwand/tasmofleet/internal/model"
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
		// Features is read apart (localeID): a shape we do not know must not cost the device its
		// identity.
		Features json.RawMessage
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
		FWR:        fwrFacts(in.Status2),
	}
	// An unparseable Version does not cost the device its identity (#45): the version stays
	// empty and the variant unknown, so the identity is never Known() and Guard refuses
	// every OTA, exactly as for unknown hardware. Nothing is guessed from the string.
	id, err := applyVersion(id, s2.StatusFWR.Version)
	if err != nil {
		return model.Identity{}, err
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
		// The language build is the locale ID alone (#686): the main page is not read for it, so a
		// page that says otherwise cannot change it, and without Status 4 the language stays unread.
		id.Language, id.LanguageKnown = languageFromLCID(localeID(m.Features))
	}
	return id, nil
}

// localeID is the first entry of StatusMEM.Features: the locale ID of the language build,
// written as "%04X" (support_features.ino, ResponseAppendFeatures). "" for anything else.
func localeID(features json.RawMessage) string {
	var entries []json.RawMessage
	if json.Unmarshal(features, &entries) != nil || len(entries) == 0 {
		return ""
	}
	var first string
	if json.Unmarshal(entries[0], &first) != nil {
		return ""
	}
	return first
}

// applyVersion sets Version, Image and Variant of id from a firmware version string, checking the
// image against the hardware id already carries (unknown hardware: nothing to check against). A
// string outside the grammar leaves them empty/unknown without an error (#45); an image that
// cannot run on the hardware is ErrFamilyMismatch.
func applyVersion(id model.Identity, version string) (model.Identity, error) {
	fv, ok := parseVersion(version)
	if !ok {
		return id, nil
	}
	variant, ok := variantFromTag(fv.tag, id.Family, id.SingleCore)
	// CORE32SOLO1 builds exist only for the classic ESP32.
	if fv.cores == coresSingle && id.Family != model.ChipUnknown && id.Family != model.ChipESP32 {
		ok = false
	}
	if !ok {
		return model.Identity{}, fmt.Errorf("fwselect: image %q (%q) does not run on hardware %q: %w", fv.tag, fv.cores, id.Hardware, ErrFamilyMismatch)
	}
	id.Version, id.Image, id.Variant = fv.semver, fv.tag, variant
	return id, nil
}

// DetectInfo1 reads the firmware version from a tele/<topic>/INFO1 message, which every device
// publishes at boot ({"Info1":{"Module":..,"Version":"16.0.0(release-tasmota)",..}}), and applies
// it to base, the identity already known (zero: none). Only Version, Image and Variant change.
// INFO1 carries no hardware, so without a family in base the result is never Known() and Guard
// refuses every update on it; the variant is checked against base's hardware when there is one.
// A version outside the grammar is an error (nothing to take over), unlike Detect, which keeps
// the hardware of such a device.
func DetectInfo1(payload []byte, base model.Identity) (model.Identity, error) {
	var m struct {
		Info1 *struct{ Version string }
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return model.Identity{}, fmt.Errorf("fwselect: parse INFO1: %w", err)
	}
	if m.Info1 == nil {
		return model.Identity{}, errors.New("fwselect: INFO1 message has no Info1 object")
	}
	if _, ok := parseVersion(m.Info1.Version); !ok {
		return model.Identity{}, fmt.Errorf("%w: %q", errBadVersion, m.Info1.Version)
	}
	base.Version, base.Image, base.Variant = "", "", variantUnknown
	return applyVersion(base, m.Info1.Version)
}

// detectMinimal handles firmware that answers Status 2 with Unknown:
// minimal (ESP8266 only, so the image proves the family) and safeboot (the
// chip is not reported, so the family stays unknown). The image comes from
// Root (an INFO1 since the device's last boot, or the main page footer); the
// version from the bare Upgrade reply and/or Root; they must agree.
// RootInfo.Minimal is not enough on its own: the heading is set for minimal
// and for safeboot alike, so the image tag decides. Without Root only an
// update job can name the image (detectInJob). Inside a job the image must be
// the intermediate image of the job's chip family, whatever told it.
func detectMinimal(in Inputs) (model.Identity, error) {
	if in.Root == nil {
		return detectInJob(in)
	}
	image, ok := imageOfTag(in.Root.Image)
	var fam model.ChipFamily
	switch {
	case ok && image == variantMinimal:
		fam = model.ChipESP8266
	case ok && image == variantSafeboot:
		fam = model.ChipUnknown
	default:
		return model.Identity{}, fmt.Errorf("fwselect: Status 2 unavailable on image %q: %w", in.Root.Image, ErrUnknownHardware)
	}
	if !intermediateOf(image, in.JobFamily) {
		return model.Identity{}, fmt.Errorf("fwselect: image %q is not the intermediate image of the job's chip family %q: %w", in.Root.Image, in.JobFamily, ErrFamilyMismatch)
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
	// Minimal firmware answers no Status 4, so a page code two builds share stays unknown.
	lang, known := languageFromRoot(in.Root)
	return model.Identity{
		Family:        fam,
		Version:       version,
		Image:         in.Root.Image,
		Variant:       image,
		Language:      lang,
		LanguageKnown: known,
	}, nil
}

// intermediateOf reports whether image ("minimal" or "safeboot") is the intermediate image of an
// update job on chip family job: minimal exists only for the ESP8266, safeboot only for the ESP32
// line. Outside a job (ChipUnknown) there is nothing to check against.
func intermediateOf(image string, job model.ChipFamily) bool {
	switch {
	case job == model.ChipUnknown:
		return true
	case image == variantMinimal:
		return job == model.ChipESP8266
	default:
		return job.IsESP32()
	}
}

// detectInJob names minimal or safeboot firmware without its image (#686, path 2): inside
// TasmoFleet's own update job, whose preflight read the chip family, Status 2 Unknown is the
// intermediate image of that family (minimal on an ESP8266, safeboot on the ESP32 line). The
// version must come from the bare Upgrade reply; nothing else is filled in, and the image stays
// empty rather than guessed. Minimal has no Status 5, so the MAC cannot be checked here, and nothing
// here proves that the device at the job's address is still the one it started on: Guard checks the
// job's preflight identity, not this one, and hardwareChanged ignores the unknown family of safeboot.
// The OTA device API therefore never comes here inside a job since #918: without an INFO1 it reads
// the main page, whose image detectMinimal checks against the job's family. Outside a job (JobFamily
// unknown) this is ErrUnknownHardware, as before.
func detectInJob(in Inputs) (model.Identity, error) {
	var id model.Identity
	switch {
	case in.JobFamily == model.ChipESP8266:
		id.Family, id.Variant = model.ChipESP8266, variantMinimal
	case in.JobFamily.IsESP32():
		id.Family, id.Variant = model.ChipUnknown, variantSafeboot // safeboot does not report the chip
	default:
		return model.Identity{}, fmt.Errorf("fwselect: Status 2 unavailable, no INFO1 or main page and no update job: %w", ErrUnknownHardware)
	}
	if in.Upgrade == nil || !isNumericVersion(in.Upgrade.Version) {
		return model.Identity{}, fmt.Errorf("fwselect: Status 2 unavailable and the Upgrade reply names no version: %w", ErrUnknownHardware)
	}
	id.Version = in.Upgrade.Version
	return id, nil
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

// imageOfTag splits a raw image tag into the image name and says whether the name may be trusted.
// An official tag is "<sha-or-release>-<image>": the name is what follows the first dash. A
// self-compiled build reports the bare environment name, which names nothing we can rely on (spec
// 6.2, #52): it is not trusted, so a custom full build is never replaced without a variant choice
// and no chip is inferred from it (bare "minimal" would prove the ESP8266). The one exception is
// the bare tag "safeboot" (#584): it names no family and no variant to keep, so it is the same
// identity as "release-safeboot". Do not add a second bare tag here without its own issue.
func imageOfTag(tag string) (image string, trusted bool) {
	if _, image, ok := strings.Cut(tag, "-"); ok {
		return image, true
	}
	if tag == variantSafeboot {
		return tag, true
	}
	return "", false
}

// variantFromTag applies the §6.2 table to a raw image tag
// ("<sha-or-release>-<image>"). ok is false when the image cannot run on
// the detected hardware. With unknown hardware there is nothing to check
// against; the identity is unusable for updates anyway.
func variantFromTag(tag string, fam model.ChipFamily, single bool) (string, bool) {
	image, found := imageOfTag(tag)
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
			// No official single-core build reports "tasmota32" (solo1 reports
			// "solo1" since the tag exists, v12.5); S2 and P4 have two builds
			// each. Which one runs is not provable: never guess.
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

// languageFromRoot maps <html lang> to the upstream language suffix ("" is the plain English
// build) and says whether that is a fact. Only minimal and safeboot firmware are read this way:
// they answer no Status 4, so their main page (path 3 of #686: no INFO1, no job that knows the chip) is the only place
// that names the build. The code is looked up in the language table (language.go), never derived
// from the spelling: Tasmota writes the ISO code, the release files use their own suffixes (#509).
// A code the table does not name ("pt", which two builds write, an unlisted one) leaves the
// language unread: an empty Language is then no proof of English. The language is also only known
// when it came from a device main page (an INFO1 carries none).
func languageFromRoot(r *RootInfo) (suffix string, known bool) {
	if r == nil {
		return "", false
	}
	suffix, ok := languageFromHTML(r.Lang)
	return suffix, r.LangKnown && ok
}

// fwrFacts reads the display-only StatusFWR fields. They never decide identity or updatability,
// so a field of an unexpected type is dropped instead of failing the detection: every field is
// decoded on its own and only a value of the expected type is kept.
func fwrFacts(status2 []byte) model.FWRFacts {
	var outer struct{ StatusFWR map[string]json.RawMessage }
	if json.Unmarshal(status2, &outer) != nil {
		return model.FWRFacts{}
	}
	str := func(key string) string {
		var v string
		if json.Unmarshal(outer.StatusFWR[key], &v) != nil {
			return ""
		}
		return v
	}
	var mhz int
	if json.Unmarshal(outer.StatusFWR["CpuFrequency"], &mhz) != nil {
		mhz = 0
	}
	return model.FWRFacts{Version: str("Version"), BuildDateTime: str("BuildDateTime"), Core: str("Core"), SDK: str("SDK"), CPUFrequency: mhz, CR: str("CR")}
}
