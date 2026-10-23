package fwselect

import (
	"fmt"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// maxOtaURLLen is the size of Tasmota's OtaUrl settings text (§7.5 rule 8).
const maxOtaURLLen = 100

// guard implements the eight rules of §7.5. Each refusal wraps exactly one
// sentinel error; the rule numbers below follow the spec.
func guard(id model.Identity, a model.Artifact, otaURL string, c Choice) error {
	// Rule 1: known identity, and an identity consistent with its hardware.
	if !id.Known() {
		return fmt.Errorf("guard: identity %q/%q/%q: %w", id.Family, id.Version, id.Variant, ErrUnknownHardware)
	}
	if id.Hardware != "" {
		fam, single := hardwareInfo(id.Hardware)
		if fam == model.ChipUnknown {
			return fmt.Errorf("guard: hardware %q: %w", id.Hardware, ErrUnknownHardware)
		}
		if fam != id.Family || single != id.SingleCore {
			return fmt.Errorf("guard: hardware %q is not %s (single core %v): %w", id.Hardware, id.Family, id.SingleCore, ErrFamilyMismatch)
		}
	}
	// Rule 4: never safeboot or factory images.
	if a.Safeboot || strings.Contains(a.Filename, variantSafeboot) || strings.Contains(a.Filename, ".factory.") {
		return fmt.Errorf("guard: %q: %w", a.Filename, ErrSafeboot)
	}
	// Rule 2: header-proven artifact family equals device family.
	if a.Family != id.Family {
		return fmt.Errorf("guard: artifact %s, device %s: %w", a.Family, id.Family, ErrFamilyMismatch)
	}
	// Rule 3: file name agrees with the artifact, URL is its firmware server URL.
	fi, ok := parseFilename(a.Filename)
	if !ok {
		return fmt.Errorf("guard: %q is not an OTA build name: %w", a.Filename, ErrFilenameMismatch)
	}
	if fi.family != a.Family || fi.variant != a.Variant || fi.lang != a.Language {
		return fmt.Errorf("guard: name %q says %s/%s/%s, artifact %s/%s/%s: %w",
			a.Filename, fi.family, fi.variant, fi.lang, a.Family, a.Variant, a.Language, ErrFilenameMismatch)
	}
	if c.Release != "" && a.Release != c.Release {
		return fmt.Errorf("guard: artifact release %q, chosen %q: %w", a.Release, c.Release, ErrFilenameMismatch)
	}
	if !isServerURL(otaURL, a) {
		return fmt.Errorf("guard: OtaUrl is not the firmware server URL of %s/%s: %w", a.Release, a.Filename, ErrFilenameMismatch)
	}
	// Rule 5: ESP8266 gets .bin.gz, ESP32 gets .bin.
	if fi.gz != (a.Family == model.ChipESP8266) {
		return fmt.Errorf("guard: %q has the wrong container for %s: %w", a.Filename, a.Family, ErrFilenameMismatch)
	}
	// Rule 6: solo1 builds only for single-core hardware and vice versa.
	if fi.solo1 != id.SingleCore {
		return fmt.Errorf("guard: %q on single core = %v: %w", a.Filename, id.SingleCore, ErrFamilyMismatch)
	}
	// Rule 8: the URL fits Tasmota's settings slot.
	if len(otaURL) > maxOtaURLLen {
		return fmt.Errorf("guard: OtaUrl has %d characters, max %d: %w", len(otaURL), maxOtaURLLen, ErrFilenameMismatch)
	}
	// Rule 7: exactly the selected build, or its esp8266 minimal stage.
	sel, err := Select(id, c)
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	if a.Filename != sel.Filename && a.Filename != sel.Minimal {
		return fmt.Errorf("guard: artifact %q, selected %q: %w", a.Filename, sel.Filename, ErrVariantChange)
	}
	return nil
}

// isServerURL checks that u is a plain http URL whose path ends in
// /fw/<release>/<filename> (§8.3), with no query, fragment, whitespace or
// control characters that could make the device fetch something else.
func isServerURL(u string, a model.Artifact) bool {
	rest, ok := strings.CutPrefix(u, "http://")
	if !ok || !isNumericVersion(a.Release) {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if b := rest[i]; b <= ' ' || b == 0x7f || b == '?' || b == '#' || b == '\\' {
			return false
		}
	}
	host, p, ok := strings.Cut(rest, "/")
	return ok && host != "" && strings.HasSuffix("/"+p, "/fw/"+a.Release+"/"+a.Filename)
}
