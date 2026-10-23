package fwselect

import (
	"encoding/binary"
	"fmt"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

const (
	imageMagic       = 0xE9
	extHeaderMarker  = 0xEE // byte 8 of ESP32 app images (WP pin "disabled")
	imageHeaderBytes = 24   // common header (8) + extended header (16)
)

// chipIDs maps the ESP32 extended header chip_id (§7.4) to the family.
var chipIDs = map[uint16]model.ChipFamily{
	0x0000: model.ChipESP32,
	0x0002: model.ChipESP32S2,
	0x0005: model.ChipESP32C3,
	0x0009: model.ChipESP32S3,
	0x000C: model.ChipESP32C2,
	0x000D: model.ChipESP32C6,
	0x0012: model.ChipESP32P4,
	0x0014: model.ChipESP32C61,
	0x0017: model.ChipESP32C5,
}

func inspectImage(h []byte) (ImageInfo, error) {
	if len(h) < imageHeaderBytes {
		return ImageInfo{}, fmt.Errorf("%w: header has %d bytes, need %d", ErrBadImage, len(h), imageHeaderBytes)
	}
	if h[0] != imageMagic {
		return ImageInfo{}, fmt.Errorf("%w: magic %#02x", ErrBadImage, h[0])
	}
	if h[8] != extHeaderMarker {
		return ImageInfo{Family: model.ChipESP8266, ChipID: -1}, nil
	}
	id := binary.LittleEndian.Uint16(h[12:14])
	fam, ok := chipIDs[id]
	if !ok {
		return ImageInfo{}, fmt.Errorf("%w: unknown chip_id %#04x", ErrBadImage, id)
	}
	return ImageInfo{Family: fam, ChipID: int(id)}, nil
}
