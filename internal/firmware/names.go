package firmware

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Additional errors of this package.
var (
	// ErrBadName is returned for release or file names that are not a
	// Tasmota OTA image name or could escape the cache directory.
	ErrBadName = errors.New("firmware: invalid release or file name")
	// ErrNotInIndex is returned when no configured source lists the file.
	ErrNotInIndex = errors.New("firmware: release or file not offered by any configured source")
	// ErrChecksumMismatch is returned when two sources deliver different bytes.
	ErrChecksumMismatch = errors.New("firmware: SHA-256 differs between sources")
)

// nameInfo is what an upstream file name says about the image.
type nameInfo struct {
	Family   model.ChipFamily
	Variant  string
	Language string
	Safeboot bool
}

// esp32Chips maps the chip token after "tasmota32" to its family. Longest
// tokens first so that "c61" is not read as "c6" plus "1". "solo1" is the
// single-core classic ESP32 and carries the same chip_id as the dual-core
// build (spec 7.4), "s2cdc" and "p4r3" are build flavours of s2 and p4.
var esp32Chips = []struct {
	token  string
	family model.ChipFamily
}{
	{"s2cdc", model.ChipESP32S2}, {"solo1", model.ChipESP32}, {"p4r3", model.ChipESP32P4},
	{"c61", model.ChipESP32C61}, {"s2", model.ChipESP32S2}, {"s3", model.ChipESP32S3},
	{"c2", model.ChipESP32C2}, {"c3", model.ChipESP32C3}, {"c5", model.ChipESP32C5},
	{"c6", model.ChipESP32C6}, {"p4", model.ChipESP32P4},
}

func isNameChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_'
}

// validPathElement reports whether s is safe to use as one directory or file
// name: a non-empty run of letters, digits, dot, dash and underscore that
// does not start with a dot. Anything else (slashes, backslashes, NUL, %,
// spaces, "..") is refused, so no URL form can address another path.
func validPathElement(s string) bool {
	if s == "" || s[0] == '.' || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !isNameChar(r) {
			return false
		}
	}
	return true
}

func validRelease(s string) bool { return validPathElement(s) }

func isLanguage(s string) bool {
	if len(s) != 2 {
		return false
	}
	return s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// parseName classifies an upstream OTA file name (spec 7.1). Factory images,
// maps and everything else are refused.
func parseName(file string) (nameInfo, error) {
	bad := func() (nameInfo, error) { return nameInfo{}, fmt.Errorf("%w: %q", ErrBadName, file) }
	if !validPathElement(file) {
		return bad()
	}
	var stem string
	var info nameInfo
	switch {
	case strings.HasPrefix(file, "tasmota32"):
		if !strings.HasSuffix(file, ".bin") {
			return bad()
		}
		stem = strings.TrimSuffix(strings.TrimPrefix(file, "tasmota32"), ".bin")
		info.Family = model.ChipESP32
		for _, c := range esp32Chips {
			if strings.HasPrefix(stem, c.token) && (len(stem) == len(c.token) || stem[len(c.token)] == '-') {
				info.Family = c.family
				stem = stem[len(c.token):]
				break
			}
		}
	case strings.HasPrefix(file, "tasmota"):
		if !strings.HasSuffix(file, ".bin.gz") {
			return bad()
		}
		stem = strings.TrimSuffix(strings.TrimPrefix(file, "tasmota"), ".bin.gz")
		info.Family = model.ChipESP8266
	default:
		return bad()
	}
	info.Variant = "tasmota"
	switch {
	case stem == "":
	case stem[0] != '-' || len(stem) == 1:
		return bad() // e.g. tasmota32xyz.bin
	default:
		tag := stem[1:]
		switch {
		case strings.ContainsAny(tag, "."):
			return bad() // e.g. tasmota32-DE.factory.bin: serial flashing image, never an OTA target
		case tag == "safeboot" && info.Family.IsESP32():
			info.Safeboot = true
		case isLanguage(tag):
			info.Language = tag
		default:
			info.Variant = tag
		}
	}
	return info, nil
}

// compareVersions compares dotted versions numerically per field. A suffix
// such as "-rc1" is ignored; missing fields count as 0.
func compareVersions(a, b string) int {
	fa, fb := versionFields(a), versionFields(b)
	for i := 0; i < len(fa) || i < len(fb); i++ {
		var x, y int
		if i < len(fa) {
			x = fa[i]
		}
		if i < len(fb) {
			y = fb[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

func versionFields(v string) []int {
	var out []int
	for _, f := range strings.Split(strings.TrimPrefix(v, "v"), ".") {
		end := 0
		for end < len(f) && f[end] >= '0' && f[end] <= '9' {
			end++
		}
		n, _ := strconv.Atoi(f[:end]) // empty or overflowing field counts as 0
		out = append(out, n)
	}
	return out
}
