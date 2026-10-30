package fwselect

import (
	"fmt"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// UpLayoutHint is what an HTTP read of the device's /up page showed about its flash layout (#30).
// It can only sharpen the reason of a refusal; it never lets an image pass that the bound below
// does not prove (an unknown layout is the normal case over MQTT).
type UpLayoutHint string

// Layout hints.
const (
	UpLayoutUnknown  UpLayoutHint = "unknown"
	UpLayoutDirect   UpLayoutHint = "direct"   // two app partitions: the image goes into the other one
	UpLayoutSafeboot UpLayoutHint = "safeboot" // one app partition next to safeboot: the image is staged behind the running firmware
)

// MQTT FileUpload Type 1 constants (Tasmota v16.0.0).
const (
	// mqttUploadSector is SPI_FLASH_SEC_SIZE: MqttFileUploadValidate rounds the size up to it.
	mqttUploadSector = 4096
	// mqttUploadMarginKB is the part of Status 4 "Free" that head_room can never use, see the
	// derivation on MQTTUploadFits.
	mqttUploadMarginKB = 192
)

// MQTTUploadFits decides, before anything is sent, whether Tasmota will accept an image of
// imageSize bytes as FileUpload Type 1 (xdrv_02_1_mqtt_file.ino, MqttFileUploadValidate):
//
//	head_room    = (FlashWriteMaxSector() - FlashWriteStartSector()) * 4096
//	rounded_size = round_up_4096(file_size)
//	refused ("Not enough space", error 2) when rounded_size > head_room
//
// The device is never asked to try. The rule, valid for both ESP32 layouts without knowing which
// one the device has:
//
//	round_up_4k(imageSize) <= FreeKB*1024 - 192 KiB      (FreeKB = Status 4 "Free")
//
// Why 192 KiB (support_esp32.ino, v16.0.0):
//   - FlashWriteStartSector = (EspFlashBaseAddress + 128 KiB) rounded down to 64 KiB,
//     FlashWriteMaxSector = EspFlashBaseEndAddress rounded down to 64 KiB.
//   - Two app partitions (direct): EspFlashBaseAddress is the start of the OTHER partition and
//     EspFlashBaseEndAddress its end, and ESP_getFreeSketchSpace() (Status 4 Free) is that
//     partition's size. So head_room >= Free - 128 KiB - 63 KiB (rounding down the end), and
//     the size of the running sketch plays no part.
//   - One app partition (safeboot layout): EspFlashBaseAddress is running->address + sketch size,
//     the end is the partition's end, and Free is partition size - sketch size. Again
//     head_room >= Free - 128 KiB - 63 KiB.
//   - Status 4 prints Free in whole KB (rounded down), another < 1 KiB. 128 + 63 + 1 = 192 KiB.
//
// Refused outright, each with its own reason: not an ESP32 family (the only other family is ESP8266, which
// has no second stage: minimal firmware has no FileUpload), identity not Known, variant minimal or safeboot (neither has
// FileUpload), Free unknown, image size unknown. needKB is the rounded image size and roomKB what
// the bound allows (0 when Free is below the margin or unknown).
func MQTTUploadFits(id model.Identity, layout UpLayoutHint, imageSize int64) (ok bool, needKB, roomKB int, reason string) {
	switch {
	case !id.Known():
		return false, 0, 0, "the device's identity is not known, so an upload over MQTT is not attempted"
	case id.Family == model.ChipESP8266 || !id.Family.IsESP32():
		return false, 0, 0, "an ESP8266 cannot take an upload over MQTT: its minimal firmware has no FileUpload"
	case id.Variant == variantMinimal || id.Variant == variantSafeboot:
		return false, 0, 0, fmt.Sprintf("the device runs %s firmware, which has no FileUpload", id.Variant)
	case id.FreeKB <= 0:
		return false, 0, 0, "the device's free flash (Status 4) is unknown"
	case imageSize <= 0:
		return false, 0, 0, "the image size is unknown"
	}
	need := (imageSize + mqttUploadSector - 1) / mqttUploadSector * mqttUploadSector
	needKB = int((need + 1023) / 1024)
	roomKB = max(id.FreeKB-mqttUploadMarginKB, 0)
	if need > int64(roomKB)*1024 {
		why := "the image does not fit"
		if layout == UpLayoutSafeboot {
			why = "safeboot layout: the image is staged behind the running firmware and does not fit"
		}
		return false, needKB, roomKB, fmt.Sprintf("%s for an upload over MQTT (needs %d KB, %d KB of the %d KB free are usable)", why, needKB, roomKB, id.FreeKB)
	}
	return true, needKB, roomKB, fmt.Sprintf("the image fits for an upload over MQTT (needs %d KB, %d KB usable)", needKB, roomKB)
}
