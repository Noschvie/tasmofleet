package fwselect

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func selectFile(id model.Identity, c Choice) (Selection, error) {
	if !id.Known() {
		return Selection{}, fmt.Errorf("select: %w", ErrUnknownHardware)
	}
	variant := id.Variant
	if c.Variant != "" {
		variant = c.Variant
	}
	lang := id.Language
	if c.Language != "" {
		lang = c.Language
	}
	if lang == LanguageEnglish {
		lang = ""
	}
	if c.Variant == "" && !esp8266Variants[id.Variant] && !esp32Variants[id.Variant] {
		// minimal, safeboot, unknown: there is no build to keep.
		return Selection{}, fmt.Errorf("select: device runs %q, choose a variant: %w", id.Variant, ErrVariantChange)
	}
	if (variant != id.Variant || lang != id.Language) && !c.AllowVariantChange {
		return Selection{}, fmt.Errorf("select: %s/%s -> %s/%s: %w", id.Variant, id.Language, variant, lang, ErrVariantChange)
	}
	if lang != "" && (variant != variantTasmota || !isLanguage(lang)) {
		return Selection{}, fmt.Errorf("select: variant %q with language %q: %w", variant, lang, ErrNoSuchVariant)
	}

	sel := Selection{Family: id.Family, Variant: variant, Language: lang}
	suffix := variant
	if variant == variantTasmota {
		suffix = lang
	}
	if suffix != "" {
		suffix = "-" + suffix
	}
	switch {
	case id.Family == model.ChipESP8266 && esp8266Variants[variant]:
		sel.Filename = "tasmota" + suffix + ".bin.gz"
		sel.Minimal = minimalFile
	case id.Family == model.ChipESP32 && id.SingleCore && suffix == "":
		sel.Filename = "tasmota32solo1.bin"
	case id.Family == model.ChipESP32 && !id.SingleCore && esp32Variants[variant]:
		sel.Filename = "tasmota32" + suffix + ".bin"
	case id.Family != model.ChipESP8266 && id.Family != model.ChipESP32 && suffix == "":
		build, err := chipBuild(id)
		if err != nil {
			return Selection{}, err
		}
		sel.Filename = "tasmota32" + build + ".bin"
	default:
		return Selection{}, fmt.Errorf("select: variant %q for %s: %w", variant, id.Family, ErrNoSuchVariant)
	}
	return sel, nil
}

// chipBuild returns the tasmota32<chip> build for the device's chip. Every
// chip build reports the image tag "tasmota32" (§6.2), so the build comes
// from the hardware alone. ESP32-S2 has two builds (s2, s2cdc) that nothing
// the device reports can tell apart: refused, never defaulted (#85). ESP32-P4
// has two builds for disjoint chip revisions, decided by p4Build.
func chipBuild(id model.Identity) (string, error) {
	if id.Family == model.ChipESP32S2 {
		return "", fmt.Errorf("select: ESP32-S2 has two builds (s2, s2cdc) that report the same image name; choosing the build is not supported yet: %w", ErrUnknownHardware)
	}
	if id.Family == model.ChipESP32P4 {
		return p4Build(id.Hardware)
	}
	build, ok := defaultBuild[id.Family]
	if !ok {
		return "", fmt.Errorf("select: no build for %s: %w", id.Family, ErrUnknownHardware)
	}
	return build, nil
}

// p4Build maps the chip revision in Status 2 Hardware ("ESP32-P4 v3.1":
// Tasmota prints " v<rev/100>.<rev%100>", support_esp32.ino) to the build
// that boots on it. Board esp32p4 (chip_variant esp32p4_es) is built for
// revisions 0.1..1.99 (CONFIG_ESP32P4_REV_MIN_1, REV_MAX_FULL 199), board
// esp32p4r3 with the ESP-IDF default 3.1..3.99 (REV_MIN_301, REV_MAX_FULL
// 399; Kconfig.hw_support). Every other revision, or none, is refused.
func p4Build(hw string) (string, error) {
	rev, ok := chipRevision(hw, "ESP32-P4 v")
	switch {
	case ok && rev >= 1 && rev <= 199:
		return "p4", nil
	case ok && rev >= 301 && rev <= 399:
		return "p4r3", nil
	}
	return "", fmt.Errorf("select: ESP32-P4 hardware %q is in no revision range of the p4 (0.1-1.99) or p4r3 (3.1-3.99) build: %w", hw, ErrUnknownHardware)
}

// chipRevision parses "<prefix><major>.<minor>" (major one digit, minor
// 0..99) into major*100+minor, the chip_revision Tasmota printed. The
// length bounds also keep Atoi from overflowing.
func chipRevision(hw, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(hw, prefix)
	if !ok {
		return 0, false
	}
	major, minor, ok := strings.Cut(rest, ".")
	if !ok || !isNumericVersion(major) || !isNumericVersion(minor) || len(major) != 1 || len(minor) > 2 {
		return 0, false
	}
	ma, _ := strconv.Atoi(major) // one digit: cannot fail
	mi, _ := strconv.Atoi(minor) // digits only, at most two: cannot fail
	return ma*100 + mi, true
}

// fileInfo is what an upstream file name says about its build.
type fileInfo struct {
	family  model.ChipFamily
	variant string
	lang    string
	solo1   bool
	gz      bool
}

// parseFilename reads the §7.1 naming scheme. ok is false for names that
// are not an OTA build of a known family/variant.
func parseFilename(name string) (fileInfo, bool) {
	var fi fileInfo
	base, gz := strings.CutSuffix(name, ".bin.gz")
	if !gz {
		var bin bool
		if base, bin = strings.CutSuffix(name, ".bin"); !bin {
			return fileInfo{}, false
		}
	}
	fi.gz = gz
	prefix, suffix, hasSuffix := strings.Cut(base, "-")
	build := ""
	switch {
	case prefix == "tasmota":
		fi.family = model.ChipESP8266
	case strings.HasPrefix(prefix, "tasmota32"):
		build = strings.TrimPrefix(prefix, "tasmota32")
		fam, ok := chipBuilds[build]
		if !ok {
			return fileInfo{}, false
		}
		fi.family = fam
		fi.solo1 = build == "solo1"
	default:
		return fileInfo{}, false
	}
	fi.variant = variantTasmota
	switch {
	case !hasSuffix:
	case suffix == variantSafeboot:
		fi.variant = variantSafeboot
	case isLanguage(suffix) && build == "":
		fi.lang = suffix
	case fi.family == model.ChipESP8266 && suffix != variantTasmota && (esp8266Variants[suffix] || suffix == variantMinimal):
		fi.variant = suffix
	case build == "" && fi.family == model.ChipESP32 && suffix != variantTasmota && esp32Variants[suffix]:
		fi.variant = suffix
	default:
		return fileInfo{}, false
	}
	return fi, true
}
