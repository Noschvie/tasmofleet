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

	semver, tag, err := splitVersion(s2.StatusFWR.Version)
	if err != nil {
		return model.Identity{}, err
	}
	hw := s2.StatusFWR.Hardware
	fam, single := hardwareInfo(hw)
	variant, ok := variantFromTag(tag, fam, single)
	if !ok {
		return model.Identity{}, fmt.Errorf("fwselect: image %q does not run on hardware %q: %w", tag, hw, ErrFamilyMismatch)
	}
	id := model.Identity{
		Family:     fam,
		Hardware:   hw,
		SingleCore: single,
		Version:    semver,
		Image:      tag,
		Variant:    variant,
		Language:   languageFromRoot(in.Root),
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

// splitVersion splits "16.0.0(release-tasmota)" into "16.0.0" and
// "release-tasmota".
func splitVersion(v string) (string, string, error) {
	semver, rest, ok := strings.Cut(v, "(")
	if !ok || !strings.HasSuffix(rest, ")") || !isNumericVersion(semver) {
		return "", "", fmt.Errorf("%w: %q", errBadVersion, v)
	}
	tag := strings.TrimSuffix(rest, ")")
	if tag == "" {
		return "", "", fmt.Errorf("%w: %q", errBadVersion, v)
	}
	return semver, tag, nil
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

// variantFromTag applies the §6.2 table to a raw image tag
// ("<sha-or-release>-<image>"). ok is false when the image cannot run on
// the detected hardware. With unknown hardware there is nothing to check
// against; the identity is unusable for updates anyway.
func variantFromTag(tag string, fam model.ChipFamily, single bool) (string, bool) {
	_, image, found := strings.Cut(tag, "-")
	if !found {
		return variantUnknown, true
	}
	known := fam != model.ChipUnknown
	switch {
	case esp8266Variants[image] || image == variantMinimal:
		return image, !known || fam == model.ChipESP8266
	case image == variantSafeboot:
		return variantSafeboot, !known || fam.IsESP32()
	case strings.HasPrefix(image, "tasmota32-"):
		v := strings.TrimPrefix(image, "tasmota32-")
		if !esp32Variants[v] || v == variantTasmota {
			return variantUnknown, true
		}
		return v, !known || (fam == model.ChipESP32 && !single)
	case strings.HasPrefix(image, "tasmota32"):
		suffix := strings.TrimPrefix(image, "tasmota32")
		buildFam, ok := chipBuilds[suffix]
		if !ok {
			return variantUnknown, true
		}
		return variantTasmota, !known || (buildFam == fam && (suffix == "solo1") == single)
	default:
		return variantUnknown, true
	}
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
