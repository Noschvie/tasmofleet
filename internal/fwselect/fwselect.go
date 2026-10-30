// Package fwselect decides which firmware file a device may receive. It is
// pure (no I/O) and is the most heavily tested package in the repository:
// table tests over every captured device fixture, fuzzing of the parsers,
// and mutation testing (gremlins) on Guard and Plan.
//
// Four responsibilities, one per function:
//
//   - Detect: Status 2 + Status 4 answers -> model.Identity
//     (chip family from Hardware, variant/language from the Version image tag).
//   - Select: (identity, release, chosen variant) -> the upstream file name,
//     e.g. tasmota-sensors.bin.gz (ESP8266) or tasmota32c3.bin (ESP32-C3).
//   - InspectImage: first bytes of a firmware file -> the chip family the
//     binary was built for (ESP image header: magic 0xE9, chip_id at offset 12
//     for ESP32 app images).
//   - Guard: the hard safety check run immediately before every OtaUrl/Upgrade.
//     Refuses unless device family == artifact family == file-name family, the
//     artifact is not a safeboot image, the identity is known, and the variant
//     equals the device variant unless the user explicitly chose another.
//   - Plan: direct vs staged (minimal first). Staged only for ESP8266 when the
//     target image does not fit into the free OTA space; never decided by
//     model name.
package fwselect

import (
	"errors"
	"fmt"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Guard and selection errors. Every refusal wraps one of these so the UI and
// tests can tell them apart.
var (
	ErrUnknownHardware  = errors.New("fwselect: unknown hardware, refusing to guess")
	ErrFamilyMismatch   = errors.New("fwselect: chip family mismatch")
	ErrFilenameMismatch = errors.New("fwselect: OTA file name does not match the artifact")
	ErrSafeboot         = errors.New("fwselect: safeboot image is not an OTA target")
	ErrVariantChange    = errors.New("fwselect: variant change without explicit choice")
	ErrNoSuchVariant    = errors.New("fwselect: variant not built for this chip family")
	ErrBadImage         = errors.New("fwselect: not an ESP firmware image")
	ErrNotImplemented   = errors.New("fwselect: not implemented")

	// ErrLanguageUnknown: the language build of the device was never read from its web page
	// (MQTT-only devices), so Select will not pick a file without an explicit Choice.Language.
	// "EN" is an allowed explicit choice. Never guess: a German device must not get the English build.
	ErrLanguageUnknown = errors.New("fwselect: language build never read from the device, choose a language")

	// ErrBuildChoiceNeeded is the refusal for a chip with two upstream builds
	// that nothing the device reports can tell apart (ESP32-S2: s2, s2cdc, #88).
	// It is an ErrUnknownHardware, so every caller that refuses on that keeps
	// refusing; the UI additionally offers BuildOptions.
	ErrBuildChoiceNeeded = fmt.Errorf("fwselect: this chip has two builds that report the same image name, choose one: %w", ErrUnknownHardware)
)

// LanguageEnglish in Choice.Language explicitly selects the English build.
// An empty Choice.Language keeps the detected language, so without this
// value a German device could not be switched to an English build.
const LanguageEnglish = "EN"

// Inputs are the raw device answers Detect works from. Status2/Status4 are
// the JSON replies of "Status 2" and "Status 4" (Status2 may be
// {"Command":"Unknown"} on minimal firmware, Status4 then empty). Upgrade is
// the parsed bare "Upgrade" reply and Root the parsed main page; both are
// required for minimal firmware and Root is always used for the language.
type Inputs struct {
	Status2 []byte
	Status4 []byte
	Upgrade *UpgradeProbe
	Root    *RootInfo
}

// UpgradeProbe carries the version from a bare "Upgrade" reply
// ("1 or >X to upgrade"). Normal firmware answers the same way, so the probe
// never proves minimal firmware on its own: Detect treats a device as minimal
// only when Status 2 is {"Command":"Unknown"} and Root says minimal/safeboot
// (heading or image tag "release-minimal"/"safeboot").
type UpgradeProbe struct {
	Version string
}

// RootInfo mirrors the fields of tasmota.RootInfo that Detect needs.
type RootInfo struct {
	Version string
	Image   string // e.g. "release-minimal"
	Lang    string // <html lang>, e.g. "en", "de"
	// LangKnown: Lang was read from a device main page (HTTP). Without it the language build is
	// unknown, because only the web page shows it (D10); an empty Lang is then no proof of English.
	LangKnown bool
	Minimal   bool
}

// Detect builds an identity from the device answers. It never guesses: an
// unknown Hardware string gives ChipUnknown, an image tag whose family
// disagrees with Hardware gives ErrFamilyMismatch.
func Detect(in Inputs) (model.Identity, error) {
	id, err := detect(in)
	if err != nil {
		return model.Identity{}, err
	}
	return id, nil
}

// FamilyFromHardware maps StatusFWR.Hardware to a chip family. Unknown
// strings return ChipUnknown (never a guess).
func FamilyFromHardware(hw string) model.ChipFamily {
	fam, _ := hardwareInfo(hw)
	return fam
}

// Choice is the user's selection for an update.
type Choice struct {
	Release  string // e.g. "16.0.0"
	Variant  string // empty = keep the detected variant
	Language string // empty = keep the detected language; required when Identity.LanguageKnown is false
	// AllowVariantChange must be true when Variant differs from the detected
	// one. Set only from an explicit UI/CLI confirmation.
	AllowVariantChange bool
	// Build is the explicit chip build (ESP32-S2: "s2" or "s2cdc"). Empty for
	// every device but an ESP32-S2, where Select refuses without it (#88). Set
	// only from an explicit UI/CLI confirmation; it must be one of
	// BuildOptions(id). For an S2 it implies the plain tasmota variant, and the
	// variant change from the detected "unknown" that this means.
	Build  string
	Policy model.UpdatePolicy
}

// Decision is what Decide returns for a device.
type Decision string

// Decisions.
const (
	DecideUpgrade   Decision = "upgrade"
	DecideReinstall Decision = "reinstall"
	DecideDowngrade Decision = "downgrade"
	DecideSkip      Decision = "skip"
)

// Decide applies the update policy to the current and target versions.
// A device running minimal firmware is always "upgrade" (it must leave
// minimal). Versions compare numerically per dotted field.
func Decide(id model.Identity, c Choice) (Decision, error) {
	return decide(id, c)
}

// Selection is the result of Select.
type Selection struct {
	Family   model.ChipFamily
	Variant  string
	Language string
	Filename string // upstream file name to fetch and serve
	Minimal  string // ESP8266 only: "tasmota-minimal.bin.gz"; filled for every ESP8266 selection
}

// Select maps a device identity and a choice to the upstream file name.
func Select(id model.Identity, c Choice) (Selection, error) {
	sel, err := selectFile(id, c)
	if err != nil {
		return Selection{}, err
	}
	return sel, nil
}

// ImageInfo is what InspectImage reads from a firmware header.
type ImageInfo struct {
	Family model.ChipFamily // ChipESP32 also for solo1 builds (same chip_id 0)
	ChipID int              // ESP32 extended header chip_id, -1 for ESP8266
}

// InspectImage parses the first 24 bytes of an uncompressed firmware image.
// Callers gunzip .bin.gz first.
func InspectImage(header []byte) (ImageInfo, error) {
	return inspectImage(header)
}

// Guard is the last check before OtaUrl/Upgrade. otaURL is the exact URL
// that will be sent to the device. It returns nil only if every rule holds.
//
// id is the identity whose variant and language are kept. For both stages
// of a staged job the engine passes the preflight identity (IdentityBefore),
// because during the final stage the device runs minimal and has no variant
// of its own. Rule 7 is enforced as "the artifact is exactly Select(id, c)",
// or the esp8266 minimal stage file of that selection.
func Guard(id model.Identity, a model.Artifact, otaURL string, c Choice) error {
	return guard(id, a, otaURL, c)
}

// Plan decides direct vs staged. targetSize is the size in bytes of the
// target image as the device will download it (.bin.gz for ESP8266).
func Plan(id model.Identity, targetSize int64) (model.Plan, error) {
	return plan(id, targetSize)
}

// BuildOptions lists the builds the user may choose for the device's chip:
// "s2" and "s2cdc" for an ESP32-S2, nil for every other device. An ESP32-P4
// has two builds too, but its chip revision proves which one boots (Select),
// so there is nothing to choose.
func BuildOptions(id model.Identity) []string {
	if id.Family == model.ChipESP32S2 {
		return []string{"s2", "s2cdc"}
	}
	return nil
}

// DetectedVariant is the variant Detect will read from a device after it has
// run sel. A chip with two builds reports the shared image tag "tasmota32",
// from which Detect cannot tell the build: the variant is "unknown" there
// although the selection is plain "tasmota". The update's verify step must
// compare against this, not against Selection.Variant, or a correct S2 update
// would end as "not taken".
func DetectedVariant(sel Selection) string {
	if ambiguousChips[sel.Family] {
		return variantUnknown
	}
	return sel.Variant
}
