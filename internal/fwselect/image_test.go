package fwselect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// TestInspectImageFixtures runs over every captured upstream header. The
// firmware cache rejects files whose header family disagrees with their
// name, so a wrong mapping here either blocks good files or lets a binary
// for another CPU through.
func TestInspectImageFixtures(t *testing.T) {
	want := map[string]ImageInfo{
		"tasmota.bin.gz.hdr":         {model.ChipESP8266, -1},
		"tasmota-lite.bin.gz.hdr":    {model.ChipESP8266, -1},
		"tasmota-minimal.bin.gz.hdr": {model.ChipESP8266, -1},
		"tasmota-sensors.bin.gz.hdr": {model.ChipESP8266, -1},
		"tasmota32.bin.hdr":          {model.ChipESP32, 0x00},
		"tasmota32solo1.bin.hdr":     {model.ChipESP32, 0x00},
		"tasmota32-safeboot.bin.hdr": {model.ChipESP32, 0x00},
		"tasmota32s2.bin.hdr":        {model.ChipESP32S2, 0x02},
		"tasmota32s2cdc.bin.hdr":     {model.ChipESP32S2, 0x02},
		"tasmota32c3.bin.hdr":        {model.ChipESP32C3, 0x05},
		"tasmota32s3.bin.hdr":        {model.ChipESP32S3, 0x09},
		"tasmota32c2.bin.hdr":        {model.ChipESP32C2, 0x0C},
		"tasmota32c6.bin.hdr":        {model.ChipESP32C6, 0x0D},
		"tasmota32p4.bin.hdr":        {model.ChipESP32P4, 0x12},
		"tasmota32c61.bin.hdr":       {model.ChipESP32C61, 0x14},
		"tasmota32c5.bin.hdr":        {model.ChipESP32C5, 0x17},
	}
	files, err := filepath.Glob(filepath.Join(fixturesDir, "firmware-headers", "*.hdr"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(want) {
		t.Errorf("found %d header fixtures, table has %d rows: every fixture needs a row", len(files), len(want))
	}
	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			w, ok := want[name]
			if !ok {
				t.Fatalf("no expectation for %s", name)
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			got, err := InspectImage(b)
			wantErr(t, err, nil)
			if got != w {
				t.Errorf("InspectImage = %+v, want %+v", got, w)
			}
			// Exactly 24 bytes (the documented minimum) must be enough.
			got24, err := InspectImage(b[:24])
			wantErr(t, err, nil)
			if got24 != w {
				t.Errorf("InspectImage(24 bytes) = %+v, want %+v", got24, w)
			}
			// The header family must agree with the file-name family, the
			// same cross-check Guard does.
			fi, ok := parseFilename(strings.TrimSuffix(name, ".hdr"))
			if !ok || fi.family != w.Family {
				t.Errorf("file-name family %+v (ok=%v) disagrees with header %q", fi, ok, w.Family)
			}
		})
	}
}

func esp32Header(chipID uint16) []byte {
	b := make([]byte, 24)
	b[0] = 0xE9
	b[1] = 0x05
	b[8] = 0xEE
	b[12] = byte(chipID)
	b[13] = byte(chipID >> 8)
	return b
}

// TestInspectImageBad: anything that is not a recognisable ESP image must be
// rejected, never mapped to a default family.
func TestInspectImageBad(t *testing.T) {
	good := readFixture(t, "firmware-headers/tasmota32c3.bin.hdr")
	gz := []byte{0x1f, 0x8b, 0x08, 0x00}
	gz = append(gz, make([]byte, 28)...)
	tests := []struct {
		name string
		in   []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"one byte magic", []byte{0xE9}},
		{"truncated 23 bytes", good[:23]},
		{"truncated 13 bytes", good[:13]},
		{"gzip not decompressed", gz},
		{"zeros", make([]byte, 32)},
		{"html", []byte("<html><body>404 Not Found</body></html>")},
		{"unknown chip id 0x0001", esp32Header(0x0001)},
		{"unknown chip id 0x0004 (H2 family)", esp32Header(0x0010)},
		{"unknown chip id 0xFFFF", esp32Header(0xFFFF)},
		{"chip id high byte set", esp32Header(0x0105)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InspectImage(tt.in)
			wantErr(t, err, ErrBadImage)
			if got.Family != model.ChipUnknown {
				t.Errorf("family on error = %q, want unknown", got.Family)
			}
		})
	}
}

// TestInspectImageChipIDs pins the full §7.4 mapping, including the
// little-endian read of chip_id (a big-endian read would turn c3 0x0005
// into 0x0500 and reject every C3 image).
func TestInspectImageChipIDs(t *testing.T) {
	tests := []struct {
		id   uint16
		want model.ChipFamily
	}{
		{0x0000, model.ChipESP32},
		{0x0002, model.ChipESP32S2},
		{0x0005, model.ChipESP32C3},
		{0x0009, model.ChipESP32S3},
		{0x000C, model.ChipESP32C2},
		{0x000D, model.ChipESP32C6},
		{0x0012, model.ChipESP32P4},
		{0x0014, model.ChipESP32C61},
		{0x0017, model.ChipESP32C5},
	}
	for _, tt := range tests {
		got, err := InspectImage(esp32Header(tt.id))
		wantErr(t, err, nil)
		if got.Family != tt.want || got.ChipID != int(tt.id) {
			t.Errorf("chip_id %#x: got %+v, want %q", tt.id, got, tt.want)
		}
	}
	// ESP8266: byte 8 is not 0xEE, so there is no extended header.
	h := esp32Header(0x0005)
	h[8] = 0xED
	got, err := InspectImage(h)
	wantErr(t, err, nil)
	if got != (ImageInfo{model.ChipESP8266, -1}) {
		t.Errorf("no extended header: got %+v, want esp8266", got)
	}
}

func FuzzInspectImage(f *testing.F) {
	files, err := filepath.Glob(filepath.Join(fixturesDir, "firmware-headers", "*.hdr"))
	if err != nil {
		f.Fatal(err)
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte{})
	f.Add([]byte{0xE9})
	f.Fuzz(func(t *testing.T, b []byte) {
		info, err := InspectImage(b)
		if err != nil {
			if info.Family != model.ChipUnknown {
				t.Fatalf("family %q returned together with error %v", info.Family, err)
			}
			return
		}
		if len(b) < 24 || b[0] != 0xE9 {
			t.Fatalf("accepted a non-image: % x", b)
		}
		if info.Family == model.ChipUnknown {
			t.Fatal("success with unknown family")
		}
		if info.Family.IsESP32() != (b[8] == 0xEE) {
			t.Fatalf("family %q disagrees with extended header byte %#x", info.Family, b[8])
		}
	})
}
