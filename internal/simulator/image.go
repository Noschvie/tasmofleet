package simulator

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// versionMarker is embedded in synthetic firmware images by NewImage. A
// simulated device reads it after the download to learn which version it
// "flashed", standing in for the version string a real binary carries.
const versionMarker = "TASMOFLEET-SIM-VERSION="

// ESP32 chip ids in the extended image header (bytes 12-13, little endian).
var chipIDs = map[uint16]string{
	0x0000: "esp32", 0x0002: "esp32s2", 0x0005: "esp32c3", 0x0009: "esp32s3",
	0x000C: "esp32c2", 0x000D: "esp32c6", 0x0012: "esp32p4", 0x0014: "esp32c61",
	0x0017: "esp32c5",
}

// imageFamily is the tiny header parser a simulated device uses to refuse
// wrong-family images like the real bootloader check ("Verify Bin Header
// Failed"). ESP32 images carry 0xEE at byte 8 and a chip id at bytes 12-13;
// ESP8266 images have no extended header.
func imageFamily(b []byte) (string, error) {
	if len(b) < 16 || b[0] != 0xE9 {
		return "", errors.New("bad magic")
	}
	if b[8] == 0xEE {
		id := uint16(b[12]) | uint16(b[13])<<8
		fam, ok := chipIDs[id]
		if !ok {
			return "", fmt.Errorf("unknown chip id 0x%04x", id)
		}
		return fam, nil
	}
	return "esp8266", nil
}

// NewImage builds a synthetic firmware file for tests. name is the file name
// as published upstream (for example "tasmota32c3.bin" or
// "tasmota-minimal.bin.gz"); its first 32 bytes are the real header captured
// in <fixturesDir>/firmware-headers, followed by deterministic filler and the
// version marker. Names ending in .gz are gzip compressed (the header files
// of .gz images describe the decompressed bytes, as a device sees them).
func NewImage(fixturesDir, name, version string) ([]byte, error) {
	hdr, err := os.ReadFile(filepath.Join(fixturesDir, "firmware-headers", path.Base(name)+".hdr")) //nolint:gosec // path.Base keeps it inside the fixtures dir
	if errors.Is(err, os.ErrNotExist) && strings.HasSuffix(name, ".bin.gz") {
		hdr, err = os.ReadFile(filepath.Join(fixturesDir, "firmware-headers", "tasmota.bin.gz.hdr")) //nolint:gosec // constant name
	}
	if err != nil {
		return nil, fmt.Errorf("simulator: header for %s: %w", name, err)
	}
	raw := append([]byte(nil), hdr...)
	for i := 0; i < 2048; i++ {
		raw = append(raw, byte((i*31+len(name))&0xff))
	}
	raw = append(raw, []byte(versionMarker+version+"\x00")...)
	if !strings.HasSuffix(name, ".gz") {
		return raw, nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("simulator: gzip image: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("simulator: gzip image: %w", err)
	}
	return buf.Bytes(), nil
}

// maxImage bounds what a simulated device reads from a download.
const maxImage = 16 << 20

// unpack returns the image as the device would flash it: gzip is expanded on
// the fly by a real ESP8266, so the header check sees the decompressed bytes.
func unpack(body []byte) ([]byte, error) {
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(zr, maxImage))
	}
	return body, nil
}

// findVersion extracts the version marker written by NewImage.
func findVersion(img []byte) (string, bool) {
	i := bytes.Index(img, []byte(versionMarker))
	if i < 0 {
		return "", false
	}
	rest := img[i+len(versionMarker):]
	if j := bytes.IndexByte(rest, 0); j >= 0 {
		rest = rest[:j]
	}
	return string(rest), len(rest) > 0
}

var minimalRewrite = regexp.MustCompile(`^tasmota(-[a-z0-9]+)?(\.bin(\.gz)?)?$`)

// imageKind derives the image name a flash from this URL produces, purely
// from the file name like the real flow: "minimal", "safeboot", the ESP32
// build name ("tasmota32", "tasmota32c3", ...), "tasmota" or an ESP8266
// variant such as "lite".
func imageKind(urlPath string, esp32 bool) string {
	base := strings.ToLower(path.Base(urlPath))
	switch {
	case strings.Contains(base, "safeboot"):
		return "safeboot"
	case strings.Contains(base, "minimal"):
		if esp32 {
			return "safeboot"
		}
		return "minimal"
	case esp32:
		// The ESP32 image tag is the build name: tasmota32, tasmota32c3,
		// tasmota32solo1, tasmota32-ir, ...
		if b := strings.TrimSuffix(base, ".bin"); strings.HasPrefix(b, "tasmota32") {
			return b
		}
		return "tasmota32"
	}
	if m := regexp.MustCompile(`^tasmota-([a-z0-9]+)`).FindStringSubmatch(base); m != nil {
		return m[1]
	}
	return "tasmota"
}

// minimalURL is Tasmota's automatic fallback rewrite: tasmota[-xx][.bin[.gz]]
// becomes tasmota-minimal[.bin[.gz]] in the last path element.
func minimalURL(u string) (string, bool) {
	i := strings.LastIndex(u, "/")
	base := u[i+1:]
	if m := minimalRewrite.FindStringSubmatch(base); m != nil {
		return u[:i+1] + "tasmota-minimal" + m[2], true
	}
	return "", false
}
