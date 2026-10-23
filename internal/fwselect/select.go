package fwselect

import (
	"fmt"
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
		sel.Filename = "tasmota32" + chipBuild(id) + ".bin"
	default:
		return Selection{}, fmt.Errorf("select: variant %q for %s: %w", variant, id.Family, ErrNoSuchVariant)
	}
	return sel, nil
}

// chipBuild keeps an alternative chip build (s2cdc, p4r3) the device runs,
// otherwise the chip's standard build.
func chipBuild(id model.Identity) string {
	_, image, _ := strings.Cut(id.Image, "-")
	if build, ok := strings.CutPrefix(image, "tasmota32"); ok && build != "" && chipBuilds[build] == id.Family {
		return build
	}
	return defaultBuild[id.Family]
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
